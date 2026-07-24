package kube

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/config"
)

const (
	testNamespace  = "autopilot-demo"
	testDeployment = "checkout-api"
)

func testConfig() config.Config {
	var cfg config.Config
	cfg.Target.Namespace = testNamespace
	cfg.Target.Deployment = testDeployment
	return cfg
}

func int32Ptr(v int32) *int32 { return &v }

func readyDeployment(uid types.UID, generation int64, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testDeployment,
			Namespace:  testNamespace,
			UID:        uid,
			Generation: generation,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(replicas),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": testDeployment},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: generation,
			Replicas:           replicas,
			AvailableReplicas:  replicas,
		},
	}
}

func ownedPod(name string, rsUID types.UID) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", UID: rsUID, Controller: boolPtr(true)},
			},
		},
	}
}

func boolPtr(v bool) *bool { return &v }

func ownedReplicaSet(uid, deployUID types.UID) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "checkout-api-rs",
			Namespace: testNamespace,
			UID:       uid,
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Deployment", UID: deployUID, Controller: boolPtr(true)},
			},
		},
	}
}

func TestWaitForRolloutSucceedsWhenDesiredPodsAreReady(t *testing.T) {
	deployUID := types.UID("deploy-uid-1")
	rsUID := types.UID("rs-uid-1")
	clientset := fake.NewClientset(
		readyDeployment(deployUID, 2, 4),
		ownedReplicaSet(rsUID, deployUID),
		ownedPod("checkout-api-abc", rsUID),
	)

	c := New(testConfig(), clientset, WithPollInterval(10*time.Millisecond))
	ctx := context.Background()

	if err := c.WaitForRollout(ctx, 2, time.Second); err != nil {
		t.Fatalf("expected rollout to succeed, got: %v", err)
	}
}

func TestWaitForRolloutTimesOutOnUnavailablePods(t *testing.T) {
	deployUID := types.UID("deploy-uid-2")
	deploy := readyDeployment(deployUID, 2, 4)
	deploy.Status.AvailableReplicas = 1 // not all ready

	clientset := fake.NewClientset(deploy)
	c := New(testConfig(), clientset, WithPollInterval(10*time.Millisecond))

	ctx := context.Background()
	err := c.WaitForRollout(ctx, 2, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected rollout to time out when pods are unavailable")
	}
}

func TestTargetRejectsUnexpectedDeploymentUID(t *testing.T) {
	deployUID := types.UID("deploy-uid-3")
	clientset := fake.NewClientset(readyDeployment(deployUID, 1, 2))

	c := New(testConfig(), clientset, WithExpectedDeploymentUID(types.UID("other-uid")))
	_, err := c.Target(context.Background())
	if err == nil {
		t.Fatal("expected error when deployment UID does not match pinned UID")
	}
}

func TestTargetListsOnlyOwnedPods(t *testing.T) {
	deployUID := types.UID("deploy-uid-4")
	rsUID := types.UID("rs-uid-4")
	foreignRS := types.UID("foreign-rs")

	clientset := fake.NewClientset(
		readyDeployment(deployUID, 1, 2),
		ownedReplicaSet(rsUID, deployUID),
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "foreign-rs",
				Namespace: testNamespace,
				UID:       foreignRS,
				OwnerReferences: []metav1.OwnerReference{
					{Kind: "Deployment", UID: types.UID("foreign-deploy"), Controller: boolPtr(true)},
				},
			},
		},
		ownedPod("checkout-api-owned", rsUID),
		ownedPod("checkout-api-foreign", foreignRS),
	)

	c := New(testConfig(), clientset)
	state, err := c.Target(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(state.Pods) != 1 {
		t.Fatalf("expected 1 owned pod, got %d", len(state.Pods))
	}
	if state.Pods[0].Name != "checkout-api-owned" {
		t.Fatalf("expected owned pod checkout-api-owned, got %s", state.Pods[0].Name)
	}
}

func TestValidatePodOwnershipRejectsForeignPod(t *testing.T) {
	deployUID := types.UID("deploy-uid-5")
	rsUID := types.UID("rs-uid-5")
	pod := ownedPod("checkout-api-owned", rsUID)

	clientset := fake.NewClientset(
		readyDeployment(deployUID, 1, 1),
		ownedReplicaSet(rsUID, deployUID),
		pod,
	)
	c := New(testConfig(), clientset)

	if err := c.ValidatePodOwnership(context.Background(), pod.Name, pod.UID); err != nil {
		t.Fatalf("expected owned pod to validate, got: %v", err)
	}
	if err := c.ValidatePodOwnership(context.Background(), "other-pod", types.UID("other")); err == nil {
		t.Fatal("expected foreign pod to be rejected")
	}
}

func TestReplicasReturnsDeploymentCounts(t *testing.T) {
	deployUID := types.UID("deploy-uid-6")
	clientset := fake.NewClientset(readyDeployment(deployUID, 1, 6))
	c := New(testConfig(), clientset)

	status, err := c.Replicas(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.Current != 6 || status.Available != 6 {
		t.Fatalf("expected 6/6 replicas, got %d/%d", status.Current, status.Available)
	}
}
