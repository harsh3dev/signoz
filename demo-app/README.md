# SigNoz Telemetry Shop Demo Application

Welcome to the **SigNoz Telemetry Shop**! This is a mock e-commerce storefront ("Telemetry Shop") and Ops Chaos Lab designed to demonstrate a robust, real-world integration of **self-hosted SigNoz** using Node.js (Express), a Vanilla HTML/JS frontend, and the official **SigNoz Docker Collection Agent**.

The primary goal of this application is to demonstrate how to emit and correlate **distributed traces, structured logs, and custom metrics** into your local SigNoz instance.

---

## 🏗️ Architecture & Data Flow

This demo application uses a professional, self-contained architecture that completely avoids port conflicts and provides end-to-end distributed tracing.

```
┌────────────────────────────────────────────────────────┐
│                   DEMO APPLICATION                     │
│                                                        │
│  ┌───────────────────────┐      ┌───────────────────┐  │
│  │   Vanilla Browser     │      │    Express API    │  │
│  │ (telemetry-shop-web)  │      │(telemetry-shop-api│  │
│  └───────────┬───────────┘      └─────────┬─────────┘  │
└──────────────│────────────────────────────│────────────┘
               │ fetch() + traceparent      │
               │ (distributed tracing)      │
               ▼                            │
     [POST /signoz/otel/...]                │
   (SSRF-safe Browser Proxy)                │
               │                            │
               ▼                            │
        ┌──────┴────────────────────────────┴──────────┐
        │  SigNoz Docker Collection Agent (OTel Contrib)│
        │               (Port 14318)                   │
        └──────────────────────┬───────────────────────┘
                               │
                               │ OTLP HTTP
                               ▼
        ┌──────────────────────────────────────────────┐
        │         Self-Hosted SigNoz Ingester          │
        │               (Port 4318)                    │
        └──────────────────────────────────────────────┘
```

### Highlights of the Integration
1. **Distributed Tracing (Browser ↔ API)**: The browser initiates a trace when you perform actions. OpenTelemetry automatically injects a W3C `traceparent` header into fetch calls. The Express server intercepts this and attaches child spans (`validateCart`, `reserveInventory`, `processPayment`, `persistOrder`), creating a single unified trace in SigNoz.
2. **SSRF-Safe Browser Proxy**: Browser traces are sent via a proxy route (`POST /signoz/otel/v1/traces`) inside the Express app, which forwards them to the collector agent. This avoids browser CORS errors and prevents exposing internal collector endpoints directly to the public internet.
3. **Structured Logs Correlation**: Server logs are managed via Winston and correlated with OpenTelemetry. When a request is active, Winston automatically attaches `trace_id` and `span_id` to log entries.
4. **Custom Metrics**: We record operational metrics such as orders created (`orders.created`), checkout durations (`checkout.duration`), payment issues (`payment.failures`), and user-driven events (`demo.custom_events`).
5. **No Host Port Conflicts**: The SigNoz stack already publishes ports `4317`/`4318` on your host. To avoid conflicts, our custom Docker Collection Agent is configured to listen on ports **`14317`** (gRPC) and **`14318`** (HTTP) for incoming application OTLP telemetry, while continuing to scrape host/container data.

---

## 🚀 Quick Start Guide

Follow these steps to spin up the entire pipeline:

