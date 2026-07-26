package installer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/config"
)

func testConfig() config.Config {
	cfg := config.Config{}
	cfg.SigNoz.URL = "http://signoz.test"
	cfg.Target.Service = "checkout-api"
	cfg.Target.Namespace = "autopilot-demo"
	cfg.Target.Deployment = "checkout-api"
	cfg.Signals.RequestRateQuery = `sum(rate(checkout_requests_total{"service.name"="checkout-api"}[2m]))`
	cfg.Signals.P95LatencyQuery = `(histogram_quantile(0.95, sum by (le) (rate(checkout_duration_milliseconds_bucket{"service.name"="checkout-api"}[2m]))) or vector(0))`
	cfg.Signals.ErrorRateQuery = `(sum(rate(checkout_requests_total{"service.name"="checkout-api",status="failed"}[2m])) or sum(rate(checkout_requests_total{"service.name"="checkout-api",status="success"}[2m])) * 0) / sum(rate(checkout_requests_total{"service.name"="checkout-api",status="success"}[2m]))`
	cfg.Signals.SLIQuery = `1 - ((sum(rate(checkout_requests_total{"service.name"="checkout-api",status="failed"}[5m])) or sum(rate(checkout_requests_total{"service.name"="checkout-api",status="success"}[5m])) * 0) / sum(rate(checkout_requests_total{"service.name"="checkout-api",status="success"}[5m])))`
	cfg.Signals.SLIObjective = 0.99
	cfg.Signals.FreshnessLimit = 60 * time.Second
	cfg.Policy.MinReplicas = 2
	cfg.Policy.MaxReplicas = 10
	cfg.Controller.Mode = "approval"
	return cfg
}

func TestEnsureDashboardCreatesMissingDashboardOnce(t *testing.T) {
	cfg := testConfig()
	title := dashboardTitle(cfg)
	var createCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/dashboards":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success","data":{"dashboards":[],"total":0,"tags":[],"reservedKeywords":[]}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/dashboards":
			createCalls++
			body, _ := io.ReadAll(r.Body)
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("decode create payload: %v", err)
			}
			spec := payload["spec"].(map[string]any)
			panels := spec["panels"].(map[string]any)
			if _, ok := panels[widgetReplicas]; !ok {
				t.Fatalf("expected stable widget %q", widgetReplicas)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":"success","data":{"id":"dash-1"}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg.SigNoz.URL = srv.URL
	inst := New(cfg, "test-key", "http://autopilot.test")
	url, err := inst.EnsureDashboard(context.Background())
	if err != nil {
		t.Fatalf("EnsureDashboard: %v", err)
	}
	if createCalls != 1 {
		t.Fatalf("expected one create call, got %d", createCalls)
	}
	if url != srv.URL+"/dashboard/dash-1" {
		t.Fatalf("unexpected dashboard url: %s", url)
	}
	if got := dashboardTitle(cfg); got != title {
		t.Fatalf("unexpected title: %s", got)
	}
}

func TestEnsureDashboardUpdatesExistingDashboardByTitle(t *testing.T) {
	cfg := testConfig()
	title := dashboardTitle(cfg)
	var updateCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/dashboards":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success","data":{"dashboards":[{"id":"dash-existing","name":"incident-autopilot-checkout-api","spec":{"display":{"name":"` + title + `"}}}],"total":1,"tags":[],"reservedKeywords":[]}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/v2/dashboards/dash-existing":
			updateCalls++
			body, _ := io.ReadAll(r.Body)
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("decode update payload: %v", err)
			}
			if payload["name"] != "incident-autopilot-checkout-api" {
				t.Fatalf("expected RFC1123 root name, got %v", payload["name"])
			}
			spec := payload["spec"].(map[string]any)
			panels := spec["panels"].(map[string]any)
			if _, ok := panels[widgetPodOutliers]; !ok {
				t.Fatalf("expected stable widget %q in update payload", widgetPodOutliers)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"success","data":{"id":"dash-existing"}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg.SigNoz.URL = srv.URL
	inst := New(cfg, "test-key", "http://autopilot.test")
	url, err := inst.EnsureDashboard(context.Background())
	if err != nil {
		t.Fatalf("EnsureDashboard: %v", err)
	}
	if updateCalls != 1 {
		t.Fatalf("expected one update call, got %d", updateCalls)
	}
	if url != srv.URL+"/dashboard/dash-existing" {
		t.Fatalf("unexpected dashboard url: %s", url)
	}
}

