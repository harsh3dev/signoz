// Command autopilot runs the SigNoz Incident Autopilot controller: it
// evaluates trusted SigNoz telemetry against the configured policy, exposes
// an approval UI/API, and publishes a bounded desired-replica metric that a
// KEDA ScaledObject consumes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	otelmetric "go.opentelemetry.io/otel/metric"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/config"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/controller"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/kube"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/policy"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/signoz"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/telemetry"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the autopilot configuration file")
	listenAddr := flag.String("listen-addr", ":8080", "address to serve the approval UI/API and /metrics on")
	kubeconfig := flag.String("kubeconfig", "", "path to a kubeconfig file; defaults to in-cluster config")
	approvalSecretEnv := flag.String("approval-secret-env", "AUTOPILOT_APPROVAL_SECRET", "environment variable holding the approval HMAC/bearer secret")
	otlpEndpoint := flag.String("otlp-endpoint", "", "SigNoz OTLP HTTP endpoint for controller metrics/logs (e.g. http://host.docker.internal:4318); leave empty to disable")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	log.Printf("incident-autopilot starting: service=%s namespace=%s mode=%s",
		cfg.Target.Service, cfg.Target.Namespace, cfg.Controller.Mode)

	approvalSecret := []byte(os.Getenv(*approvalSecretEnv))
	if len(approvalSecret) == 0 {
		log.Fatalf("approval secret is required: set %s", *approvalSecretEnv)
	}

	signozAPIKey := ""
	if cfg.SigNoz.APIKeyEnv != "" {
		signozAPIKey = os.Getenv(cfg.SigNoz.APIKeyEnv)
	}
	signozClient := signoz.NewClient(cfg.SigNoz.URL, signozAPIKey)

	kubeClient, err := newKubeClient(*kubeconfig, cfg)
	if err != nil {
		log.Fatalf("build kubernetes client: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		meter    otelmetric.Meter
		logger   otellog.Logger
		otelShut = func(context.Context) error { return nil }
	)
	if *otlpEndpoint != "" {
		providers, err := telemetry.NewProviders(ctx, *otlpEndpoint, "incident-autopilot")
		if err != nil {
			log.Printf("warning: OTLP telemetry disabled, continuing with Prometheus only: %v", err)
		} else {
			meter, logger, otelShut = providers.Meter, providers.Logger, providers.Shutdown
		}
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := otelShut(shutdownCtx); err != nil {
			log.Printf("warning: OTLP shutdown: %v", err)
		}
	}()

	emitter, err := telemetry.New(cfg, meter, logger)
	if err != nil {
		log.Fatalf("build telemetry emitter: %v", err)
	}

	engine := policy.New(cfg)
	ctrl, err := controller.New(cfg, engine, signozClient, kubeClient, emitter, approvalSecret,
		controller.WithMetricsHandler(emitter.Handler()),
		controller.WithPrometheusAPIHandler(emitter.InstantQueryHandler()))
	if err != nil {
		log.Fatalf("build controller: %v", err)
	}

	server := &http.Server{
		Addr:              *listenAddr,
		Handler:           ctrl.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("serving approval UI/API and /metrics on %s", *listenAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	ticker := time.NewTicker(cfg.Controller.EvaluationInterval)
	defer ticker.Stop()

	log.Printf("evaluation loop starting: interval=%s", cfg.Controller.EvaluationInterval)
	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := server.Shutdown(shutdownCtx); err != nil {
				log.Printf("warning: http server shutdown: %v", err)
			}
			cancel()
			return
		case <-ticker.C:
			evalCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			if err := ctrl.Evaluate(evalCtx); err != nil {
				log.Printf("evaluate: %v", err)
			}
			cancel()
		}
	}
}

func newKubeClient(kubeconfigPath string, cfg config.Config) (*kube.Client, error) {
	restConfig, err := loadRESTConfig(kubeconfigPath)
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes clientset: %w", err)
	}
	return kube.New(cfg, clientset), nil
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
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig %q: %w", kubeconfigPath, err)
	}
	return cfg, nil
}
