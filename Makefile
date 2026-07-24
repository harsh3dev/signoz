.PHONY: help up down urls dev stop-dev
.PHONY: signoz-up signoz-down signoz-logs signoz-status
.PHONY: collector-up collector-down collector-logs
.PHONY: demo-install demo demo-build
.PHONY: autopilot-build autopilot-test autopilot-vet autopilot-run
.PHONY: test cluster cluster-delete k8s-setup k8s-install-keda k8s-build-images k8s-deploy k8s-load physical-verify clean

# Repository layout
ROOT            := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
SIGNOZ_DIR      := $(ROOT)/pours/deployment
COLLECTOR_DIR   := $(ROOT)/demo-app/collector
DEMO_DIR        := $(ROOT)/demo-app
AUTOPILOT_DIR   := $(ROOT)/incident-autopilot

# Tunables (override on the command line, e.g. make cluster CLUSTER_NAME=my-cluster)
CLUSTER_NAME         ?= autopilot
AUTOPILOT_PORT       ?= 8090
AUTOPILOT_SECRET     ?= dev-approval-secret
AUTOPILOT_CONFIG     ?= config.local.yaml
SIGNOZ_OTLP_ENDPOINT ?= http://localhost:4318
DEMO_PORT            ?= 3000
DEMO_IMAGE           ?= telemetry-shop:dev
AUTOPILOT_IMAGE      ?= incident-autopilot:dev

.DEFAULT_GOAL := help

help: ## Show available targets
	@echo "SigNoz Incident Autopilot — project Makefile"
	@echo ""
	@echo "Quick start:"
	@echo "  make up          Start SigNoz + collection agent (Docker)"
	@echo "  make dev         Start Docker services and print run commands"
	@echo "  make demo        Run Telemetry Shop demo app (foreground)"
	@echo "  make autopilot   Run incident-autopilot server (foreground)"
	@echo ""
	@echo "Targets:"
	@grep -E '^[a-zA-Z0-9_.-]+:.*##' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*## "}; {printf "  %-22s %s\n", $$1, $$2}'

# ---------------------------------------------------------------------------
# Combined workflows
# ---------------------------------------------------------------------------

up: signoz-up collector-up urls ## Start SigNoz stack and OTLP collection agent

down: collector-down signoz-down ## Stop collection agent and SigNoz stack

dev: up demo-install ## Start Docker services; run demo + autopilot in other terminals
	@echo ""
	@echo "Docker services are up. In separate terminals run:"
	@echo "  make demo"
	@echo "  make autopilot"
	@echo ""
	@echo "When finished: make down"

urls: ## Print local service URLs
	@echo "SigNoz UI:              http://localhost:8080"
	@echo "SigNoz OTLP (HTTP):     http://localhost:4318"
	@echo "Collection agent OTLP:  http://localhost:14318"
	@echo "Telemetry Shop:         http://localhost:$(DEMO_PORT)"
	@echo "Incident Autopilot API: http://localhost:$(AUTOPILOT_PORT)"

# ---------------------------------------------------------------------------
# SigNoz (self-hosted stack)
# ---------------------------------------------------------------------------

signoz-up: ## Start self-hosted SigNoz via Docker Compose
	@echo "==> Starting SigNoz"
	cd $(SIGNOZ_DIR) && docker compose up -d
	@echo "==> Waiting for SigNoz health"
	@for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30; do \
		if curl -sf http://localhost:8080/api/v1/health >/dev/null 2>&1; then \
			echo "SigNoz is ready"; \
			exit 0; \
		fi; \
		sleep 2; \
	done; \
	echo "SigNoz did not become healthy in time; check: make signoz-logs"; \
	exit 1

signoz-down: ## Stop SigNoz stack
	cd $(SIGNOZ_DIR) && docker compose down

signoz-logs: ## Tail SigNoz container logs
	cd $(SIGNOZ_DIR) && docker compose logs -f --tail=100

signoz-status: ## Show SigNoz container status
	cd $(SIGNOZ_DIR) && docker compose ps

# ---------------------------------------------------------------------------
# Demo app collection agent
# ---------------------------------------------------------------------------

collector-up: ## Start the demo OTLP collection agent
	@echo "==> Starting SigNoz collection agent"
	cd $(COLLECTOR_DIR) && docker compose up -d

collector-down: ## Stop the demo OTLP collection agent
	cd $(COLLECTOR_DIR) && docker compose down

