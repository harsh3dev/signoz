import { NodeSDK } from '@opentelemetry/sdk-node';
import { getNodeAutoInstrumentations } from '@opentelemetry/auto-instrumentations-node';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-http';
import { OTLPMetricExporter } from '@opentelemetry/exporter-metrics-otlp-http';
import { OTLPLogExporter } from '@opentelemetry/exporter-logs-otlp-http';
import { SimpleLogRecordProcessor, LoggerProvider } from '@opentelemetry/sdk-logs';
import { logs } from '@opentelemetry/api-logs';
import { PeriodicExportingMetricReader } from '@opentelemetry/sdk-metrics';
import { Resource } from '@opentelemetry/resources';
import dotenv from 'dotenv';

dotenv.config();

const signozEndpoint = process.env.SIGNOZ_OTLP_ENDPOINT || 'http://localhost:14318';

const resource = new Resource({
  'service.name': process.env.OTEL_SERVICE_NAME || 'telemetry-shop-api',
  'service.version': '1.0.0',
  'deployment.environment': process.env.OTEL_DEPLOYMENT_ENVIRONMENT || 'local',
  'k8s.namespace.name': process.env.K8S_NAMESPACE_NAME || 'local',
  'k8s.deployment.name': process.env.K8S_DEPLOYMENT_NAME || 'local',
  'k8s.pod.name': process.env.K8S_POD_NAME || 'local',
  'k8s.pod.uid': process.env.K8S_POD_UID || 'local',
});

// Trace Exporter
const traceExporter = new OTLPTraceExporter({
  url: `${signozEndpoint}/v1/traces`,
});

// Metric Exporter and Reader
const metricExporter = new OTLPMetricExporter({
  url: `${signozEndpoint}/v1/metrics`,
});
const metricReader = new PeriodicExportingMetricReader({
  exporter: metricExporter,
  exportIntervalMillis: 10000, // Export every 10s
});

// Log Exporter
const logExporter = new OTLPLogExporter({
  url: `${signozEndpoint}/v1/logs`,
});

// Set global logger provider for winston transport and manual logging
const loggerProvider = new LoggerProvider({ resource });
loggerProvider.addLogRecordProcessor(new SimpleLogRecordProcessor(logExporter));
logs.setGlobalLoggerProvider(loggerProvider);

// Node SDK Initialization
const sdk = new NodeSDK({
  resource,
  traceExporter,
  metricReader,
  logRecordProcessor: new SimpleLogRecordProcessor(logExporter),
  instrumentations: [
    getNodeAutoInstrumentations({
      '@opentelemetry/instrumentation-express': {
        enabled: true,
      },
      '@opentelemetry/instrumentation-http': {
        enabled: true,
      },
    }),
  ],
});

sdk.start();

console.log('OpenTelemetry Node SDK initialized successfully.');

// Graceful shutdown
process.on('SIGTERM', () => {
  sdk.shutdown()
    .then(() => console.log('SDK shut down successfully.'))
    .catch((error) => console.log('Error shutting down SDK', error))
    .finally(() => process.exit(0));
});
export default sdk;
export { loggerProvider };
