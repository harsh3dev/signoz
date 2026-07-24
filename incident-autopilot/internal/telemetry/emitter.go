// Package telemetry exposes the Incident Autopilot controller's own
// decisions as both a Prometheus endpoint (for KEDA to scrape the
// desired-replica signal) and OTLP metrics/logs (for the SigNoz dashboard
// and incident reports). Labels stay bounded to service/namespace/deployment
// and decision metadata; full explanations go into structured logs only.
package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/config"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/model"
)

var labelNames = []string{"service", "namespace", "deployment"}

type Emitter struct {
	cfg config.Config

	registry            *prometheus.Registry
	recommendedReplicas *prometheus.GaugeVec
	currentReplicas     *prometheus.GaugeVec
	pendingApproval     *prometheus.GaugeVec
	decisionTotal       *prometheus.CounterVec
	freshnessSeconds    *prometheus.GaugeVec
	heartbeat           prometheus.Gauge

	meter           metric.Meter
	otelRecommended metric.Float64Gauge
	otelCurrent     metric.Float64Gauge
	logger          otellog.Logger
}

// New builds an Emitter. meter and logger may be nil (e.g. in tests, or when
// OTLP export is disabled); Prometheus metrics are always recorded.
func New(cfg config.Config, meter metric.Meter, logger otellog.Logger) (*Emitter, error) {
	registry := prometheus.NewRegistry()
	e := &Emitter{
		cfg:      cfg,
		registry: registry,
		recommendedReplicas: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "autopilot_recommended_replicas",
			Help: "Replica count the Incident Autopilot controller has published for KEDA to scale toward.",
		}, labelNames),
		currentReplicas: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "autopilot_current_replicas",
			Help: "Replica count observed for the target deployment.",
		}, labelNames),
		pendingApproval: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "autopilot_pending_approval",
			Help: "1 if a recommendation is awaiting operator approval, 0 otherwise.",
		}, labelNames),
		decisionTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "autopilot_decision_total",
			Help: "Count of policy decisions by type and policy version.",
		}, append(append([]string{}, labelNames...), "decision", "policy_version")),
		freshnessSeconds: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "autopilot_telemetry_freshness_seconds",
			Help: "Seconds between the most recent complete telemetry point and the decision that used it.",
		}, labelNames),
		heartbeat: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "autopilot_heartbeat",
			Help: "Unix timestamp of the controller's last completed evaluation loop.",
		}),
		meter:  meter,
		logger: logger,
	}

	collectors := []prometheus.Collector{
		e.recommendedReplicas, e.currentReplicas, e.pendingApproval,
		e.decisionTotal, e.freshnessSeconds, e.heartbeat,
	}
	for _, collector := range collectors {
		if err := registry.Register(collector); err != nil {
			return nil, err
		}
	}

	if meter != nil {
		var err error
		if e.otelRecommended, err = meter.Float64Gauge("autopilot_recommended_replicas"); err != nil {
			return nil, err
		}
		if e.otelCurrent, err = meter.Float64Gauge("autopilot_current_replicas"); err != nil {
			return nil, err
		}
	}

	return e, nil
}

// Handler serves Prometheus exposition format for scraping.
func (e *Emitter) Handler() http.Handler {
	return promhttp.HandlerFor(e.registry, promhttp.HandlerOpts{})
}

// InstantQueryHandler implements just enough of Prometheus's HTTP query API
// (GET /api/v1/query?query=<metric_name>) for KEDA's "prometheus" trigger to
// read a single gauge by name, without running a separate Prometheus server.
// It does not evaluate PromQL; query must be an exact registered metric name.
func (e *Emitter) InstantQueryHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		families, err := e.registry.Gather()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		type sample struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		}
		result := []sample{}
		now := float64(time.Now().Unix())

		for _, family := range families {
			if family.GetName() != query {
				continue
			}
			for _, m := range family.GetMetric() {
				value := 0.0
				switch {
				case m.GetGauge() != nil:
					value = m.GetGauge().GetValue()
				case m.GetCounter() != nil:
					value = m.GetCounter().GetValue()
				}
				labels := make(map[string]string, len(m.GetLabel()))
				for _, l := range m.GetLabel() {
					labels[l.GetName()] = l.GetValue()
				}
				result = append(result, sample{
					Metric: labels,
					Value:  [2]any{now, strconv.FormatFloat(value, 'f', -1, 64)},
				})
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result":     result,
			},
		})
	})
}

func (e *Emitter) labelValues() []string {
	return []string{e.cfg.Target.Service, e.cfg.Target.Namespace, e.cfg.Target.Deployment}
}

// RecordRecommendation only reflects whether a scaling recommendation is
// awaiting operator approval. It intentionally does NOT set
// autopilot_recommended_replicas: that metric drives KEDA directly, so it
// must only change through RecordPublishedReplicas, once the controller's
// mode gating (dry-run/approval/automatic) has actually decided to act.
func (e *Emitter) RecordRecommendation(rec model.Recommendation) {
	pending := 0.0
	isScale := rec.Decision == model.DecisionScaleUp || rec.Decision == model.DecisionScaleDown
	if isScale && e.cfg.Controller.Mode == "approval" {
		pending = 1.0
	}
	e.pendingApproval.WithLabelValues(e.labelValues()...).Set(pending)
}

// RecordPublishedReplicas sets autopilot_recommended_replicas, the metric
// KEDA's ScaledObject reads. Callers must only pass the approval-gated
// published replica count, never the raw policy recommendation.
func (e *Emitter) RecordPublishedReplicas(replicas int32) {
	e.recommendedReplicas.WithLabelValues(e.labelValues()...).Set(float64(replicas))
	if e.meter != nil {
		e.otelRecommended.Record(context.Background(), float64(replicas))
	}
}

// RecordObservedReplicas sets autopilot_current_replicas: the replica count
// actually observed on the target Deployment right now, for comparison
// against the published/recommended value on dashboards.
func (e *Emitter) RecordObservedReplicas(replicas int32) {
	e.currentReplicas.WithLabelValues(e.labelValues()...).Set(float64(replicas))
	if e.meter != nil {
		e.otelCurrent.Record(context.Background(), float64(replicas))
	}
}

func (e *Emitter) RecordDecision(rec model.Recommendation) {
	values := append(append([]string{}, e.labelValues()...), string(rec.Decision), rec.PolicyVersion)
	e.decisionTotal.WithLabelValues(values...).Inc()

	if !rec.CreatedAt.IsZero() {
		e.freshnessSeconds.WithLabelValues(e.labelValues()...).Set(time.Since(rec.CreatedAt).Seconds())
	}

	if e.logger == nil {
		return
	}
	var record otellog.Record
	record.SetTimestamp(rec.CreatedAt)
	record.SetSeverity(otellog.SeverityInfo)
	record.SetBody(otellog.StringValue(rec.Reason))
	record.AddAttributes(
		otellog.String("autopilot.recommendation_id", rec.ID),
		otellog.String("autopilot.decision", string(rec.Decision)),
		otellog.Int64("autopilot.current_replicas", int64(rec.CurrentReplicas)),
		otellog.Int64("autopilot.recommended_replicas", int64(rec.RecommendedReplicas)),
		otellog.String("autopilot.target_pod", rec.TargetPod),
		otellog.Float64("autopilot.confidence", rec.Confidence),
		otellog.String("autopilot.policy_version", rec.PolicyVersion),
	)
	e.logger.Emit(context.Background(), record)
}

func (e *Emitter) Heartbeat() {
	e.heartbeat.Set(float64(time.Now().Unix()))
}
