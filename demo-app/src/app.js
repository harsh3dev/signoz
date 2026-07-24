import express from 'express';
import cors from 'cors';
import cookieParser from 'cookie-parser';
import dotenv from 'dotenv';
import fs from 'fs';
import path from 'path';
import { fileURLToPath } from 'url';
import { trace, metrics, SpanStatusCode } from '@opentelemetry/api';
import logger from './utils/logger.js';

dotenv.config();

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

const app = express();
app.disable('x-powered-by');
const PORT = process.env.PORT || 3000;
const signozEndpoint = process.env.SIGNOZ_OTLP_ENDPOINT || 'http://localhost:14318';

// Global mock databases/state
let products = [];
try {
  const rawProducts = fs.readFileSync(path.join(__dirname, 'data', 'products.json'), 'utf8');
  products = JSON.parse(rawProducts);
} catch (err) {
  logger.error('Failed to load products database. Initializing with empty list.', { error: err.message });
}

const ordersDb = new Map();

// Chaos dynamic configuration (in-memory state)
let chaosConfig = {
  delayMs: 0,        // dynamic processing latency injection
  errorRate: 0,      // % chance of random checkout failures (0-100)
  enabled: process.env.CHAOS_ENABLED !== 'false',
};

// Deterministic pod-local behavior, controlled via /api/demo/behavior.
// Used by the Incident Autopilot demo to drive reproducible capacity and
// bad-pod scenarios, independent of the random chaos config above.
const podBehavior = {
  ready: true,
  inventoryDelayMs: Number(process.env.INVENTORY_DELAY_MS || 0),
  inventoryErrorRate: Number(process.env.INVENTORY_ERROR_RATE || 0),
};

// OpenTelemetry Metrics Setup
const meter = metrics.getMeter('telemetry-shop-meter');

const ordersCreatedCounter = meter.createCounter('orders.created', {
  description: 'Total number of orders created',
});

const checkoutDurationHistogram = meter.createHistogram('checkout.duration', {
  description: 'Duration of the checkout flow',
  unit: 'ms',
});

const paymentFailuresCounter = meter.createCounter('payment.failures', {
  description: 'Total number of payment failures due to inventory or billing issues',
});

const customEventsCounter = meter.createCounter('demo.custom_events', {
  description: 'Custom manual button clicks or user telemetry events',
});

// Underscore-named metrics consumed directly by the Incident Autopilot
// controller's Prometheus queries (see incident-autopilot/config.example.yaml).
const checkoutRequestsCounter = meter.createCounter('checkout_requests_total', {
  description: 'Checkout requests classified by outcome',
});

const checkoutDurationMilliseconds = meter.createHistogram('checkout_duration_milliseconds', {
  description: 'Checkout duration in milliseconds',
  unit: 'ms',
});

// Middleware
app.use(cors());
app.use(cookieParser());
app.use(express.json());

// Express Static Hosting
app.use(express.static(path.join(__dirname, '../public')));

// Tracer for manual instrumentation
const tracer = trace.getTracer('telemetry-shop-tracer');

// Chaos latency and error injection middleware
app.use((req, res, next) => {
  if (!chaosConfig.enabled) {
    return next();
  }

  // Latency injection (check for header X-Chaos-Delay first, then global config)
  let delay = 0;
  if (req.headers['x-chaos-delay']) {
    delay = parseInt(req.headers['x-chaos-delay'], 10) || 0;
  } else if (chaosConfig.delayMs > 0) {
    delay = chaosConfig.delayMs;
  }

  if (delay > 0) {
    const activeSpan = trace.getActiveSpan();
    if (activeSpan) {
      activeSpan.setAttribute('chaos.delay_applied_ms', delay);
    }
    logger.info(`Injecting chaos latency of ${delay}ms`, { path: req.path });
    setTimeout(next, delay);
  } else {
    next();
  }
});

// --- API Routes ---

// 1. Health Endpoint
app.get('/api/health', (req, res) => {
  logger.info('Liveness check requested');
  res.status(200).json({ status: 'healthy', timestamp: new Date().toISOString() });
});

// 1b. Kubernetes-style liveness/readiness probes and deterministic behavior
// control, used by the Incident Autopilot demo scenarios.
app.get('/api/health/live', (_req, res) => {
  res.status(200).json({ status: 'alive' });
});

app.get('/api/health/ready', (_req, res) => {
  res.status(podBehavior.ready ? 200 : 503).json({
    status: podBehavior.ready ? 'ready' : 'not_ready',
    pod: process.env.K8S_POD_NAME || 'local',
  });
});

app.post('/api/demo/behavior', (req, res) => {
  const { ready, inventoryDelayMs, inventoryErrorRate } = req.body;
  if (typeof ready === 'boolean') podBehavior.ready = ready;
  if (Number.isFinite(inventoryDelayMs)) podBehavior.inventoryDelayMs = inventoryDelayMs;
  if (Number.isFinite(inventoryErrorRate)) podBehavior.inventoryErrorRate = inventoryErrorRate;
  logger.info('Deterministic pod behavior updated', podBehavior);
  res.json(podBehavior);
});

