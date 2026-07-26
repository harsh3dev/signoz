// Package loadtest drives demo load generation and pod behavior injection
// from inside the controller pod (no kubectl exec).
package loadtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	jobName     = "load-generator"
	loaderImage = "telemetry-shop:dev"
	servicePort = 3000
)

type behaviorState struct {
	DelayMs   int `json:"delayMs"`
	ErrorRate int `json:"errorRate"`
}

// LoadStatus describes an active load job and injected pod behavior.
type LoadStatus struct {
	Running         bool              `json:"running"`
	VUs             int               `json:"vus,omitempty"`
	DurationSeconds int               `json:"durationSeconds,omitempty"`
	StartedAt       *time.Time        `json:"startedAt,omitempty"`
	ElapsedSeconds  int               `json:"elapsedSeconds,omitempty"`
	InjectedPods    map[string]string `json:"injectedPods,omitempty"`
}

// Runner manages load-generator Jobs and demo-app behavior injection.
type Runner struct {
	clientset  kubernetes.Interface
	namespace  string
	deployment string

	httpClient *http.Client
	postFn     func(ctx context.Context, url string, body []byte) error

	mu           sync.Mutex
	injectedPods map[string]behaviorState
}

type Option func(*Runner)

// WithHTTPClient overrides the HTTP client used for behavior POSTs (tests).
func WithHTTPClient(c *http.Client) Option {
	return func(r *Runner) { r.httpClient = c }
}

// WithPostFn overrides behavior POSTs entirely (tests).
func WithPostFn(fn func(ctx context.Context, url string, body []byte) error) Option {
	return func(r *Runner) { r.postFn = fn }
}

func NewRunner(clientset kubernetes.Interface, namespace, deployment string, opts ...Option) *Runner {
	r := &Runner{
		clientset:    clientset,
		namespace:    namespace,
		deployment:   deployment,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		injectedPods: make(map[string]behaviorState),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func (r *Runner) labelSelector() string {
	return "app=" + r.deployment
}

func (r *Runner) listReadyPods(ctx context.Context) ([]corev1.Pod, error) {
	list, err := r.clientset.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: r.labelSelector(),
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	ready := make([]corev1.Pod, 0, len(list.Items))
	for _, pod := range list.Items {
		if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
			continue
		}
		isReady := false
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				isReady = true
				break
			}
		}
		if isReady {
			ready = append(ready, pod)
		}
	}
	return ready, nil
}

// PickTargetPod returns the last ready pod name (sorted), mirroring demo-lib.
func (r *Runner) PickTargetPod(ctx context.Context) (string, error) {
	pods, err := r.listReadyPods(ctx)
	if err != nil {
		return "", err
	}
	if len(pods) == 0 {
		return "", fmt.Errorf("no ready pods for deployment %q", r.deployment)
	}
	names := make([]string, len(pods))
	for i, pod := range pods {
		names[i] = pod.Name
	}
	sort.Strings(names)
	return names[len(names)-1], nil
}

// SetBehavior POSTs demo behavior to one pod (targetPod non-empty) or all
// ready pods (targetPod empty).
func (r *Runner) SetBehavior(ctx context.Context, delayMs, errorRate int, targetPod string) error {
	pods, err := r.listReadyPods(ctx)
	if err != nil {
		return err
	}
	if len(pods) == 0 {
		return fmt.Errorf("no ready pods for deployment %q", r.deployment)
	}

	targets := pods
	if targetPod != "" {
		targets = nil
		for _, pod := range pods {
			if pod.Name == targetPod {
				targets = []corev1.Pod{pod}
				break
			}
		}
		if len(targets) == 0 {
			return fmt.Errorf("target pod %q not found or not ready", targetPod)
		}
	}

	payload, err := json.Marshal(map[string]int{
		"inventoryDelayMs":    delayMs,
		"inventoryErrorRate":  errorRate,
	})
	if err != nil {
		return fmt.Errorf("marshal behavior payload: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, pod := range targets {
		url := fmt.Sprintf("http://%s:%d/api/demo/behavior", pod.Status.PodIP, servicePort)
		if err := r.postBehavior(ctx, url, payload); err != nil {
			return fmt.Errorf("set behavior on %s: %w", pod.Name, err)
		}
		if delayMs == 0 && errorRate == 0 {
			delete(r.injectedPods, pod.Name)
		} else {
			r.injectedPods[pod.Name] = behaviorState{DelayMs: delayMs, ErrorRate: errorRate}
		}
	}
	return nil
}

func (r *Runner) postBehavior(ctx context.Context, url string, payload []byte) error {
	if r.postFn != nil {
		return r.postFn(ctx, url, payload)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// StartCapacityLoad sets inventory delay on all pods and starts the load job.
func (r *Runner) StartCapacityLoad(ctx context.Context, delayMs, vus, durationSeconds int) error {
	if err := r.SetBehavior(ctx, delayMs, 0, ""); err != nil {
		return err
	}
	return r.startJob(ctx, vus, durationSeconds)
}

// StartLoadJob creates the load-generator Job without changing pod behavior.
func (r *Runner) StartLoadJob(ctx context.Context, vus, durationSeconds int) error {
	return r.startJob(ctx, vus, durationSeconds)
}

func (r *Runner) startJob(ctx context.Context, vus, durationSeconds int) error {
	jobs := r.clientset.BatchV1().Jobs(r.namespace)
	_ = jobs.Delete(ctx, jobName, metav1.DeleteOptions{})

	durationMS := int64(durationSeconds) * 1000
	backoff := int32(0)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: r.namespace,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:            "loader",
							Image:           loaderImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Env: []corev1.EnvVar{
								{Name: "CONCURRENCY", Value: fmt.Sprintf("%d", vus)},
								{Name: "DURATION_MS", Value: fmt.Sprintf("%d", durationMS)},
								{Name: "TARGET_URL", Value: fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/api/orders", r.deployment, r.namespace, servicePort)},
							},
							Command: []string{"node", "-e"},
							Args: []string{loadGeneratorScript},
						},
					},
				},
			},
		},
	}
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create load job: %w", err)
	}
	return nil
}