collector-logs: ## Tail collection agent logs
	docker logs -f signoz-collection-agent

# ---------------------------------------------------------------------------
# Telemetry Shop demo application
# ---------------------------------------------------------------------------

demo-install: ## Install demo app npm dependencies
	cd $(DEMO_DIR) && npm install

demo-build: demo-install ## Build browser OpenTelemetry bundle
	cd $(DEMO_DIR) && npm run build

demo: demo-install ## Run Telemetry Shop locally (foreground)
	cd $(DEMO_DIR) && npm run dev

# ---------------------------------------------------------------------------
# Incident Autopilot controller
# ---------------------------------------------------------------------------

autopilot-build: ## Build incident-autopilot binary
	cd $(AUTOPILOT_DIR) && go build -o bin/autopilot ./cmd/autopilot

autopilot-test: ## Run incident-autopilot unit tests
	cd $(AUTOPILOT_DIR) && go test ./...

autopilot-vet: ## Run go vet on incident-autopilot
	cd $(AUTOPILOT_DIR) && go vet ./...

autopilot: autopilot-run ## Alias for autopilot-run

autopilot-run: ## Run incident-autopilot locally (foreground)
	@mkdir -p $(AUTOPILOT_DIR)/.state
	cd $(AUTOPILOT_DIR) && \
		AUTOPILOT_APPROVAL_SECRET=$(AUTOPILOT_SECRET) \
		go run ./cmd/autopilot \
			--config $(AUTOPILOT_CONFIG) \
			--listen-addr :$(AUTOPILOT_PORT) \
			--otlp-endpoint $(SIGNOZ_OTLP_ENDPOINT)

test: autopilot-test ## Run all project tests

# ---------------------------------------------------------------------------
# Kubernetes / Kind (for later integration testing)
# ---------------------------------------------------------------------------

cluster: ## Create local Kind cluster for autopilot testing
	@if kind get clusters 2>/dev/null | grep -qx "$(CLUSTER_NAME)"; then \
		echo "Kind cluster '$(CLUSTER_NAME)' already exists"; \
	else \
		kind create cluster --name $(CLUSTER_NAME) --config $(AUTOPILOT_DIR)/deploy/kind.yaml; \
	fi
	kubectl config use-context kind-$(CLUSTER_NAME)

cluster-delete: ## Delete local Kind cluster
	kind delete cluster --name $(CLUSTER_NAME)

k8s-setup: ## Install Kind cluster + KEDA for local testing
	$(AUTOPILOT_DIR)/scripts/k8s-setup.sh

k8s-install-keda: cluster ## Install or upgrade KEDA on the Kind cluster
	helm repo add kedacore https://kedacore.github.io/charts >/dev/null 2>&1 || true
	helm repo update kedacore
	helm upgrade --install keda kedacore/keda \
		--namespace keda \
		--create-namespace \
		--wait \
		--timeout 5m

k8s-build-images: ## Build and load demo + autopilot images into Kind
	docker build -t $(DEMO_IMAGE) $(DEMO_DIR)
	docker build -t $(AUTOPILOT_IMAGE) $(AUTOPILOT_DIR)
	kind load docker-image $(DEMO_IMAGE) --name $(CLUSTER_NAME)
	kind load docker-image $(AUTOPILOT_IMAGE) --name $(CLUSTER_NAME)

k8s-deploy: cluster ## Apply autopilot Kubernetes manifests
	kubectl apply -f $(AUTOPILOT_DIR)/deploy/rbac.yaml
	kubectl apply -f $(AUTOPILOT_DIR)/deploy/demo-app.yaml
	kubectl apply -f $(AUTOPILOT_DIR)/deploy/autopilot.yaml
	kubectl apply -f $(AUTOPILOT_DIR)/deploy/scaledobject.yaml
	kubectl -n autopilot-demo rollout status deployment/checkout-api --timeout=180s

k8s-load: ## Deploy load generator for capacity testing
	kubectl apply -f $(AUTOPILOT_DIR)/deploy/load-generator.yaml

physical-verify: ## Run end-to-end physical verification on Kind
	$(AUTOPILOT_DIR)/scripts/physical-verify.sh

clean: ## Remove local build artifacts
	rm -rf $(AUTOPILOT_DIR)/bin
	rm -rf $(DEMO_DIR)/node_modules
	rm -rf $(AUTOPILOT_DIR)/.state