### Step 1: Start Self-Hosted SigNoz
Ensure your main SigNoz services are running. From the workspace root:
```bash
cd pours/deployment
docker compose up -d
```
Verify you can access the SigNoz UI at [http://localhost:8080](http://localhost:8080).

### Step 2: Start the SigNoz Docker Collection Agent
This agent collects infrastructure metrics (CPU, Memory, Disk), Docker stats, Docker container logs, and handles OTLP forwarding.
```bash
cd ../../demo-app/collector
docker compose up -d
```
Validate that the agent started successfully:
```bash
docker logs signoz-collection-agent
```
You should see: `... Everything is ready. Begin running and processing data.`

### Step 3: Run the Telemetry Shop App
Ensure you have Node.js 20+ installed. Open a terminal in the `demo-app/` directory:
```bash
cd ../ # Navigate to demo-app/
npm install
npm run dev
```
The shop storefront will compile the browser OTel bundle and start the Express server at [http://localhost:3000](http://localhost:3000).

---

## 🔍 Verification & Runbook in SigNoz UI

Once the application is running, open [http://localhost:3000](http://localhost:3000) and perform actions to generate data. Then, open [http://localhost:8080](http://localhost:8080) to inspect your signals:

### 1. Host & Container Metrics (Infrastructure)
- In SigNoz, navigate to **Infrastructure Monitoring -> Hosts**.
- Your local machine host will appear, plotting live CPU, memory, network, and disk metrics.
- Navigate to the **Logs** tab and click **Logs Explorer**. Filter by `container_name = signoz-collection-agent` to verify Docker container logs are flowing!

### 2. Service Map & End-to-End Distributed Tracing
- Open the storefront at `localhost:3000`.
- Add items to your cart, fill in the shipping form, and click **Place Order**.
- In SigNoz, go to **Services** or **Traces**. You will see two services:
  - `telemetry-shop-web` (the client-side browser)
  - `telemetry-shop-api` (the Node.js backend)
- Open the latest trace for `POST /api/orders`. You will see a beautiful distributed tree:
  - Parent span: `POST /api/orders` (HTTP request)
    - Manual child span: `validateCart` (with attributes like `cart.item_count` and calculated total)
    - Manual child span: `reserveInventory`
    - Manual child span: `processPayment` (with billing attributes)
    - Manual child span: `persistOrder` (with `db.order_id` and database operation attributes)

### 3. Log-to-Trace Correlation
- In SigNoz, go to the **Logs Explorer** page.
- Query: `service_name = telemetry-shop-api`
- You will see structured server logs like `"Cart validated successfully"`, `"Payment processed successfully"`, or `"Order persisted to mock database"`.
- Click on any log line. In the details drawer, you'll see associated `trace_id` and `span_id`. Click the **View Trace** button to immediately pivot from that log line to the exact millisecond in the trace timeline!

### 4. Custom Metrics Charts
- In SigNoz, click on the **Dashboards** or **Explore** tab and choose **Metrics**.
- Search for our custom metrics:
  - `orders.created` (Counter of checkout completions)
  - `checkout.duration` (Histogram showing transaction latency)
  - `payment.failures` (Counter tracking billing declines)
  - `demo.custom_events` (Tracking manual clicks on the Chaos Lab banner, newsletter, or promo codes)
- Go to the **Chaos & Telemetry Lab** page (`localhost:3000/chaos.html`).
- Click **Emit Count +1** buttons under *Emit Business Telemetry Metrics* several times.
- Build a custom SigNoz dashboard with a Panel plotting `demo.custom_events` grouped by the attribute `event_name`!

### 5. Chaos, Error Rates, and Exception Recording
- Navigate to the **Chaos & Telemetry Lab** (`localhost:3000/chaos.html`).
- Set the **Forced Checkout Error Rate** slider to `100%` and click **Save Configurations**.
- Go back to the Storefront (`localhost:3000/index.html`) and attempt to place an order. It will fail.
- Return to SigNoz and check the **Traces** or **Errors** tab.
- Look at the failed trace for `POST /api/orders`. The parent span and the failing child span (`reserveInventory` or `processPayment`) will be highlighted in **red** (Error state).
- Open the span details. Under **Exceptions**, you will see the exact error stack trace recorded seamlessly via OpenTelemetry's standard `span.recordException()`.
- Client-facing error responses are sanitized (e.g. `Order processing failed. Please try again later.`), while the detailed trace and logs capture the full diagnostic detail.

---

## ⚙️ Environment Variables (`.env`)

You can customize the integration behavior in `demo-app/.env`:

```bash
# Target OTLP HTTP endpoint of our custom collection agent
SIGNOZ_OTLP_ENDPOINT=http://localhost:14318

# OpenTelemetry Resource Attributes
OTEL_SERVICE_NAME=telemetry-shop-api
OTEL_DEPLOYMENT_ENVIRONMENT=local

# Application port
PORT=3000

# Toggle active middleware chaos simulation (true/false)
CHAOS_ENABLED=true
```

---

## 🛡️ Security Guardrails
- **Input Validation**: All incoming REST cart arrays and customer identifiers are fully validated on the server.
- **Client-Facing Error Shielding**: Express exception boundaries intercept crash logs, capture stack traces to internal spans/logs, and return safe, generic error objects (`{ "error": "An unexpected internal error has occurred." }`) to the browser client.
- **SSRF Prevention**: The OTLP proxy only allows forwarding requests to the verified `SIGNOZ_OTLP_ENDPOINT` variable.