// 2. Fetch Products
app.get('/api/products', (req, res) => {
  logger.info('Fetching products list', { count: products.length });
  res.status(200).json(products);
});

// 3. Create Order (Checkout flow with multi-step spans)
app.post('/api/orders', async (req, res) => {
  const startTime = Date.now();
  const { items, customerName, shippingAddress } = req.body;

  if (!items || !Array.isArray(items) || items.length === 0) {
    logger.warn('Order creation failed due to empty cart validation');
    return res.status(400).json({ error: 'Cart must contain at least one product.' });
  }

  logger.info('Starting checkout process', { itemsCount: items.length, customer: customerName });

  // Custom tracing for multi-step checkout
  const currentSpan = trace.getActiveSpan();
  if (currentSpan) {
    currentSpan.setAttribute('order.customer_name', customerName);
    currentSpan.setAttribute('order.items_count', items.length);
  }

  try {
    let orderTotal = 0;

    // Step 1: Validate items (Local database scan simulation)
    const validatedItems = await tracer.startActiveSpan('validateCart', async (span) => {
      try {
        const results = [];
        for (const orderItem of items) {
          const product = products.find(p => p.id === orderItem.id);
          if (!product) {
            const err = new Error(`Product with ID ${orderItem.id} does not exist.`);
            span.setStatus({ code: SpanStatusCode.ERROR, message: err.message });
            throw err;
          }
          const quantity = parseInt(orderItem.quantity, 10) || 1;
          const validated = { ...product, quantity, subtotal: product.price * quantity };
          orderTotal += validated.subtotal;
          results.push(validated);
        }
        span.setAttribute('cart.validation', 'success');
        span.setAttribute('cart.calculated_total_cents', orderTotal);
        logger.info('Cart validated successfully', { total: orderTotal });
        return results;
      } finally {
        span.end();
      }
    });

    // Step 2: Reserve Inventory
    await tracer.startActiveSpan('reserveInventory', async (span) => {
      try {
        span.setAttribute('inventory.count', validatedItems.length);

        // Deterministic queueing delay for the Incident Autopilot capacity scenario.
        const queueStart = Date.now();
        if (podBehavior.inventoryDelayMs > 0) {
          await new Promise((resolve) => setTimeout(resolve, podBehavior.inventoryDelayMs));
        }
        span.setAttribute('inventory.queue_time_ms', Date.now() - queueStart);

        if (podBehavior.inventoryErrorRate > 0) {
          // Deterministic failure rate for the Incident Autopilot bad-pod scenario.
          const chance = Math.random() * 100;
          if (chance < podBehavior.inventoryErrorRate) {
            const err = new Error('Inventory lock timeout. Item temporarily out of stock.');
            span.recordException(err);
            span.setStatus({ code: SpanStatusCode.ERROR, message: err.message });
            logger.warn('Inventory reservation failed due to deterministic pod behavior', {
              inventoryErrorRate: podBehavior.inventoryErrorRate,
            });
            throw err;
          }
        } else if (chaosConfig.enabled && chaosConfig.errorRate > 0) {
          // Legacy random chaos simulation, used when deterministic behavior is disabled.
          const chance = Math.random() * 100;
          if (chance < (chaosConfig.errorRate / 2)) { // Split failure risk between payment and inventory
            const err = new Error('Inventory lock timeout. Item temporarily out of stock.');
            span.recordException(err);
            span.setStatus({ code: SpanStatusCode.ERROR, message: err.message });
            logger.warn('Inventory reservation failed due to simulated chaos error');
            throw err;
          }
        }
        logger.info('Inventory items reserved successfully');
      } finally {
        span.end();
      }
    });

    // Step 3: Process Payment
    await tracer.startActiveSpan('processPayment', async (span) => {
      try {
        span.setAttribute('payment.provider', 'Stripe-Simulated');
        span.setAttribute('payment.amount_cents', orderTotal);

        // Chaos simulation: Payment processing random failures
        if (chaosConfig.enabled && chaosConfig.errorRate > 0) {
          const chance = Math.random() * 100;
          if (chance < (chaosConfig.errorRate / 2)) {
            paymentFailuresCounter.add(1, { reason: 'chaos_failure', provider: 'Stripe-Simulated' });
            const err = new Error('Credit card was declined. Insufficient funds.');
            span.recordException(err);
            span.setStatus({ code: SpanStatusCode.ERROR, message: err.message });
            logger.warn('Payment declined by payment gateway (simulated chaos)');
            throw err;
          }
        }
        logger.info('Payment processed successfully through Stripe-Simulated');
      } finally {
        span.end();
      }
    });

    // Step 4: Persist Order
    const order = await tracer.startActiveSpan('persistOrder', async (span) => {
      try {
        const orderId = `ord-${Math.floor(100000 + Math.random() * 900000)}`;
        const newOrder = {
          id: orderId,
          items: validatedItems,
          customerName,
          shippingAddress,
          total: orderTotal,
          status: 'confirmed',
          createdAt: new Date().toISOString(),
        };
        ordersDb.set(orderId, newOrder);
        span.setAttribute('db.order_id', orderId);
        span.setAttribute('db.operation', 'INSERT');
        logger.info('Order persisted to mock database', { orderId });
        return newOrder;
      } finally {
        span.end();
      }
    });

    // Calculate checkout duration
    const duration = Date.now() - startTime;
    checkoutDurationHistogram.record(duration, { status: 'success' });
    ordersCreatedCounter.add(1, { status: 'success' });
    checkoutDurationMilliseconds.record(duration, { status: 'success' });
    checkoutRequestsCounter.add(1, { status: 'success' });

    res.status(201).json({
      message: 'Order created successfully!',
      orderId: order.id,
      total: order.total,
    });
  } catch (err) {
    const duration = Date.now() - startTime;
    checkoutDurationHistogram.record(duration, { status: 'failed' });
    ordersCreatedCounter.add(1, { status: 'failed' });
    checkoutDurationMilliseconds.record(duration, { status: 'failed' });
    checkoutRequestsCounter.add(1, { status: 'failed' });

    // Client-facing error messages must be generic and non-sensitive
    logger.error('Checkout flow failed', { error: err.message });
    res.status(500).json({ error: 'Order processing failed. Please try again later.' });
  }
});