const loadGeneratorScript = `
const http = require("http");
const url = process.env.TARGET_URL;
const duration = Number(process.env.DURATION_MS || 900000);
const concurrency = Number(process.env.CONCURRENCY || 20);
const payload = JSON.stringify({
  items: [{ id: "prod-001", quantity: 1 }],
  customerName: "load-generator",
  shippingAddress: "1 Autopilot Way",
});
const deadline = Date.now() + duration;
const worker = () => {
  if (Date.now() >= deadline) return;
  const req = http.request(
    url,
    {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "Content-Length": Buffer.byteLength(payload),
      },
    },
    (res) => {
      res.resume();
      setImmediate(worker);
    },
  );
  req.on("error", () => setImmediate(worker));
  req.write(payload);
  req.end();
};
for (let i = 0; i < concurrency; i++) worker();
setTimeout(() => process.exit(0), duration + 2000);
`

// StopLoad deletes the load job and clears injected behavior on all pods.
func (r *Runner) StopLoad(ctx context.Context) error {
	_ = r.clientset.BatchV1().Jobs(r.namespace).Delete(ctx, jobName, metav1.DeleteOptions{})
	return r.SetBehavior(ctx, 0, 0, "")
}

// Status reports load job state and in-memory injected behavior tracking.
func (r *Runner) Status(ctx context.Context) (LoadStatus, error) {
	status := LoadStatus{InjectedPods: map[string]string{}}

	job, err := r.clientset.BatchV1().Jobs(r.namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err == nil && job.Status.StartTime != nil {
		status.Running = job.Status.Active > 0 || job.Status.Succeeded == 0 && job.Status.Failed == 0
		if job.Status.Active > 0 || (job.Status.Succeeded == 0 && job.Status.Failed == 0 && job.Status.CompletionTime == nil) {
			status.Running = true
		}
		start := job.Status.StartTime.Time
		status.StartedAt = &start
		status.ElapsedSeconds = int(time.Since(start).Seconds())

		for _, env := range job.Spec.Template.Spec.Containers[0].Env {
			switch env.Name {
			case "CONCURRENCY":
				fmt.Sscanf(env.Value, "%d", &status.VUs)
			case "DURATION_MS":
				var ms int
				fmt.Sscanf(env.Value, "%d", &ms)
				status.DurationSeconds = ms / 1000
			}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for name, b := range r.injectedPods {
		parts := []string{}
		if b.DelayMs > 0 {
			parts = append(parts, fmt.Sprintf("delay=%dms", b.DelayMs))
		}
		if b.ErrorRate > 0 {
			parts = append(parts, fmt.Sprintf("error=%d%%", b.ErrorRate))
		}
		if len(parts) > 0 {
			status.InjectedPods[name] = strings.Join(parts, ", ")
		}
	}
	return status, nil
}

// ListPodNames returns ready pod names for UI dropdowns.
func (r *Runner) ListPodNames(ctx context.Context) ([]string, error) {
	pods, err := r.listReadyPods(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(pods))
	for i, pod := range pods {
		names[i] = pod.Name
	}
	sort.Strings(names)
	return names, nil
}
