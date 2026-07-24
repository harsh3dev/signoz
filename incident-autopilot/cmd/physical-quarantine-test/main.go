// Command physical-quarantine-test exercises Task 8 against a live Kubernetes API:
// readiness gate sync, quarantine drain, replacement scale-up, and safe deletion.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/config"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/controller"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/kube"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/model"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/policy"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/telemetry"
)

type sequenceSnapshots struct {
	values []model.Snapshot
}

func (s *sequenceSnapshots) Snapshot(_ context.Context, _ config.Config, replicas int32) (model.Snapshot, error) {
	if len(s.values) == 0 {
		return model.Snapshot{}, fmt.Errorf("no snapshots configured")
	}
	snap := s.values[0]
	s.values = s.values[1:]
	snap.CurrentReplicas = replicas
	return snap, nil
}

type autoScaleRollout struct {
	*kube.Client
	clientset  kubernetes.Interface
	cfg        config.Config
	targetReplicas int32
	scaled     bool
}

func (a *autoScaleRollout) EnsureReplacementCapacity(ctx context.Context, replicas int32) error {
	a.targetReplicas = replicas
	return a.ensureScaled(ctx)
}

func (a *autoScaleRollout) ensureScaled(ctx context.Context) error {
	if a.scaled {
		return nil
	}
	deploy, err := a.clientset.AppsV1().Deployments(a.cfg.Target.Namespace).Get(ctx, a.cfg.Target.Deployment, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get deployment for replacement scale: %w", err)
	}
	replicas := a.targetReplicas
	if replicas == 0 {
		replicas = 1
		if deploy.Spec.Replicas != nil {
			replicas = *deploy.Spec.Replicas
		}
		replicas++
	}
	deploy.Spec.Replicas = &replicas
	if _, err := a.clientset.AppsV1().Deployments(a.cfg.Target.Namespace).Update(ctx, deploy, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("scale deployment for replacement: %w", err)
	}
	a.scaled = true
	fmt.Printf("    scaled deployment to %d replicas for replacement capacity\n", replicas)
	return nil
}

