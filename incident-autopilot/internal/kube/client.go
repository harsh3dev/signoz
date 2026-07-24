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
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/config"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/model"
)

const defaultReadinessGate = "autopilot.signoz.io/healthy"

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
	service        string
	expectedUID    types.UID
	pollInterval   time.Duration
	rolloutTimeout time.Duration
	drainTimeout   time.Duration
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

// WithDrainTimeout overrides the endpoint drain wait timeout (tests only).
func WithDrainTimeout(d time.Duration) Option {
	return func(c *Client) { c.drainTimeout = d }
}

func New(cfg config.Config, clientset kubernetes.Interface, opts ...Option) *Client {
	service := cfg.Target.Service
	if service == "" {
		service = cfg.Target.Deployment
	}
	c := &Client{
		cfg:            cfg,
		clientset:      clientset,
		namespace:      cfg.Target.Namespace,
		deployment:     cfg.Target.Deployment,
		service:        service,
		pollInterval:   500 * time.Millisecond,
		rolloutTimeout: 5 * time.Minute,
		drainTimeout:   2 * time.Minute,
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
// and all desired replicas are available, or the timeout elapses. When
// excludeOutlierPod is non-empty, the custom readiness gate is initialized on
// replacement pods during each poll so gated rollouts can complete.
func (c *Client) WaitForRollout(ctx context.Context, generation int64, timeout time.Duration) error {
	return c.waitForRollout(ctx, generation, timeout, "")
}

// WaitForRolloutWithOutlier is like WaitForRollout but keeps replacement pods'
// custom readiness gates initialized while a quarantined pod is excluded.
func (c *Client) WaitForRolloutWithOutlier(ctx context.Context, generation int64, timeout time.Duration, excludeOutlierPod string) error {
	return c.waitForRollout(ctx, generation, timeout, excludeOutlierPod)
}

func (c *Client) waitForRollout(ctx context.Context, generation int64, timeout time.Duration, excludeOutlierPod string) error {
	if timeout <= 0 {
		timeout = c.rolloutTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	lastGateSync := time.Time{}

	for {
		if excludeOutlierPod != "" && time.Since(lastGateSync) >= 5*time.Second {
			if err := c.SyncReadinessGates(ctx, excludeOutlierPod); err != nil {
				return fmt.Errorf("sync readiness gates during rollout: %w", err)
			}
			lastGateSync = time.Now()
		}

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

func (c *Client) readinessGateType() corev1.PodConditionType {
	if c.cfg.Target.ReadinessGate != "" {
		return corev1.PodConditionType(c.cfg.Target.ReadinessGate)
	}
	return corev1.PodConditionType(defaultReadinessGate)
}

// SetAutopilotReady patches only the custom readiness gate condition on an
// owned pod, preserving every other status field.
func (c *Client) SetAutopilotReady(ctx context.Context, podName string, podUID types.UID, ready bool, reason, message string) error {
	if err := c.ValidatePodOwnership(ctx, podName, podUID); err != nil {
		return err
	}

	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionFalse
	}
	condType := c.readinessGateType()
	cond := corev1.PodCondition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}

	for attempt := 0; attempt < 5; attempt++ {
		pod, err := c.clientset.CoreV1().Pods(c.namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get pod %s/%s: %w", c.namespace, podName, err)
		}
		if pod.UID != podUID {
			return fmt.Errorf("pod %s/%s UID mismatch: expected %s, got %s", c.namespace, podName, podUID, pod.UID)
		}

		pod.Status.Conditions = upsertPodCondition(pod.Status.Conditions, cond)
		if _, err := c.clientset.CoreV1().Pods(c.namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return fmt.Errorf("patch pod status %s/%s: %w", c.namespace, podName, err)
		}
		return nil
	}
	return fmt.Errorf("patch pod status %s/%s: conflict after retries", c.namespace, podName)
}

// SyncReadinessGates initializes the custom readiness gate to healthy for every
// owned pod that is container-ready and not the active outlier target.
func (c *Client) SyncReadinessGates(ctx context.Context, activeOutlierPod string) error {
	state, err := c.Target(ctx)
	if err != nil {
		return err
	}
	gate := c.readinessGateType()
	for _, pod := range state.Pods {
		if activeOutlierPod != "" && pod.Name == activeOutlierPod {
			continue
		}
		if !podContainersReady(&pod) {
			continue
		}
		if autopilotConditionStatus(&pod, gate) == corev1.ConditionTrue {
			continue
		}
		if err := c.SetAutopilotReady(ctx, pod.Name, pod.UID, true, "AutopilotHealthy",
			"Pod passed SigNoz Incident Autopilot readiness gate"); err != nil {
			return err
		}
	}
	return nil
}

// WaitUntilNotRouted blocks until the pod UID no longer appears as ready or
// serving in any EndpointSlice selected by the target Service.
func (c *Client) WaitUntilNotRouted(ctx context.Context, podUID types.UID, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = c.drainTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	for {
		routed, err := c.isPodRouted(ctx, podUID)
		if err != nil {
			return err
		}
		if !routed {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for pod %s to drain from endpoints: %w", podUID, ctx.Err())
		case <-ticker.C:
		}
	}
}

// WaitForReplacementReady blocks until a newly created owned pod other than
// excludeUID becomes fully ready. knownUIDs must contain every pod UID present
// before replacement capacity was requested.
func (c *Client) WaitForReplacementReady(ctx context.Context, excludeUID types.UID, knownUIDs map[types.UID]struct{}, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = c.rolloutTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	gate := c.readinessGateType()
	lastGateSync := time.Time{}

	for {
		state, err := c.Target(ctx)
		if err != nil {
			return err
		}
		excludePodName := ""
		for i := range state.Pods {
			if state.Pods[i].UID == excludeUID {
				excludePodName = state.Pods[i].Name
				break
			}
		}

		if time.Since(lastGateSync) >= 5*time.Second {
			if err := c.SyncReadinessGates(ctx, excludePodName); err != nil {
				return fmt.Errorf("sync readiness gates while waiting for replacement: %w", err)
			}
			lastGateSync = time.Now()
			state, err = c.Target(ctx)
			if err != nil {
				return err
			}
		}

		for i := range state.Pods {
			pod := state.Pods[i]
			if pod.UID == excludeUID {
				continue
			}
			if _, known := knownUIDs[pod.UID]; known {
				continue
			}
			if podFullyReady(&pod, gate) {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for replacement pod (known=%d owned=%d): %w",
				len(knownUIDs), len(state.Pods), ctx.Err())
		case <-ticker.C:
		}
	}
}

// DeleteOwnedPod deletes a pod only after ownership is validated and uses a
// UID precondition so a recreated pod with the same name cannot be removed.
func (c *Client) DeleteOwnedPod(ctx context.Context, podName string, podUID types.UID) error {
	if err := c.ValidatePodOwnership(ctx, podName, podUID); err != nil {
		return err
	}
	if err := c.WaitUntilNotRouted(ctx, podUID, c.drainTimeout); err != nil {
		return err
	}
	opts := metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &podUID},
	}
	if err := c.clientset.CoreV1().Pods(c.namespace).Delete(ctx, podName, opts); err != nil {
		return fmt.Errorf("delete pod %s/%s: %w", c.namespace, podName, err)
	}
	return c.waitForPodDeleted(ctx, podUID)
}

func (c *Client) waitForPodDeleted(ctx context.Context, podUID types.UID) error {
	ctx, cancel := context.WithTimeout(ctx, c.rolloutTimeout)
	defer cancel()

	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	for {
		podList, err := c.clientset.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list pods after delete: %w", err)
		}
		found := false
		for _, pod := range podList.Items {
			if pod.UID == podUID {
				found = true
				break
			}
		}
		if !found {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for pod %s to be deleted: %w", podUID, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (c *Client) isPodRouted(ctx context.Context, podUID types.UID) (bool, error) {
	slices, err := c.clientset.DiscoveryV1().EndpointSlices(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=" + c.service,
	})
	if err != nil {
		return false, fmt.Errorf("list endpointslices for service %s: %w", c.service, err)
	}
	for _, slice := range slices.Items {
		if endpointSliceContainsRoutablePod(slice, podUID) {
			return true, nil
		}
	}
	return false, nil
}

func endpointSliceContainsRoutablePod(slice discoveryv1.EndpointSlice, podUID types.UID) bool {
	for _, endpoint := range slice.Endpoints {
		if endpoint.TargetRef == nil || endpoint.TargetRef.UID != podUID {
			continue
		}
		if endpoint.Conditions.Ready != nil && *endpoint.Conditions.Ready {
			return true
		}
		if endpoint.Conditions.Serving != nil && *endpoint.Conditions.Serving {
			return true
		}
	}
	return false
}

func upsertPodCondition(conditions []corev1.PodCondition, cond corev1.PodCondition) []corev1.PodCondition {
	for i, existing := range conditions {
		if existing.Type == cond.Type {
			conditions[i] = cond
			return conditions
		}
	}
	return append(conditions, cond)
}

func podContainersReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.ContainersReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func autopilotConditionStatus(pod *corev1.Pod, gate corev1.PodConditionType) corev1.ConditionStatus {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == gate {
			return cond.Status
		}
	}
	return ""
}

func podFullyReady(pod *corev1.Pod, gate corev1.PodConditionType) bool {
	ready := false
	gateOK := autopilotConditionStatus(pod, gate) == corev1.ConditionTrue
	for _, cond := range pod.Status.Conditions {
		switch cond.Type {
		case corev1.PodReady:
			ready = cond.Status == corev1.ConditionTrue
		}
	}
	return ready && gateOK
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
