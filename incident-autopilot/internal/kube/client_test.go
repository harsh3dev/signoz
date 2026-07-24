package kube

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/config"
)

const (
	testNamespace  = "autopilot-demo"
	testDeployment = "checkout-api"
	testService    = "checkout-api"
	testGate       = "autopilot.signoz.io/healthy"
)

func testConfig() config.Config {
	var cfg config.Config
	cfg.Target.Namespace = testNamespace
	cfg.Target.Deployment = testDeployment
	cfg.Target.Service = testService
	cfg.Target.ReadinessGate = testGate
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

func readyOwnedPod(name string, uid, rsUID types.UID) *corev1.Pod {
	pod := ownedPod(name, rsUID)
	pod.UID = uid
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		{Type: corev1.PodConditionType(testGate), Status: corev1.ConditionTrue},
	}
	return pod
}

func endpointSliceForPod(serviceName string, podUID types.UID, ready bool) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "slice-" + string(podUID),
			Namespace: testNamespace,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: serviceName,
			},
		},
		Endpoints: []discoveryv1.Endpoint{
			{
				TargetRef: &corev1.ObjectReference{
					Kind: "Pod",
					UID:  podUID,
				},
				Conditions: discoveryv1.EndpointConditions{
					Ready:   boolPtr(ready),
					Serving: boolPtr(ready),
				},
			},
		},
	}
}

func TestQuarantineRejectsPodNotOwnedByTarget(t *testing.T) {
	deployUID := types.UID("deploy-uid-q1")
	rsUID := types.UID("rs-uid-q1")
	podUID := types.UID("pod-uid-owned")

	clientset := fake.NewClientset(
		readyDeployment(deployUID, 1, 2),
		ownedReplicaSet(rsUID, deployUID),
		readyOwnedPod("checkout-api-owned", podUID, rsUID),
	)
	c := New(testConfig(), clientset)

	err := c.SetAutopilotReady(context.Background(), "checkout-api-owned", types.UID("wrong-uid"), false, "TelemetryOutlier", "quarantine")
	if err == nil {
		t.Fatal("expected error when quarantining a pod not owned by the target deployment")
	}
}

func TestQuarantineSetsCustomReadinessConditionFalse(t *testing.T) {
	deployUID := types.UID("deploy-uid-q2")
	rsUID := types.UID("rs-uid-q2")
	podUID := types.UID("pod-uid-q2")

	clientset := fake.NewClientset(
		readyDeployment(deployUID, 1, 2),
		ownedReplicaSet(rsUID, deployUID),
		readyOwnedPod("checkout-api-bad", podUID, rsUID),
	)
	c := New(testConfig(), clientset)

	if err := c.SetAutopilotReady(context.Background(), "checkout-api-bad", podUID, false, "TelemetryOutlier",
		"Quarantined after approved SigNoz Incident Autopilot recommendation"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pod, err := clientset.CoreV1().Pods(testNamespace).Get(context.Background(), "checkout-api-bad", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	var found bool
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodConditionType(testGate) {
			found = true
			if cond.Status != corev1.ConditionFalse {
				t.Fatalf("expected readiness gate false, got %s", cond.Status)
			}
			if cond.Reason != "TelemetryOutlier" {
				t.Fatalf("expected TelemetryOutlier reason, got %s", cond.Reason)
			}
		}
	}
	if !found {
		t.Fatal("expected custom readiness condition to be set")
	}
}

func TestDeleteWaitsUntilEndpointSliceNoLongerContainsPod(t *testing.T) {
	deployUID := types.UID("deploy-uid-q3")
	rsUID := types.UID("rs-uid-q3")
	podUID := types.UID("pod-uid-q3")

	clientset := fake.NewClientset(
		readyDeployment(deployUID, 1, 2),
		ownedReplicaSet(rsUID, deployUID),
		readyOwnedPod("checkout-api-bad", podUID, rsUID),
		endpointSliceForPod(testService, podUID, true),
	)
	c := New(testConfig(), clientset, WithPollInterval(10*time.Millisecond), WithDrainTimeout(50*time.Millisecond))

	err := c.WaitUntilNotRouted(context.Background(), podUID, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected drain to time out while pod remains in ready endpoints")
	}

	slices, err := clientset.DiscoveryV1().EndpointSlices(testNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=" + testService,
	})
	if err != nil {
		t.Fatalf("list endpointslices: %v", err)
	}
	if len(slices.Items) != 1 {
		t.Fatalf("expected one endpoints slice, got %d", len(slices.Items))
	}
	slice := slices.Items[0]
	slice.Endpoints[0].Conditions.Ready = boolPtr(false)
	slice.Endpoints[0].Conditions.Serving = boolPtr(false)
	if _, err := clientset.DiscoveryV1().EndpointSlices(testNamespace).Update(context.Background(), &slice, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update endpoints slice: %v", err)
	}

	if err := c.WaitUntilNotRouted(context.Background(), podUID, time.Second); err != nil {
		t.Fatalf("expected drain to succeed once endpoints are not ready: %v", err)
	}

	if err := c.DeleteOwnedPod(context.Background(), "checkout-api-bad", podUID); err != nil {
		t.Fatalf("unexpected delete error: %v", err)
	}
	_, err = clientset.CoreV1().Pods(testNamespace).Get(context.Background(), "checkout-api-bad", metav1.GetOptions{})
	if err == nil {
		t.Fatal("expected quarantined pod to be deleted after drain")
	}
}

func TestSyncReadinessGatesInitializesHealthyPods(t *testing.T) {
	deployUID := types.UID("deploy-uid-q4")
	rsUID := types.UID("rs-uid-q4")
	podUID := types.UID("pod-uid-q4")

	pod := readyOwnedPod("checkout-api-new", podUID, rsUID)
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}

	clientset := fake.NewClientset(
		readyDeployment(deployUID, 1, 2),
		ownedReplicaSet(rsUID, deployUID),
		pod,
	)
	c := New(testConfig(), clientset)

	if err := c.SyncReadinessGates(context.Background(), ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated, err := clientset.CoreV1().Pods(testNamespace).Get(context.Background(), "checkout-api-new", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if autopilotConditionStatus(updated, corev1.PodConditionType(testGate)) != corev1.ConditionTrue {
		t.Fatal("expected autopilot readiness gate to be initialized to true")
	}
}
