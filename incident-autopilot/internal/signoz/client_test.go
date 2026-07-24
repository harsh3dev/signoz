package signoz

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/config"
)

// point is a shorthand for a query_range response value at a given offset
// (in seconds) before the request's end time.
type point struct {
	offsetSeconds int64
	value         float64
}

func newFakeServer(t *testing.T, stepSeconds int64, points []point) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("SIGNOZ-API-KEY") == "" {
			t.Errorf("expected SIGNOZ-API-KEY header to be set")
		}
		var body struct {
			End            int64 `json:"end"`
			CompositeQuery struct {
				Queries []struct {
					Spec struct {
						Name string `json:"name"`
					} `json:"spec"`
				} `json:"queries"`
			} `json:"compositeQuery"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		queryName := body.CompositeQuery.Queries[0].Spec.Name

		values := make([]map[string]any, 0, len(points))
		for _, p := range points {
			values = append(values, map[string]any{
				"timestamp": body.End - p.offsetSeconds*1000,
				"value":     p.value,
			})
		}

		resp := map[string]any{
			"status": "success",
			"data": map[string]any{
				"type": "scalar",
				"meta": map[string]any{
					"stepIntervals": map[string]any{queryName: stepSeconds},
				},
				"data": map[string]any{
					"results": []map[string]any{
						{
							"queryName": queryName,
							"aggregations": []map[string]any{
								{
									"series": []map[string]any{
										{"values": values},
									},
								},
							},
						},
					},
				},
			},
		}
		if len(points) == 0 {
			resp["data"].(map[string]any)["data"].(map[string]any)["results"].([]map[string]any)[0]["aggregations"] = []map[string]any{
				{"series": nil},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
}

func TestQueryScalarRejectsMissingSeries(t *testing.T) {
	server := newFakeServer(t, 60, nil)
	defer server.Close()

	client := NewClient(server.URL, "test-key")
	end := time.Now()
	_, err := client.QueryScalar(t.Context(), "sum(rate(checkout_requests_total[5m]))", end.Add(-5*time.Minute), end)
	if err == nil {
		t.Fatal("expected error for missing series")
	}
}

func TestQueryScalarRejectsPartialPoint(t *testing.T) {
	// Single point whose bucket has not fully closed relative to `end`.
	server := newFakeServer(t, 60, []point{{offsetSeconds: 10, value: 42}})
	defer server.Close()

	client := NewClient(server.URL, "test-key")
	end := time.Now()
	_, err := client.QueryScalar(t.Context(), "sum(rate(checkout_requests_total[5m]))", end.Add(-5*time.Minute), end)
	if err == nil {
		t.Fatal("expected error when only a partial point is available")
	}
}

func TestQueryScalarUsesNewestCompletePoint(t *testing.T) {
	// Points at 120s, 60s, and 0s (partial) before `end`, with a 60s step.
	// The 0s point has not fully closed, so the newest complete point is 60s.
	server := newFakeServer(t, 60, []point{
		{offsetSeconds: 120, value: 1},
		{offsetSeconds: 60, value: 2},
		{offsetSeconds: 0, value: 3},
	})
	defer server.Close()

	client := NewClient(server.URL, "test-key")
	end := time.Now()
	scalar, err := client.QueryScalar(t.Context(), "sum(rate(checkout_requests_total[5m]))", end.Add(-5*time.Minute), end)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if scalar.Value != 2 {
		t.Fatalf("expected newest complete point value 2, got %v", scalar.Value)
	}
}

func TestSnapshotRejectsStaleSignals(t *testing.T) {
	// Only complete point available is 10 minutes old, far outside the
	// configured freshness limit.
	server := newFakeServer(t, 60, []point{{offsetSeconds: 660, value: 5}})
	defer server.Close()

	client := NewClient(server.URL, "test-key")

	var cfg config.Config
	cfg.Signals.RequestRateQuery = "sum(rate(checkout_requests_total[5m]))"
	cfg.Signals.P95LatencyQuery = "histogram_quantile(0.95, checkout_duration_milliseconds_bucket)"
	cfg.Signals.ErrorRateQuery = "sum(rate(checkout_requests_total{status=\"failed\"}[5m]))"
	cfg.Signals.SLIQuery = "1 - sum(rate(checkout_requests_total{status=\"failed\"}[5m]))"
	cfg.Signals.FreshnessLimit = 60 * time.Second

	_, err := client.Snapshot(t.Context(), cfg, 2)
	if err == nil {
		t.Fatal("expected stale telemetry error")
	}
	if !errors.Is(err, ErrStaleTelemetry) {
		t.Fatalf("expected errors.Is(err, ErrStaleTelemetry), got %v", err)
	}
}

func TestExpandPodQueryInjectsValidatedPodName(t *testing.T) {
	query, err := ExpandPodQuery(`sum(rate(checkout_requests_total{${POD_FILTER}}[5m]))`, "checkout-api-abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := `sum(rate(checkout_requests_total{"k8s.pod.name"="checkout-api-abc123"}[5m]))`
	if query != expected {
		t.Fatalf("expected %q, got %q", expected, query)
	}
}

func TestExpandPodQueryRejectsInvalidPodName(t *testing.T) {
	_, err := ExpandPodQuery(`sum(rate(checkout_requests_total{${POD_FILTER}}[5m]))`, `checkout-api"; DROP`)
	if err == nil {
		t.Fatal("expected error for invalid pod name")
	}
}

func TestExpandPodQueryRejectsMissingMarker(t *testing.T) {
	_, err := ExpandPodQuery(`sum(rate(checkout_requests_total[5m]))`, "checkout-api-abc123")
	if err == nil {
		t.Fatal("expected error when template marker is missing")
	}
}
