// Package installer idempotently provisions SigNoz dashboards and human-facing
// alerts for the Incident Autopilot controller.
package installer

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
	"time"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/config"
)

const (
	dashboardSchemaVersion = "v6"
	alertSchemaVersion     = "v2alpha1"
	alertAPIVersion        = "v5"

	widgetReplicas           = "autopilot-replicas"
	widgetRequestRate        = "autopilot-request-rate"
	widgetP95Latency         = "autopilot-p95-latency"
	widgetErrorRate          = "autopilot-error-rate"
	widgetSLI                = "autopilot-sli"
	widgetPendingApproval    = "autopilot-pending-approval"
	widgetTelemetryFreshness = "autopilot-telemetry-freshness"
	widgetDecisionsByReason  = "autopilot-decisions-by-reason"
	widgetScalingActions     = "autopilot-scaling-actions"
	widgetQuarantineActions  = "autopilot-quarantine-actions"
	widgetVerification       = "autopilot-verification"
	widgetPodOutliers        = "autopilot-pod-outliers"
)

var (
	ErrChannelNotFound = errors.New("notification channel not found")
	rfc1123NamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
)

type dashboardRef struct {
	ID   string
	Name string
}

// Installer provisions dashboards and alerts against a stock SigNoz instance.
type Installer struct {
	cfg         config.Config
	apiKey      string
	approvalURL string
	baseURL     string
	httpClient  *http.Client
}