func TestEnsureAlertsRequiresExistingChannel(t *testing.T) {
	cfg := testConfig()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/channels":
			_, _ = w.Write([]byte(`{"status":"success","data":[{"id":"ch-1","name":"other-channel"}]}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg.SigNoz.URL = srv.URL
	inst := New(cfg, "test-key", "http://autopilot.test")
	err := inst.EnsureAlerts(context.Background(), "hackathon-email")
	if err == nil {
		t.Fatal("expected channel lookup error")
	}
	if !strings.Contains(err.Error(), "notification channel not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureAlertsUpdatesExistingRuleWhenThresholdChanges(t *testing.T) {
	cfg := testConfig()
	title := dashboardTitle(cfg)
	var (
		mu          sync.Mutex
		createRules int
		updateRules int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/channels":
			_, _ = w.Write([]byte(`{"status":"success","data":[{"id":"ch-1","name":"hackathon-email"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/dashboards":
			_, _ = w.Write([]byte(`{"status":"success","data":{"dashboards":[{"id":"dash-1","name":"` + title + `","spec":{"display":{"name":"` + title + `"}}}],"total":1,"tags":[],"reservedKeywords":[]}}`))
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v2/dashboards/"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"success","data":{"id":"dash-1"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/rules":
			_, _ = w.Write([]byte(`{"status":"success","data":[
				{"id":"rule-1","alert":"Autopilot stale telemetry","condition":{"thresholds":{"kind":"basic","spec":[{"target":30,"op":"above"}]}}},
				{"id":"rule-2","alert":"Autopilot pending approval","condition":{"thresholds":{"kind":"basic","spec":[{"target":0,"op":"above"}]}}}
			]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/rules":
			mu.Lock()
			createRules++
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":"success","data":{"id":"rule-new"}}`))
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v2/rules/"):
			body, _ := io.ReadAll(r.Body)
			if r.URL.Path != "/api/v2/rules/rule-1" {
				t.Fatalf("expected stale telemetry update only, got %s", r.URL.Path)
			}
			if !strings.Contains(string(body), `"target":60`) {
				t.Fatalf("expected updated stale telemetry threshold 60, got %s", string(body))
			}
			mu.Lock()
			updateRules++
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg.SigNoz.URL = srv.URL
	inst := New(cfg, "test-key", "http://autopilot.test")
	if err := inst.EnsureAlerts(context.Background(), "hackathon-email"); err != nil {
		t.Fatalf("EnsureAlerts: %v", err)
	}
	if updateRules != 1 {
		t.Fatalf("expected one rule update, got %d", updateRules)
	}
	if createRules != 3 {
		t.Fatalf("expected three rule creates for missing alerts, got %d", createRules)
	}
}

func TestEnsureAlertsNeverCreatesNotificationChannel(t *testing.T) {
	cfg := testConfig()
	title := dashboardTitle(cfg)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/channels":
			t.Fatal("installer must not create notification channels")
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/channels":
			_, _ = w.Write([]byte(`{"status":"success","data":[{"id":"ch-1","name":"hackathon-email"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/dashboards":
			_, _ = w.Write([]byte(`{"status":"success","data":{"dashboards":[{"id":"dash-1","name":"` + title + `","spec":{"display":{"name":"` + title + `"}}}],"total":1,"tags":[],"reservedKeywords":[]}}`))
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v2/dashboards/"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/rules":
			_, _ = w.Write([]byte(`{"status":"success","data":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/rules":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"channels":["hackathon-email"]`) {
				t.Fatalf("expected alert to reference existing channel, got %s", string(body))
			}
			if strings.Contains(string(body), `"webhook_configs"`) {
				t.Fatal("installer must not embed webhook channel definitions in alerts")
			}
			w.WriteHeader(http.StatusCreated)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg.SigNoz.URL = srv.URL
	inst := New(cfg, "test-key", "http://autopilot.test")
	if err := inst.EnsureAlerts(context.Background(), "hackathon-email"); err != nil {
		t.Fatalf("EnsureAlerts: %v", err)
	}
}

func TestAlertAnnotationsLinkToApprovalUIAndDashboard(t *testing.T) {
	cfg := testConfig()
	inst := New(cfg, "test-key", "http://autopilot.test:9090")
	specs := inst.alertDefinitions("hackathon-email", "http://signoz.test/dashboard/dash-1")

	for _, spec := range specs {
		desc := spec.Annot["description"]
		if !strings.Contains(desc, "http://signoz.test/dashboard/dash-1") {
			t.Fatalf("alert %q missing dashboard link: %s", spec.Alert, desc)
		}
		if !strings.Contains(desc, "http://autopilot.test:9090/actions") {
			t.Fatalf("alert %q missing approval link: %s", spec.Alert, desc)
		}
	}
}
