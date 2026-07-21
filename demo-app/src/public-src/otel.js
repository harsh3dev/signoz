import { WebTracerProvider } from '@opentelemetry/sdk-trace-web';
import { SimpleSpanProcessor } from '@opentelemetry/sdk-trace-base';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-http';
import { registerInstrumentations } from '@opentelemetry/instrumentation';
import { FetchInstrumentation } from '@opentelemetry/instrumentation-fetch';
import { ZoneContextManager } from '@opentelemetry/context-zone';
import { Resource } from '@opentelemetry/resources';

// Setup browser resources
const resource = new Resource({
  'service.name': 'telemetry-shop-web',
  'deployment.environment': 'local',
});

// Configure OTLP trace exporter to go through our Express proxy (same origin)
const traceExporter = new OTLPTraceExporter({
  url: `${window.location.origin}/signoz/otel/v1/traces`,
});

const provider = new WebTracerProvider({
  resource,
});

// For browser demo environments, SimpleSpanProcessor is standard and fast
provider.addSpanProcessor(new SimpleSpanProcessor(traceExporter));

// Register context manager and auto fetch instrumentation
provider.register({
  contextManager: new ZoneContextManager(),
});

registerInstrumentations({
  instrumentations: [
    new FetchInstrumentation({
      // Automatically add W3C traceparent headers to matching API requests
      propagateTraceHeaderCorsUrls: [
        new RegExp(`^${window.location.origin}/api/.*`),
      ],
    }),
  ],
});

console.log('OpenTelemetry Browser SDK initialized. Target path: /signoz/otel/v1/traces');
