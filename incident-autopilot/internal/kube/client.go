// Package kube reads the configured Deployment target, tracks rollout
// readiness, and validates pod ownership through the ReplicaSet chain before
// any remediation action is permitted.
package kube

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/config"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/model"
)

// TargetState is the current Deployment target and its owned Pods.
type TargetState struct {
	DeploymentUID      types.UID
	Generation         int64
	ObservedGeneration int64
	DesiredReplicas    int32
	AvailableReplicas  int32
	Pods               []corev1.Pod
}

// Client is a read-only Kubernetes client scoped to one Deployment.
type Client struct {
	cfg            config.Config
	clientset      kubernetes.Interface
	namespace      string
	deployment     string
	expectedUID    types.UID
	pollInterval   time.Duration
	rolloutTimeout time.Duration
}

type Option func(*Client)

// WithExpectedDeploymentUID pins the Deployment UID. Target rejects a
// deployment whose UID does not match, which guards against accidental
// actions after a delete/recreate of the same name.
func WithExpectedDeploymentUID(uid types.UID) Option {
	return func(c *Client) { c.expectedUID = uid }
}

// WithPollInterval overrides the rollout polling interval (tests only).
func WithPollInterval(d time.Duration) Option {
	return func(c *Client) { c.pollInterval = d }
}

// WithRolloutTimeout overrides the default rollout wait timeout (tests only).
func WithRolloutTimeout(d time.Duration) Option {
	return func(c *Client) { c.rolloutTimeout = d }
}

func New(cfg config.Config, clientset kubernetes.Interface, opts ...Option) *Client {
	c := &Client{
		cfg:            cfg,
		clientset:      clientset,
		namespace:      cfg.Target.Namespace,
		deployment:     cfg.Target.Deployment,
		pollInterval:   500 * time.Millisecond,
		rolloutTimeout: 5 * time.Minute,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Target returns the Deployment UID, generation, replica counts, and Pods
// owned by the configured Deployment through a ReplicaSet owner reference.
func (c *Client) Target(ctx context.Context) (TargetState, error) {
	deploy, err := c.clientset.AppsV1().Deployments(c.namespace).Get(ctx, c.deployment, metav1.GetOptions{})
	if err != nil {
		return TargetState{}, fmt.Errorf("get deployment %s/%s: %w", c.namespace, c.deployment, err)
	}
	if c.expectedUID != "" && deploy.UID != c.expectedUID {
		return TargetState{}, fmt.Errorf("deployment UID mismatch: expected %s, got %s", c.expectedUID, deploy.UID)
	}

	rsList, err := c.clientset.AppsV1().ReplicaSets(c.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return TargetState{}, fmt.Errorf("list replicasets: %w", err)
	}
	rsByUID := replicaSetsOwnedByDeployment(rsList.Items, deploy.UID)

	podList, err := c.clientset.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return TargetState{}, fmt.Errorf("list pods: %w", err)
	}

	var owned []corev1.Pod
	for _, pod := range podList.Items {
		if podOwnedByDeployment(&pod, deploy.UID, rsByUID) {
			owned = append(owned, pod)
		}
	}

	desired := int32(1)
	if deploy.Spec.Replicas != nil {
		desired = *deploy.Spec.Replicas
	}

	return TargetState{
		DeploymentUID:      deploy.UID,
		Generation:         deploy.Generation,
		ObservedGeneration: deploy.Status.ObservedGeneration,
		DesiredReplicas:    desired,
		AvailableReplicas:  deploy.Status.AvailableReplicas,
		Pods:               owned,
	}, nil
}

// Replicas returns the Deployment's desired and available replica counts.
func (c *Client) Replicas(ctx context.Context) (model.ReplicaStatus, error) {
	state, err := c.Target(ctx)
	if err != nil {
		return model.ReplicaStatus{}, err
	}
	return model.ReplicaStatus{
		Current:   state.DesiredReplicas,
		Available: state.AvailableReplicas,
	}, nil
}

// WaitForRollout blocks until the Deployment has observed the given generation
// and all desired replicas are available, or the timeout elapses.
func (c *Client) WaitForRollout(ctx context.Context, generation int64, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = c.rolloutTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	for {
		deploy, err := c.clientset.AppsV1().Deployments(c.namespace).Get(ctx, c.deployment, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("deployment %s/%s not found", c.namespace, c.deployment)
			}
			return fmt.Errorf("get deployment: %w", err)
		}

		desired := int32(1)
		if deploy.Spec.Replicas != nil {
			desired = *deploy.Spec.Replicas
		}

		if deploy.Status.ObservedGeneration >= generation &&
			deploy.Status.AvailableReplicas >= desired {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("rollout timed out waiting for generation %d: %w", generation, ctx.Err())
		case <-ticker.C:
		}
	}
}

// ValidatePodOwnership returns nil only when the pod is owned by the configured
// Deployment through a ReplicaSet owner reference.
func (c *Client) ValidatePodOwnership(ctx context.Context, podName string, podUID types.UID) error {
	state, err := c.Target(ctx)
	if err != nil {
		return err
	}
	for _, pod := range state.Pods {
		if pod.Name == podName && pod.UID == podUID {
			return nil
		}
	}
	return fmt.Errorf("pod %s/%s is not owned by deployment %s", c.namespace, podName, c.deployment)
}

func replicaSetsOwnedByDeployment(items []appsv1.ReplicaSet, deployUID types.UID) map[types.UID]*appsv1.ReplicaSet {
	out := make(map[types.UID]*appsv1.ReplicaSet)
	for i := range items {
		rs := &items[i]
		for _, owner := range rs.OwnerReferences {
			if owner.Kind == "Deployment" && owner.UID == deployUID {
				out[rs.UID] = rs
				break
			}
		}
	}
	return out
}

func podOwnedByDeployment(pod *corev1.Pod, deployUID types.UID, rsByUID map[types.UID]*appsv1.ReplicaSet) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind != "ReplicaSet" {
			continue
		}
		rs, ok := rsByUID[owner.UID]
		if !ok {
			continue
		}
		for _, rsOwner := range rs.OwnerReferences {
			if rsOwner.Kind == "Deployment" && rsOwner.UID == deployUID {
				return true
			}
		}
	}
	return false
}