func main() {
	configPath := flag.String("config", "config.example.yaml", "autopilot config path")
	kubeconfig := flag.String("kubeconfig", "", "kubeconfig path")
	otlpEndpoint := flag.String("otlp-endpoint", "http://localhost:4318", "OTLP HTTP endpoint for incident report")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	cfg.Controller.VerificationWindow = 5 * time.Second

	restConfig, err := loadRESTConfig(*kubeconfig)
	if err != nil {
		log.Fatalf("kubeconfig: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		log.Fatalf("clientset: %v", err)
	}
	kubeClient := kube.New(cfg, clientset,
		kube.WithPollInterval(2*time.Second),
		kube.WithRolloutTimeout(8*time.Minute),
		kube.WithDrainTimeout(3*time.Minute),
	)

	ctx := context.Background()

	if err := runKubeQuarantineFlow(ctx, cfg, clientset, kubeClient); err != nil {
		log.Fatalf("kube quarantine flow failed: %v", err)
	}
	if err := runControllerApproveFlow(ctx, cfg, clientset, kubeClient, *otlpEndpoint); err != nil {
		log.Fatalf("controller approve quarantine flow failed: %v", err)
	}

	fmt.Println("==> PASS: physical quarantine verification succeeded")
}

func runKubeQuarantineFlow(ctx context.Context, cfg config.Config, clientset kubernetes.Interface, kubeClient *kube.Client) error {
	fmt.Println("==> [kube] Reading target deployment state")
	target, err := kubeClient.Target(ctx)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	if len(target.Pods) < 2 {
		return fmt.Errorf("need at least 2 owned pods, got %d", len(target.Pods))
	}
	baselineReplicas := target.DesiredReplicas
	badPod, err := pickPod(target.Pods, cfg.Target.ReadinessGate)
	if err != nil {
		return err
	}
	fmt.Printf("    desired=%d available=%d target_pod=%s uid=%s\n",
		target.DesiredReplicas, target.AvailableReplicas, badPod.Name, badPod.UID)

	fmt.Println("==> [kube] Initializing readiness gates on healthy pods")
	if err := kubeClient.SyncReadinessGates(ctx, ""); err != nil {
		return fmt.Errorf("sync readiness gates: %w", err)
	}
	if err := waitForAvailable(ctx, clientset, cfg, kubeClient, baselineReplicas, 3*time.Minute); err != nil {
		return fmt.Errorf("wait for ready pods after gate sync: %w", err)
	}

	fmt.Printf("==> [kube] Quarantining pod %s\n", badPod.Name)
	knownUIDs := make(map[types.UID]struct{}, len(target.Pods))
	for _, pod := range target.Pods {
		knownUIDs[pod.UID] = struct{}{}
	}
	if err := kubeClient.SetAutopilotReady(ctx, badPod.Name, badPod.UID, false, "TelemetryOutlier",
		"Quarantined after approved SigNoz Incident Autopilot recommendation"); err != nil {
		return fmt.Errorf("set autopilot ready false: %w", err)
	}

	fmt.Println("==> [kube] Waiting for traffic drain via EndpointSlices")
	if err := kubeClient.WaitUntilNotRouted(ctx, badPod.UID, 0); err != nil {
		return fmt.Errorf("wait until not routed: %w", err)
	}
	fmt.Println("    pod drained from service endpoints")

	fmt.Printf("==> [kube] Scaling replacement capacity to %d replicas\n", baselineReplicas+1)
	deploy, err := clientset.AppsV1().Deployments(cfg.Target.Namespace).Get(ctx, cfg.Target.Deployment, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get deployment: %w", err)
	}
	replicas := baselineReplicas + 1
	deploy.Spec.Replicas = &replicas
	if _, err := clientset.AppsV1().Deployments(cfg.Target.Namespace).Update(ctx, deploy, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("scale deployment: %w", err)
	}

	fmt.Println("==> [kube] Waiting for replacement pod readiness")
	if err := kubeClient.WaitForReplacementReady(ctx, badPod.UID, knownUIDs, 2*time.Minute); err != nil {
		return fmt.Errorf("wait for replacement pod: %w", err)
	}

	fmt.Printf("==> [kube] Deleting quarantined pod %s\n", badPod.Name)
	if err := kubeClient.DeleteOwnedPod(ctx, badPod.Name, badPod.UID); err != nil {
		return fmt.Errorf("delete quarantined pod: %w", err)
	}

	final, err := kubeClient.Target(ctx)
	if err != nil {
		return fmt.Errorf("final target: %w", err)
	}
	for _, pod := range final.Pods {
		if pod.UID == badPod.UID {
			return fmt.Errorf("quarantined pod %s still exists after delete", badPod.Name)
		}
	}
	if final.DesiredReplicas < baselineReplicas {
		return fmt.Errorf("expected at least %d desired replicas after replacement, got %d",
			baselineReplicas, final.DesiredReplicas)
	}
	fmt.Printf("    final desired=%d available=%d owned_pods=%d\n",
		final.DesiredReplicas, final.AvailableReplicas, len(final.Pods))

	// Reset deployment to a stable baseline before the controller flow.
	if err := resetDeploymentReplicas(ctx, clientset, cfg, kubeClient, baselineReplicas); err != nil {
		return fmt.Errorf("reset deployment after kube flow: %w", err)
	}
	return nil
}

func resetDeploymentReplicas(ctx context.Context, clientset kubernetes.Interface, cfg config.Config, kubeClient *kube.Client, replicas int32) error {
	deploy, err := clientset.AppsV1().Deployments(cfg.Target.Namespace).Get(ctx, cfg.Target.Deployment, metav1.GetOptions{})
	if err != nil {
		return err
	}
	deploy.Spec.Replicas = &replicas
	if _, err := clientset.AppsV1().Deployments(cfg.Target.Namespace).Update(ctx, deploy, metav1.UpdateOptions{}); err != nil {
		return err
	}
	return waitForAvailable(ctx, clientset, cfg, kubeClient, replicas, 3*time.Minute)
}

func runControllerApproveFlow(ctx context.Context, cfg config.Config, clientset kubernetes.Interface, kubeClient *kube.Client, otlpEndpoint string) error {
	fmt.Println("==> [controller] Preparing approval-gated quarantine replacement")
	target, err := kubeClient.Target(ctx)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	if len(target.Pods) < 2 {
		return fmt.Errorf("need at least 2 owned pods, got %d", len(target.Pods))
	}
	if err := kubeClient.SyncReadinessGates(ctx, ""); err != nil {
		return fmt.Errorf("sync readiness gates: %w", err)
	}
	if err := waitForAvailable(ctx, clientset, cfg, kubeClient, target.DesiredReplicas, 3*time.Minute); err != nil {
		return fmt.Errorf("wait for ready pods: %w", err)
	}

	target, err = kubeClient.Target(ctx)
	if err != nil {
		return fmt.Errorf("target after sync: %w", err)
	}
	badPod, err := pickPod(target.Pods, cfg.Target.ReadinessGate)
	if err != nil {
		return err
	}
	current := target.DesiredReplicas

	providers, err := telemetry.NewProviders(ctx, otlpEndpoint, "incident-autopilot-physical-quarantine")
	var emitter *telemetry.Emitter
	if err != nil {
		fmt.Printf("    OTLP unavailable (%v); continuing with Prometheus-only telemetry\n", err)
		emitter, err = telemetry.New(cfg, nil, nil)
	} else {
		defer providers.Shutdown(context.Background())
		emitter, err = telemetry.New(cfg, providers.Meter, providers.Logger)
	}
	if err != nil {
		return fmt.Errorf("emitter: %w", err)
	}

	secret := []byte("physical-quarantine-secret")
	statePath := filepath.Join(os.TempDir(), fmt.Sprintf("autopilot-physical-quarantine-%d.json", time.Now().UnixNano()))
	defer os.Remove(statePath)

	rec := model.Recommendation{
		ID:                  "physical-quarantine-" + time.Now().UTC().Format("20060102T150405Z"),
		CreatedAt:           time.Now(),
		ExpiresAt:           time.Now().Add(10 * time.Minute),
		Decision:            model.DecisionQuarantine,
		CurrentReplicas:     current,
		RecommendedReplicas: current + 1,
		TargetPod:           badPod.Name,
		Reason:              "Physical test quarantine recommendation",
	}
	before := model.Snapshot{
		CurrentReplicas: current,
		Available:       target.AvailableReplicas,
		SLI:             0.95,
		P95MS:           1200,
		ErrorRate:       0.05,
	}
	if err := writeSeedState(statePath, rec, before); err != nil {
		return fmt.Errorf("seed controller state: %w", err)
	}

	cfg.Controller.StatePath = statePath
	rollout := &autoScaleRollout{Client: kubeClient, clientset: clientset, cfg: cfg}
	ctrl, err := controller.New(cfg, policy.New(cfg),
		&sequenceSnapshots{values: []model.Snapshot{{SLI: 0.995, P95MS: 800, ErrorRate: 0.01}}},
		kubeClient, emitter, secret,
		controller.WithRolloutVerifier(kubeClient),
		controller.WithPodRemediator(kubeClient),
		controller.WithReplacementScaler(rollout),
		controller.WithSleep(func(time.Duration) {}),
	)
	if err != nil {
		return fmt.Errorf("controller: %w", err)
	}

	token := controller.SignApproval(secret, rec.ID, rec.ExpiresAt)
	fmt.Printf("==> [controller] Approving quarantine for pod %s\n", badPod.Name)
	if err := ctrl.Approve(ctx, rec.ID, token, "physical-test", time.Now()); err != nil {
		return fmt.Errorf("approve quarantine: %w", err)
	}

	final, err := kubeClient.Target(ctx)
	if err != nil {
		return fmt.Errorf("final target: %w", err)
	}
	for _, pod := range final.Pods {
		if pod.UID == badPod.UID {
			return fmt.Errorf("quarantined pod %s still exists after controller approval", badPod.Name)
		}
	}
	if ctrl.PublishedReplicas() != current+1 {
		return fmt.Errorf("expected published replicas %d, got %d", current+1, ctrl.PublishedReplicas())
	}
	fmt.Printf("    controller published replicas=%d final desired=%d available=%d\n",
		ctrl.PublishedReplicas(), final.DesiredReplicas, final.AvailableReplicas)
	return nil
}

type seedState struct {
	LastRecommendation model.Recommendation `json:"last_recommendation"`
	LastSnapshot       model.Snapshot       `json:"last_snapshot"`
	PublishedReplicas  int32                `json:"published_replicas"`
}

func writeSeedState(path string, rec model.Recommendation, snap model.Snapshot) error {
	data, err := json.MarshalIndent(seedState{
		LastRecommendation: rec,
		LastSnapshot:       snap,
		PublishedReplicas:  snap.CurrentReplicas,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func pickPod(pods []corev1.Pod, gate string) (corev1.Pod, error) {
	gated := make([]corev1.Pod, 0, len(pods))
	for _, pod := range pods {
		for _, g := range pod.Spec.ReadinessGates {
			if string(g.ConditionType) == gate {
				gated = append(gated, pod)
				break
			}
		}
	}
	if len(gated) == 0 {
		return corev1.Pod{}, fmt.Errorf("no pods with readiness gate %q found; redeploy checkout-api and retry", gate)
	}
	sort.Slice(gated, func(i, j int) bool { return gated[i].Name < gated[j].Name })
	return gated[len(gated)-1], nil
}

func waitForAvailable(ctx context.Context, clientset kubernetes.Interface, cfg config.Config, kubeClient *kube.Client, desired int32, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if kubeClient != nil {
			if err := kubeClient.SyncReadinessGates(ctx, ""); err != nil {
				return err
			}
		}
		deploy, err := clientset.AppsV1().Deployments(cfg.Target.Namespace).Get(ctx, cfg.Target.Deployment, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if deploy.Status.AvailableReplicas >= desired {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %d available replicas, got %d: %w",
				desired, deploy.Status.AvailableReplicas, ctx.Err())
		case <-ticker.C:
		}
	}
}

func loadRESTConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
		if home, ok := os.LookupEnv("HOME"); ok {
			kubeconfigPath = filepath.Join(home, ".kube", "config")
		}
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
}
