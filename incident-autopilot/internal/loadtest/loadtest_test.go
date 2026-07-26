package loadtest

import (
	"context"
	"encoding/json"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func readyPod(name, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "autopilot-demo",
			Labels:    map[string]string{"app": "checkout-api"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: ip,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func TestSetBehaviorAllPods(t *testing.T) {
	posts := 0
	runner := NewRunner(
		fake.NewSimpleClientset(
			readyPod("pod-a", "10.0.0.1"),
			readyPod("pod-b", "10.0.0.2"),
		),
		"autopilot-demo",
		"checkout-api",
		WithPostFn(func(_ context.Context, _ string, body []byte) error {
			posts++
			var payload map[string]int
			if err := json.Unmarshal(body, &payload); err != nil {
				return err
			}
			if payload["inventoryDelayMs"] != 1500 {
				t.Fatalf("delay = %d", payload["inventoryDelayMs"])
			}
			return nil
		}),
	)

	if err := runner.SetBehavior(context.Background(), 1500, 0, ""); err != nil {
		t.Fatal(err)
	}
	if posts != 2 {
		t.Fatalf("expected 2 posts, got %d", posts)
	}

	status, err := runner.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.InjectedPods) != 2 {
		t.Fatalf("expected 2 injected pods, got %v", status.InjectedPods)
	}
}

func TestStartCapacityLoadCreatesJob(t *testing.T) {
	posts := 0
	runner := NewRunner(
		fake.NewSimpleClientset(readyPod("pod-a", "10.0.0.1")),
		"autopilot-demo",
		"checkout-api",
		WithPostFn(func(_ context.Context, _ string, _ []byte) error {
			posts++
			return nil
		}),
	)

	if err := runner.StartCapacityLoad(context.Background(), 500, 40, 300); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("expected 1 behavior post, got %d", posts)
	}

	job, err := runner.clientset.BatchV1().Jobs("autopilot-demo").Get(context.Background(), jobName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.Spec.Template.Spec.Containers[0].Image != loaderImage {
		t.Fatalf("unexpected image: %s", job.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestStopLoadDeletesJobAndClearsBehavior(t *testing.T) {
	posts := 0
	cs := fake.NewSimpleClientset(readyPod("pod-a", "10.0.0.1"))
	runner := NewRunner(cs, "autopilot-demo", "checkout-api", WithPostFn(func(_ context.Context, _ string, _ []byte) error {
		posts++
		return nil
	}))

	if err := runner.StartLoadJob(context.Background(), 20, 60); err != nil {
		t.Fatal(err)
	}
	runner.injectedPods["pod-a"] = behaviorState{DelayMs: 1500}

	if err := runner.StopLoad(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("expected clear behavior post, got %d posts", posts)
	}
	_, err := cs.BatchV1().Jobs("autopilot-demo").Get(context.Background(), jobName, metav1.GetOptions{})
	if err == nil {
		t.Fatal("expected job to be deleted")
	}
	if len(runner.injectedPods) != 0 {
		t.Fatalf("expected injected pods cleared, got %v", runner.injectedPods)
	}
}

func TestPickTargetPod(t *testing.T) {
	runner := NewRunner(
		fake.NewSimpleClientset(
			readyPod("pod-a", "10.0.0.1"),
			readyPod("pod-z", "10.0.0.2"),
		),
		"autopilot-demo",
		"checkout-api",
	)
	name, err := runner.PickTargetPod(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if name != "pod-z" {
		t.Fatalf("expected pod-z, got %s", name)
	}
}

func TestStatusRunningJob(t *testing.T) {
	start := metav1.Now()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: "autopilot-demo"},
		Status: batchv1.JobStatus{
			Active:    1,
			StartTime: &start,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Env: []corev1.EnvVar{
							{Name: "CONCURRENCY", Value: "40"},
							{Name: "DURATION_MS", Value: "300000"},
						},
					}},
				},
			},
		},
	}
	runner := NewRunner(fake.NewSimpleClientset(job), "autopilot-demo", "checkout-api")
	status, err := runner.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Running {
		t.Fatal("expected running job")
	}
	if status.VUs != 40 || status.DurationSeconds != 300 {
		t.Fatalf("unexpected status: %+v", status)
	}
}
