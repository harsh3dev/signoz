package telemetry

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/config"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/model"
)

func testConfig() config.Config {
	var cfg config.Config
	cfg.Target.Service = "checkout-api"
	cfg.Target.Namespace = "autopilot-demo"
	cfg.Target.Deployment = "checkout-api"
	cfg.Controller.Mode = "approval"
	return cfg
}

func scrape(t *testing.T, e *Emitter) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	e.Handler().ServeHTTP(rec, req)
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return string(body)
}

func TestRecordRecommendationExposesBoundedLabels(t *testing.T) {
	cfg := testConfig()
	e, err := New(cfg, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error creating emitter: %v", err)
	}

	e.RecordRecommendation(model.Recommendation{
		Decision:            model.DecisionScaleUp,
		RecommendedReplicas: 6,
		PolicyVersion:       "v1",
	})
	e.RecordObservedReplicas(2)
	e.RecordPublishedReplicas(2) // approval mode: the gated, KEDA-facing value holds at 2.
	e.RecordDecision(model.Recommendation{
		Decision:      model.DecisionScaleUp,
		PolicyVersion: "v1",
		CreatedAt:     time.Now(),
	})
	e.Heartbeat()

	body := scrape(t, e)
	for _, want := range []string{
		`autopilot_recommended_replicas{deployment="checkout-api",namespace="autopilot-demo",service="checkout-api"} 2`,
		`autopilot_current_replicas{deployment="checkout-api",namespace="autopilot-demo",service="checkout-api"} 2`,
		`autopilot_pending_approval{deployment="checkout-api",namespace="autopilot-demo",service="checkout-api"} 1`,
		`autopilot_decision_total{decision="scale_up",deployment="checkout-api",namespace="autopilot-demo",policy_version="v1",service="checkout-api"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected /metrics output to contain %q, got:\n%s", want, body)
		}
	}
}

func TestRecordPublishedReplicasIsTheOnlyThingThatMovesTheKEDAMetric(t *testing.T) {
	cfg := testConfig()
	e, err := New(cfg, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error creating emitter: %v", err)
	}

	// A raw recommendation to scale to 6, on its own, must never move the
	// KEDA-facing metric: that would bypass the approval gate.
	e.RecordRecommendation(model.Recommendation{Decision: model.DecisionScaleUp, RecommendedReplicas: 6})
	e.RecordPublishedReplicas(2)
	body := scrape(t, e)
	if !strings.Contains(body, `autopilot_recommended_replicas{deployment="checkout-api",namespace="autopilot-demo",service="checkout-api"} 2`) {
		t.Fatalf("expected an unapproved recommendation to leave the KEDA metric at 2, got:\n%s", body)
	}

	// Only an explicit RecordPublishedReplicas call (i.e. an approved or
	// automatic action) may move it.
	e.RecordPublishedReplicas(6)
	body = scrape(t, e)
	if !strings.Contains(body, `autopilot_recommended_replicas{deployment="checkout-api",namespace="autopilot-demo",service="checkout-api"} 6`) {
		t.Fatalf("expected the KEDA metric to reach 6 after an explicit publish, got:\n%s", body)
	}
}

func TestRecordRecommendationHoldsDoNotMarkPendingApproval(t *testing.T) {
	cfg := testConfig()
	e, err := New(cfg, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error creating emitter: %v", err)
	}

	e.RecordRecommendation(model.Recommendation{Decision: model.DecisionHold, RecommendedReplicas: 2})

	body := scrape(t, e)
	if !strings.Contains(body, `autopilot_pending_approval{deployment="checkout-api",namespace="autopilot-demo",service="checkout-api"} 0`) {
		t.Fatalf("expected pending_approval to be 0 for a hold decision, got:\n%s", body)
	}
}