// 4. Retrieve Order by ID (simulating errors easily)
app.get('/api/orders/:id', (req, res) => {
  const { id } = req.id || req.params;
  logger.info('Retrieving order status', { orderId: id });

  const order = ordersDb.get(id);
  if (!order) {
    logger.warn('Order retrieval failed, order ID not found', { orderId: id });
    const currentSpan = trace.getActiveSpan();
    if (currentSpan) {
      currentSpan.setStatus({ code: SpanStatusCode.ERROR, message: 'Order not found' });
    }
    return res.status(404).json({ error: 'Requested order could not be located.' });
  }

  res.status(200).json(order);
});

// 5. Dynamic Chaos Controls Configuration
app.post('/api/chaos/config', (req, res) => {
  const { delayMs, errorRate, enabled } = req.body;

  if (delayMs !== undefined) chaosConfig.delayMs = parseInt(delayMs, 10) || 0;
  if (errorRate !== undefined) chaosConfig.errorRate = Math.min(100, Math.max(0, parseInt(errorRate, 10) || 0));
  if (enabled !== undefined) chaosConfig.enabled = !!enabled;

  logger.info('Chaos configuration dynamically updated', chaosConfig);
  res.status(200).json({ message: 'Chaos config updated successfully', currentConfig: chaosConfig });
});

app.get('/api/chaos/config', (req, res) => {
  res.status(200).json(chaosConfig);
});

// 6. Manual Custom Metric Event Counter Trigger
app.post('/api/telemetry/event', (req, res) => {
  const { name, value, attributes } = req.body;

  if (!name) {
    return res.status(400).json({ error: 'Telemetry event requires a name.' });
  }

  const metricVal = parseInt(value, 10) || 1;
  const attrs = attributes && typeof attributes === 'object' ? attributes : {};

  customEventsCounter.add(metricVal, { event_name: name, ...attrs });
  logger.info(`Custom telemetry event tracked: ${name}`, { value: metricVal, attributes: attrs });

  res.status(200).json({ status: 'event_tracked', name, value: metricVal });
});

// 7. SSRF-safe Browser OTLP Trace Forwarding Proxy
app.post('/signoz/otel/v1/traces', async (req, res) => {
  try {
    logger.info('Forwarding browser telemetry batch to SigNoz agent');
    const response = await fetch(`${signozEndpoint}/v1/traces`, {
      method: 'POST',
      headers: {
        'Content-Type': req.headers['content-type'] || 'application/json',
      },
      body: JSON.stringify(req.body),
    });

    const text = await response.text();
    res.status(response.status).send(text);
  } catch (err) {
    logger.error('Failed to proxy browser trace batch to SigNoz collection agent', { error: err.message });
    res.status(502).json({ error: 'Local collector pipeline is unreachable.' });
  }
});

// Standard Error Handling Middleware (Security compliance)
app.use((err, req, res, next) => {
  logger.error('Unhandled express exception', { error: err.message, stack: err.stack });
  
  const activeSpan = trace.getActiveSpan();
  if (activeSpan) {
    activeSpan.setStatus({ code: SpanStatusCode.ERROR, message: err.message });
    activeSpan.recordException(err);
  }

  res.status(500).json({ error: 'An unexpected internal error has occurred.' });
});

app.listen(PORT, () => {
  logger.info(`Telemetry Shop API online and listening on http://localhost:${PORT}`);
  logger.info(`OTLP agent endpoint configured to target: ${signozEndpoint}`);
});
