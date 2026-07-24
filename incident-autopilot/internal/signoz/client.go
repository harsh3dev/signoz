// Package signoz provides a minimal, trust-gated client for querying scalar
// service signals from a stock SigNoz instance via the v5 query_range API.
package signoz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/config"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/model"
)

var (
	ErrNoData         = errors.New("no telemetry data")
	ErrStaleTelemetry = errors.New("telemetry is stale")
)

// Scalar is a single, fully-elapsed data point resolved from a query_range
// response. ObservedAt distinguishes "no data" from a valid zero value.
type Scalar struct {
	Value      float64
	ObservedAt time.Time
}

type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

type queryRangeRequest struct {
	SchemaVersion  string `json:"schemaVersion"`
	Start          int64  `json:"start"`
	End            int64  `json:"end"`
	RequestType    string `json:"requestType"`
	CompositeQuery struct {
		Queries []promQLQuery `json:"queries"`
	} `json:"compositeQuery"`
}

type promQLQuery struct {
	Type string `json:"type"`
	Spec struct {
		Name     string `json:"name"`
		Query    string `json:"query"`
		Disabled bool   `json:"disabled"`
	} `json:"spec"`
}

type queryRangeResponse struct {
	Status string `json:"status"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
	Data struct {
		Meta struct {
			StepIntervals map[string]int64 `json:"stepIntervals"`
		} `json:"meta"`
		Data struct {
			Results []struct {
				QueryName    string `json:"queryName"`
				Aggregations []struct {
					Series []struct {
						Values []struct {
							Timestamp int64   `json:"timestamp"`
							Value     float64 `json:"value"`
						} `json:"values"`
					} `json:"series"`
				} `json:"aggregations"`
			} `json:"results"`
		} `json:"data"`
	} `json:"data"`
}

const queryName = "A"

// QueryScalar runs a single PromQL query against SigNoz's v5 query_range API
// and returns the newest data point whose aggregation bucket has fully
// elapsed by end. Buckets that have not yet fully closed are treated as
// partial and never returned.
func (c *Client) QueryScalar(ctx context.Context, query string, start, end time.Time) (Scalar, error) {
	reqBody := queryRangeRequest{
		SchemaVersion: "v1",
		Start:         start.UnixMilli(),
		End:           end.UnixMilli(),
		RequestType:   "scalar",
	}
	q := promQLQuery{Type: "promql"}
	q.Spec.Name = queryName
	q.Spec.Query = query
	reqBody.CompositeQuery.Queries = []promQLQuery{q}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return Scalar{}, fmt.Errorf("marshal query_range request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v5/query_range", bytes.NewReader(payload))
	if err != nil {
		return Scalar{}, fmt.Errorf("build query_range request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("SIGNOZ-API-KEY", c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return Scalar{}, fmt.Errorf("query_range request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Scalar{}, fmt.Errorf("read query_range response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Scalar{}, fmt.Errorf("query_range returned status %d: %s", resp.StatusCode, string(body))
	}

	var decoded queryRangeResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return Scalar{}, fmt.Errorf("decode query_range response: %w", err)
	}
	if decoded.Status != "success" {
		msg := "unknown error"
		if decoded.Error != nil {
			msg = decoded.Error.Message
		}
		return Scalar{}, fmt.Errorf("query_range error: %s", msg)
	}

	if len(decoded.Data.Data.Results) == 0 {
		return Scalar{}, fmt.Errorf("%w: no results in response", ErrNoData)
	}
	result := decoded.Data.Data.Results[0]
	if len(result.Aggregations) == 0 || len(result.Aggregations[0].Series) == 0 {
		return Scalar{}, fmt.Errorf("%w: no series in response", ErrNoData)
	}

	stepSeconds, ok := decoded.Data.Meta.StepIntervals[result.QueryName]
	if !ok || stepSeconds <= 0 {
		stepSeconds = 60
	}
	stepDuration := time.Duration(stepSeconds) * time.Second

	var newest *Scalar
	for _, series := range result.Aggregations[0].Series {
		for _, v := range series.Values {
			ts := time.UnixMilli(v.Timestamp)
			if end.Sub(ts) < stepDuration {
				// This bucket has not fully elapsed yet; it is partial.
				continue
			}
			if newest == nil || ts.After(newest.ObservedAt) {
				newest = &Scalar{Value: v.Value, ObservedAt: ts}
			}
		}
	}
	if newest == nil {
		return Scalar{}, fmt.Errorf("%w: only partial data points available", ErrNoData)
	}
	return *newest, nil
}

// Snapshot queries all required service signals concurrently and fails
// closed if any of them is absent or older than cfg.Signals.FreshnessLimit.
func (c *Client) Snapshot(ctx context.Context, cfg config.Config, replicas int32) (model.Snapshot, error) {
	now := time.Now()
	window := 5 * time.Minute
	start := now.Add(-window)

	type namedQuery struct {
		name  string
		query string
	}
	queries := []namedQuery{
		{"request_rate", cfg.Signals.RequestRateQuery},
		{"p95_latency", cfg.Signals.P95LatencyQuery},
		{"error_rate", cfg.Signals.ErrorRateQuery},
		{"sli", cfg.Signals.SLIQuery},
	}

	results := make(map[string]Scalar, len(queries))
	errs := make(map[string]error, len(queries))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, nq := range queries {
		nq := nq
		wg.Add(1)
		go func() {
			defer wg.Done()
			scalar, err := c.QueryScalar(ctx, nq.query, start, now)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[nq.name] = err
				return
			}
			results[nq.name] = scalar
		}()
	}
	wg.Wait()

	for _, nq := range queries {
		if err := errs[nq.name]; err != nil {
			return model.Snapshot{}, fmt.Errorf("signal %q: %w", nq.name, err)
		}
		if freshness := cfg.Signals.FreshnessLimit; freshness > 0 {
			if now.Sub(results[nq.name].ObservedAt) > freshness {
				return model.Snapshot{}, fmt.Errorf("signal %q: %w", nq.name, ErrStaleTelemetry)
			}
		}
	}

	return model.Snapshot{
		Service:         cfg.Target.Service,
		ObservedAt:      now,
		CurrentReplicas: replicas,
		RequestRate:     results["request_rate"].Value,
		P95MS:           results["p95_latency"].Value,
		ErrorRate:       results["error_rate"].Value,
		SLI:             results["sli"].Value,
	}, nil
}

var podFilterMarker = "${POD_FILTER}"
var podNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ExpandPodQuery injects a validated Kubernetes pod name into baseQuery at
// the ${POD_FILTER} marker. It never concatenates untrusted text into
// arbitrary PromQL: the pod name must match Kubernetes pod naming rules and
// the marker must be present.
func ExpandPodQuery(baseQuery, podName string) (string, error) {
	if !podNamePattern.MatchString(podName) {
		return "", fmt.Errorf("invalid pod name %q", podName)
	}
	if !strings.Contains(baseQuery, podFilterMarker) {
		return "", fmt.Errorf("query is missing the %s marker", podFilterMarker)
	}
	filter := fmt.Sprintf(`k8s_pod_name="%s"`, podName)
	return strings.ReplaceAll(baseQuery, podFilterMarker, filter), nil
}
