package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otellog "go.opentelemetry.io/otel/log"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Providers bundles the OTLP-backed OpenTelemetry providers used to export
// the controller's own decisions and metrics to SigNoz, alongside a
// shutdown function that flushes and closes both exporters.
type Providers struct {
	Meter    otelmetric.Meter
	Logger   otellog.Logger
	Shutdown func(context.Context) error
}

// NewProviders configures OTLP/HTTP exporters for metrics and logs pointed
// at endpoint (e.g. "http://signoz:4318") and tags every record with the
// autopilot's own service identity.
func NewProviders(ctx context.Context, endpoint, serviceName string) (*Providers, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	metricExporter, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(endpoint+"/v1/metrics"))
	if err != nil {
		return nil, fmt.Errorf("build metric exporter: %w", err)
	}
	meterProvider := metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(metricExporter)),
	)

	logExporter, err := otlploghttp.New(ctx, otlploghttp.WithEndpointURL(endpoint+"/v1/logs"))
	if err != nil {
		return nil, fmt.Errorf("build log exporter: %w", err)
	}
	loggerProvider := log.NewLoggerProvider(
		log.WithResource(res),
		log.WithProcessor(log.NewBatchProcessor(logExporter)),
	)

	return &Providers{
		Meter:  meterProvider.Meter("incident-autopilot"),
		Logger: loggerProvider.Logger("incident-autopilot"),
		Shutdown: func(ctx context.Context) error {
			if err := meterProvider.Shutdown(ctx); err != nil {
				return fmt.Errorf("shutdown meter provider: %w", err)
			}
			if err := loggerProvider.Shutdown(ctx); err != nil {
				return fmt.Errorf("shutdown logger provider: %w", err)
			}
			return nil
		},
	}, nil
}