func New(cfg config.Config, apiKey, approvalURL string) *Installer {
	return &Installer{
		cfg:         cfg,
		apiKey:      apiKey,
		approvalURL: strings.TrimRight(approvalURL, "/"),
		baseURL:     strings.TrimRight(cfg.SigNoz.URL, "/"),
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// WithHTTPClient overrides the HTTP client (tests only).
func (i *Installer) WithHTTPClient(client *http.Client) *Installer {
	i.httpClient = client
	return i
}

// EnsureDashboard creates or updates the Incident Autopilot dashboard by stable
// title and returns its SigNoz URL.
func (i *Installer) EnsureDashboard(ctx context.Context) (string, error) {
	title := dashboardTitle(i.cfg)
	payload := i.buildDashboard(title)

	existing, err := i.findDashboardByTitle(ctx, title)
	if err != nil {
		return "", err
	}
	if existing.ID == "" {
		created, err := i.createDashboard(ctx, payload)
		if err != nil {
			return "", err
		}
		return i.dashboardURL(created), nil
	}
	if err := i.updateDashboard(ctx, existing, payload); err != nil {
		return "", err
	}
	return i.dashboardURL(existing.ID), nil
}

// EnsureAlerts creates or updates human-facing threshold alerts. The
// notification channel must already exist; this installer never creates one.
func (i *Installer) EnsureAlerts(ctx context.Context, channel string) error {
	if strings.TrimSpace(channel) == "" {
		return fmt.Errorf("notification channel name is required")
	}
	if _, err := i.resolveChannel(ctx, channel); err != nil {
		return err
	}

	dashboardURL, err := i.EnsureDashboard(ctx)
	if err != nil {
		return err
	}

	existing, err := i.listRules(ctx)
	if err != nil {
		return err
	}
	byName := make(map[string]ruleSummary, len(existing))
	for _, rule := range existing {
		byName[rule.Alert] = rule
	}

	for _, spec := range i.alertDefinitions(channel, dashboardURL) {
		if current, ok := byName[spec.Alert]; ok {
			if alertThresholdEqual(current, spec) {
				continue
			}
			if err := i.updateRule(ctx, current.ID, spec); err != nil {
				return fmt.Errorf("update alert %q: %w", spec.Alert, err)
			}
			continue
		}
		if err := i.createRule(ctx, spec); err != nil {
			return fmt.Errorf("create alert %q: %w", spec.Alert, err)
		}
	}
	return nil
}

func dashboardTitle(cfg config.Config) string {
	return fmt.Sprintf("Incident Autopilot — %s", cfg.Target.Service)
}

func dashboardResourceName(cfg config.Config) string {
	raw := fmt.Sprintf("incident-autopilot-%s", cfg.Target.Service)
	raw = strings.ToLower(raw)
	raw = strings.ReplaceAll(raw, "_", "-")
	if rfc1123NamePattern.MatchString(raw) {
		return raw
	}
	return "incident-autopilot"
}

func (i *Installer) dashboardURL(id string) string {
	return i.baseURL + "/dashboard/" + id
}

func (i *Installer) metricSelector() string {
	return fmt.Sprintf(
		`service="%s",namespace="%s",deployment="%s"`,
		i.cfg.Target.Service,
		i.cfg.Target.Namespace,
		i.cfg.Target.Deployment,
	)
}

func (i *Installer) buildDashboard(title string) map[string]any {
	panels := map[string]any{
		widgetReplicas:           i.timeSeriesPanel("Current vs recommended replicas", []string{
			fmt.Sprintf(`autopilot_current_replicas{%s}`, i.metricSelector()),
			fmt.Sprintf(`autopilot_recommended_replicas{%s}`, i.metricSelector()),
		}),
		widgetRequestRate:        i.timeSeriesPanel("Request rate", []string{i.cfg.Signals.RequestRateQuery}),
		widgetP95Latency:         i.timeSeriesPanel("P95 latency", []string{i.cfg.Signals.P95LatencyQuery}),
		widgetErrorRate:          i.timeSeriesPanel("Error rate", []string{i.cfg.Signals.ErrorRateQuery}),
		widgetSLI:                i.timeSeriesPanel("SLI", []string{i.cfg.Signals.SLIQuery}),
		widgetPendingApproval:    i.timeSeriesPanel("Pending approvals", []string{
			fmt.Sprintf(`autopilot_pending_approval{%s}`, i.metricSelector()),
		}),
		widgetTelemetryFreshness: i.timeSeriesPanel("Telemetry freshness (seconds)", []string{
			fmt.Sprintf(`autopilot_telemetry_freshness_seconds{%s}`, i.metricSelector()),
		}),
		widgetDecisionsByReason: i.timeSeriesPanel("Decision count by reason", []string{
			fmt.Sprintf(`sum by (decision) (autopilot_decision_total{%s})`, i.metricSelector()),
		}),
		widgetScalingActions: i.timeSeriesPanel("Scaling actions", []string{
			fmt.Sprintf(`sum by (decision) (increase(autopilot_decision_total{%s,decision=~"scale_up|scale_down"}[5m]))`, i.metricSelector()),
		}),
		widgetQuarantineActions: i.timeSeriesPanel("Quarantine actions", []string{
			fmt.Sprintf(`sum(increase(autopilot_decision_total{%s,decision="quarantine_replace"}[5m]))`, i.metricSelector()),
		}),
		widgetVerification: i.timeSeriesPanel("Pre/post verification SLI", []string{
			i.cfg.Signals.SLIQuery,
		}),
		widgetPodOutliers: i.timeSeriesPanel("Per-pod outlier error rate", []string{i.podOutlierQuery()}),
	}

	layoutItems := []map[string]any{
		gridItem(widgetReplicas, 0, 0, 6, 8),
		gridItem(widgetPendingApproval, 6, 0, 6, 8),
		gridItem(widgetRequestRate, 0, 8, 6, 8),
		gridItem(widgetP95Latency, 6, 8, 6, 8),
		gridItem(widgetErrorRate, 0, 16, 6, 8),
		gridItem(widgetSLI, 6, 16, 6, 8),
		gridItem(widgetTelemetryFreshness, 0, 24, 6, 8),
		gridItem(widgetDecisionsByReason, 6, 24, 6, 8),
		gridItem(widgetScalingActions, 0, 32, 6, 8),
		gridItem(widgetQuarantineActions, 6, 32, 6, 8),
		gridItem(widgetVerification, 0, 40, 6, 8),
		gridItem(widgetPodOutliers, 6, 40, 6, 8),
	}

	return map[string]any{
		"generateName":  true,
		"schemaVersion": dashboardSchemaVersion,
		"tags":          []any{},
		"spec": map[string]any{
			"display": map[string]any{
				"name":        title,
				"description": "SigNoz Incident Autopilot controller health, decisions, and verification.",
			},
			"variables": []any{},
			"panels":    panels,
			"layouts": []any{
				map[string]any{
					"kind": "Grid",
					"spec": map[string]any{
						"items": layoutItems,
					},
				},
			},
		},
	}
}

func (i *Installer) podOutlierQuery() string {
	service := i.cfg.Target.Service
	return fmt.Sprintf(
		`sum by (k8s_pod_name) (rate(checkout_requests_total{service_name=%q,status="failed"}[5m])) / sum by (k8s_pod_name) (rate(checkout_requests_total{service_name=%q}[5m]))`,
		service, service,
	)
}

func gridItem(panelID string, x, y, width, height int) map[string]any {
	return map[string]any{
		"x": x,
		"y": y,
		"width":  width,
		"height": height,
		"content": map[string]any{
			"$ref": "#/spec/panels/" + panelID,
		},
	}
}

func (i *Installer) timeSeriesPanel(title string, queries []string) map[string]any {
	querySpecs := make([]map[string]any, 0, len(queries))
	for idx, query := range queries {
		name := string(rune('A' + idx))
		querySpecs = append(querySpecs, map[string]any{
			"type": "promql",
			"spec": map[string]any{
				"name":  name,
				"query": query,
			},
		})
	}
	return map[string]any{
		"kind": "Panel",
		"spec": map[string]any{
			"display": map[string]any{"name": title},
			"plugin": map[string]any{
				"kind": "signoz/TimeSeriesPanel",
				"spec": map[string]any{
					"visualization": map[string]any{
						"timePreference": "global_time",
					},
				},
			},
			"queries": []any{
				map[string]any{
					"kind": "time_series",
					"spec": map[string]any{
						"plugin": map[string]any{
							"kind": "signoz/CompositeQuery",
							"spec": map[string]any{
								"queries": querySpecs,
							},
						},
					},
				},
			},
		},
	}
}

type alertSpec struct {
	Alert     string
	AlertType string
	RuleType  string
	Version   string
	Condition map[string]any
	Labels    map[string]string
	Annot     map[string]string
	Eval      map[string]any
}

func (i *Installer) alertDefinitions(channel, dashboardURL string) []alertSpec {
	approvalURL := i.approvalURL + "/actions/latest"
	annotations := func(summary string) map[string]string {
		return map[string]string{
			"summary":     summary,
			"description": fmt.Sprintf("%s Review the dashboard at %s and approve at %s.", summary, dashboardURL, approvalURL),
		}
	}
	freshnessLimit := int(i.cfg.Signals.FreshnessLimit.Seconds())
	if freshnessLimit <= 0 {
		freshnessLimit = 60
	}
	maxReplicas := i.cfg.Policy.MaxReplicas
	sliObjective := i.cfg.Signals.SLIObjective

	return []alertSpec{
		i.promqlThresholdAlert(
			"Autopilot pending approval",
			fmt.Sprintf(`autopilot_pending_approval{%s}`, i.metricSelector()),
			0, "above", channel,
			annotations("A scaling or remediation recommendation is awaiting operator approval."),
		),
		i.heartbeatAbsentAlert(channel, annotations("Incident Autopilot has not emitted a heartbeat for two minutes.")),
		i.promqlThresholdAlert(
			"Autopilot stale telemetry",
			fmt.Sprintf(`autopilot_telemetry_freshness_seconds{%s}`, i.metricSelector()),
			float64(freshnessLimit), "above", channel,
			annotations("Incident Autopilot is using stale telemetry and will not execute actions."),
		),
		i.rolloutFailureAlert(channel, annotations("Incident Autopilot reported a rollout or remediation failure.")),
		i.maxReplicasUnhealthySLIAlert(channel, maxReplicas, sliObjective, annotations(
			fmt.Sprintf("Maximum replicas (%d) reached while SLI remains below the %.2f objective.", maxReplicas, sliObjective),
		)),
	}
}

func (i *Installer) promqlThresholdAlert(name, query string, target float64, op, channel string, annotations map[string]string) alertSpec {
	return alertSpec{
		Alert:     name,
		AlertType: "METRIC_BASED_ALERT",
		RuleType:  "promql_rule",
		Version:   alertAPIVersion,
		Labels: map[string]string{
			"service": i.cfg.Target.Service,
			"team":    "incident-autopilot",
		},
		Annot: annotations,
		Eval: map[string]any{
			"kind": "rolling",
			"spec": map[string]any{
				"frequency":  "1m",
				"evalWindow": "5m",
			},
		},
		Condition: map[string]any{
			"compositeQuery": map[string]any{
				"panelType": "graph",
				"queryType": "promql",
				"queries": []any{
					map[string]any{
						"type": "promql",
						"spec": map[string]any{
							"name":  "A",
							"query": query,
						},
					},
				},
			},
			"selectedQueryName": "A",
			"thresholds": map[string]any{
				"kind": "basic",
				"spec": []any{
					map[string]any{
						"name":      "critical",
						"target":    target,
						"op":        op,
						"matchType": "at_least_once",
						"channels":  []string{channel},
					},
				},
			},
		},
	}
}

func (i *Installer) heartbeatAbsentAlert(channel string, annotations map[string]string) alertSpec {
	return alertSpec{
		Alert:     "Autopilot controller heartbeat missing",
		AlertType: "METRIC_BASED_ALERT",
		RuleType:  "promql_rule",
		Version:   alertAPIVersion,
		Labels: map[string]string{
			"service": i.cfg.Target.Service,
			"team":    "incident-autopilot",
		},
		Annot: annotations,
		Eval: map[string]any{
			"kind": "rolling",
			"spec": map[string]any{
				"frequency":  "1m",
				"evalWindow": "5m",
			},
		},
		Condition: map[string]any{
			"absentFor":     120,
			"alertOnAbsent": true,
			"compositeQuery": map[string]any{
				"panelType": "graph",
				"queryType": "promql",
				"queries": []any{
					map[string]any{
						"type": "promql",
						"spec": map[string]any{
							"name":  "A",
							"query": fmt.Sprintf(`autopilot_heartbeat{%s}`, i.metricSelector()),
						},
					},
				},
			},
			"selectedQueryName": "A",
			"thresholds": map[string]any{
				"kind": "basic",
				"spec": []any{
					map[string]any{
						"name":      "critical",
						"target":    0,
						"op":        "above",
						"matchType": "at_least_once",
						"channels":  []string{channel},
					},
				},
			},
		},
	}
}

func (i *Installer) rolloutFailureAlert(channel string, annotations map[string]string) alertSpec {
	return alertSpec{
		Alert:     "Autopilot rollout or remediation failure",
		AlertType: "LOGS_BASED_ALERT",
		RuleType:  "threshold_rule",
		Version:   alertAPIVersion,
		Labels: map[string]string{
			"service": i.cfg.Target.Service,
			"team":    "incident-autopilot",
		},
		Annot: annotations,
		Eval: map[string]any{
			"kind": "rolling",
			"spec": map[string]any{
				"frequency":  "1m",
				"evalWindow": "5m",
			},
		},
		Condition: map[string]any{
			"compositeQuery": map[string]any{
				"panelType": "graph",
				"queryType": "builder",
				"queries": []any{
					map[string]any{
						"type": "builder_query",
						"spec": map[string]any{
							"name":   "A",
							"signal": "logs",
							"filter": map[string]any{
								"expression": fmt.Sprintf(
									`service.name = '%s' AND event.name = 'autopilot.incident_report' AND autopilot.result IN ['rollout_failed', 'ineffective']`,
									i.cfg.Target.Service,
								),
							},
							"aggregations": []any{
								map[string]any{"expression": "count()"},
							},
							"stepInterval": 60,
						},
					},
				},
			},
			"selectedQueryName": "A",
			"thresholds": map[string]any{
				"kind": "basic",
				"spec": []any{
					map[string]any{
						"name":      "critical",
						"target":    0,
						"op":        "above",
						"matchType": "at_least_once",
						"channels":  []string{channel},
					},
				},
			},
		},
	}
}

func (i *Installer) maxReplicasUnhealthySLIAlert(channel string, maxReplicas int32, sliObjective float64, annotations map[string]string) alertSpec {
	query := fmt.Sprintf(
		`(autopilot_current_replicas{%s} >= %d) and on() ((%s) < %g)`,
		i.metricSelector(), maxReplicas, i.cfg.Signals.SLIQuery, sliObjective,
	)
	return i.promqlThresholdAlert(
		"Autopilot max replicas with unhealthy SLI",
		query,
		0,
		"above",
		channel,
		annotations,
	)
}

type ruleSummary struct {
	ID        string
	Alert     string
	Condition map[string]any
}

func alertThresholdEqual(existing ruleSummary, desired alertSpec) bool {
	existingSpec := extractThresholdEntries(existing.Condition)
	desiredSpec := extractThresholdEntries(desired.Condition)
	if len(existingSpec) != len(desiredSpec) {
		return false
	}
	for idx := range desiredSpec {
		if existingSpec[idx].Target != desiredSpec[idx].Target || existingSpec[idx].Op != desiredSpec[idx].Op {
			return false
		}
	}
	return true
}

type thresholdEntry struct {
	Target float64
	Op     string
}

func extractThresholdEntries(condition map[string]any) []thresholdEntry {
	thresholds, ok := condition["thresholds"].(map[string]any)
	if !ok {
		return nil
	}
	rawSpec, ok := thresholds["spec"].([]any)
	if !ok {
		return nil
	}
	out := make([]thresholdEntry, 0, len(rawSpec))
	for _, item := range rawSpec {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		target, _ := entry["target"].(float64)
		op, _ := entry["op"].(string)
		out = append(out, thresholdEntry{Target: target, Op: op})
	}
	return out
}

func (i *Installer) findDashboardByTitle(ctx context.Context, title string) (dashboardRef, error) {
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Dashboards []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Spec struct {
					Display struct {
						Name string `json:"name"`
					} `json:"display"`
				} `json:"spec"`
			} `json:"dashboards"`
		} `json:"data"`
	}
	if err := i.getJSON(ctx, "/api/v2/dashboards", &resp); err != nil {
		return dashboardRef{}, err
	}
	for _, dash := range resp.Data.Dashboards {
		displayName := dash.Spec.Display.Name
		if displayName == "" {
			displayName = dash.Name
		}
		if displayName == title {
			resourceName := dash.Name
			if resourceName == "" {
				resourceName = dashboardResourceName(i.cfg)
			}
			return dashboardRef{ID: dash.ID, Name: resourceName}, nil
		}
	}
	return dashboardRef{}, nil
}

