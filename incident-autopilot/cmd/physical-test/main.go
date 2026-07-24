// Command physical-test exercises Task 7 against a live Kubernetes API:
// Target, WaitForRollout, controller.Verify, and OTLP incident report emission.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func main() {
	configPath := flag.String("config", "config.example.yaml", "autopilot config path")
	kubeconfig := flag.String("kubeconfig", "", "kubeconfig path")
	scaleTo := flag.Int("scale-to", 4, "replicas to scale checkout-api to before verification")
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
	kubeClient := kube.New(cfg, clientset, kube.WithPollInterval(2*time.Second), kube.WithRolloutTimeout(3*time.Minute))

	ctx := context.Background()

	fmt.Println("==> Reading target deployment state")
	beforeTarget, err := kubeClient.Target(ctx)
	if err != nil {
		log.Fatalf("target: %v", err)
	}
	fmt.Printf("    deployment uid=%s generation=%d desired=%d available=%d owned_pods=%d\n",
		beforeTarget.DeploymentUID, beforeTarget.Generation, beforeTarget.DesiredReplicas,
		beforeTarget.AvailableReplicas, len(beforeTarget.Pods))

	fmt.Printf("==> Scaling %s/%s to %d replicas\n", cfg.Target.Namespace, cfg.Target.Deployment, *scaleTo)
	deploy, err := clientset.AppsV1().Deployments(cfg.Target.Namespace).Get(ctx, cfg.Target.Deployment, metav1.GetOptions{})
	if err != nil {
		log.Fatalf("get deployment: %v", err)
	}
	replicas := int32(*scaleTo)
	deploy.Spec.Replicas = &replicas
	if _, err := clientset.AppsV1().Deployments(cfg.Target.Namespace).Update(ctx, deploy, metav1.UpdateOptions{}); err != nil {
		log.Fatalf("scale deployment: %v", err)
	}

	afterScale, err := kubeClient.Target(ctx)
	if err != nil {
		log.Fatalf("target after scale: %v", err)
	}
	fmt.Printf("==> Waiting for rollout generation %d\n", afterScale.Generation)
	if err := kubeClient.WaitForRollout(ctx, afterScale.Generation, 3*time.Minute); err != nil {
		log.Fatalf("wait for rollout: %v", err)
	}
	fmt.Println("    rollout complete")

	providers, err := telemetry.NewProviders(ctx, *otlpEndpoint, "incident-autopilot-physical-test")
	if err != nil {
		log.Fatalf("otlp providers: %v", err)
	}
	defer providers.Shutdown(context.Background())

	emitter, err := telemetry.New(cfg, providers.Meter, providers.Logger)
	if err != nil {
		log.Fatalf("emitter: %v", err)
	}

	before := model.Snapshot{
		CurrentReplicas: beforeTarget.DesiredReplicas,
		Available:       beforeTarget.AvailableReplicas,
		SLI:             0.92,
		P95MS:           1200,
		ErrorRate:       0.08,
	}
	rec := model.Recommendation{
		ID:                  "physical-test-" + time.Now().UTC().Format("20060102T150405Z"),
		Decision:            model.DecisionScaleUp,
		CurrentReplicas:     beforeTarget.DesiredReplicas,
		RecommendedReplicas: int32(*scaleTo),
	}
	afterSnap := model.Snapshot{SLI: 0.995, P95MS: 800, ErrorRate: 0.01}

	ctrl, err := controller.New(cfg, policy.New(cfg), &sequenceSnapshots{values: []model.Snapshot{afterSnap}},
		kubeClient, emitter, []byte("physical-test-secret"),
		controller.WithRolloutVerifier(kubeClient),
		controller.WithSleep(func(time.Duration) {}),
	)
	if err != nil {
		log.Fatalf("controller: %v", err)
	}

	fmt.Println("==> Running post-scale verification")
	verification, err := ctrl.Verify(ctx, before, rec)
	if err != nil {
		log.Fatalf("verify: %v", err)
	}

	fmt.Println("==> Verification result")
	fmt.Printf("    result:      %s\n", verification.Result)
	fmt.Printf("    before SLI:  %.3f\n", verification.BeforeSLI)
	fmt.Printf("    after SLI:   %.3f\n", verification.AfterSLI)
	fmt.Printf("    before P95:  %.0fms\n", verification.BeforeP95MS)
	fmt.Printf("    after P95:   %.0fms\n", verification.AfterP95MS)
	fmt.Printf("    before err:  %.3f\n", verification.BeforeErrorRate)
	fmt.Printf("    after err:   %.3f\n", verification.AfterErrorRate)

	final, err := kubeClient.Target(ctx)
	if err != nil {
		log.Fatalf("final target: %v", err)
	}
	fmt.Printf("    final replicas: desired=%d available=%d\n", final.DesiredReplicas, final.AvailableReplicas)

	if verification.Result != "recovered" {
		log.Fatalf("expected recovered, got %s", verification.Result)
	}
	if final.AvailableReplicas < int32(*scaleTo) {
		log.Fatalf("expected %d available replicas, got %d", *scaleTo, final.AvailableReplicas)
	}

	fmt.Println("==> PASS: physical verification succeeded; incident report emitted via OTLP")
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