func (i *Installer) createDashboard(ctx context.Context, payload map[string]any) (string, error) {
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := i.postJSON(ctx, "/api/v2/dashboards", payload, http.StatusCreated, &resp); err != nil {
		return "", err
	}
	return resp.Data.ID, nil
}

func (i *Installer) updateDashboard(ctx context.Context, existing dashboardRef, payload map[string]any) error {
	update := map[string]any{
		"schemaVersion": payload["schemaVersion"],
		"name":          existing.Name,
		"tags":          payload["tags"],
		"spec":          payload["spec"],
	}
	return i.putJSON(ctx, "/api/v2/dashboards/"+existing.ID, update, http.StatusOK, nil)
}

func (i *Installer) resolveChannel(ctx context.Context, name string) (string, error) {
	var resp struct {
		Status string `json:"status"`
		Data   []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := i.getJSON(ctx, "/api/v1/channels", &resp); err != nil {
		return "", err
	}
	for _, ch := range resp.Data {
		if ch.Name == name {
			return ch.ID, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrChannelNotFound, name)
}

func (i *Installer) listRules(ctx context.Context) ([]ruleSummary, error) {
	var resp struct {
		Status string `json:"status"`
		Data   []struct {
			ID        string         `json:"id"`
			Alert     string         `json:"alert"`
			Condition map[string]any `json:"condition"`
		} `json:"data"`
	}
	if err := i.getJSON(ctx, "/api/v2/rules", &resp); err != nil {
		return nil, err
	}
	out := make([]ruleSummary, 0, len(resp.Data))
	for _, rule := range resp.Data {
		out = append(out, ruleSummary{
			ID:        rule.ID,
			Alert:     rule.Alert,
			Condition: rule.Condition,
		})
	}
	return out, nil
}

func (i *Installer) createRule(ctx context.Context, spec alertSpec) error {
	payload := i.rulePayload(spec)
	return i.postJSON(ctx, "/api/v2/rules", payload, http.StatusCreated, nil)
}

func (i *Installer) updateRule(ctx context.Context, id string, spec alertSpec) error {
	payload := i.rulePayload(spec)
	return i.putJSON(ctx, "/api/v2/rules/"+id, payload, http.StatusOK, nil)
}

func (i *Installer) rulePayload(spec alertSpec) map[string]any {
	return map[string]any{
		"alert":         spec.Alert,
		"alertType":     spec.AlertType,
		"ruleType":      spec.RuleType,
		"schemaVersion": alertSchemaVersion,
		"version":       spec.Version,
		"labels":        spec.Labels,
		"annotations":   spec.Annot,
		"evaluation":    spec.Eval,
		"condition":     spec.Condition,
		"disabled":      false,
		"notificationSettings": map[string]any{
			"usePolicy": false,
		},
		"preferredChannels": []string{},
	}
}

func (i *Installer) getJSON(ctx context.Context, path string, out any) error {
	return i.doJSON(ctx, http.MethodGet, path, nil, http.StatusOK, out)
}

func (i *Installer) postJSON(ctx context.Context, path string, body any, okStatus int, out any) error {
	return i.doJSON(ctx, http.MethodPost, path, body, okStatus, out)
}

func (i *Installer) putJSON(ctx context.Context, path string, body any, okStatus int, out any) error {
	return i.doJSON(ctx, http.MethodPut, path, body, okStatus, out)
}

func (i *Installer) doJSON(ctx context.Context, method, path string, body any, okStatus int, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, i.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if i.apiKey != "" {
		req.Header.Set("SIGNOZ-API-KEY", i.apiKey)
	}

	resp, err := i.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != okStatus {
		return fmt.Errorf("request %s %s returned status %d: %s", method, path, resp.StatusCode, string(respBody))
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
