CLUSTER_NAME ?= autopilot
NAMESPACE    ?= autopilot-demo
UI_DIR       := internal/controller/ui

.PHONY: test vet build build-local ui-build ui-dev cluster deploy

ui-build:
	cd $(UI_DIR) && npm ci && npm run build

ui-dev:
	cd $(UI_DIR) && npm run dev

test:
	go test ./...

vet:
	go vet ./...

build-local:
	go build -o bin/autopilot ./cmd/autopilot

build:
	docker build -t incident-autopilot:dev .

cluster:
	@if kind get clusters 2>/dev/null | grep -qx "$(CLUSTER_NAME)"; then \
		echo "Kind cluster '$(CLUSTER_NAME)' already exists"; \
	else \
		kind create cluster --name $(CLUSTER_NAME) --config deploy/kind.yaml; \
	fi
	kubectl config use-context kind-$(CLUSTER_NAME)

deploy:
	kubectl apply -f deploy/rbac.yaml
	kubectl apply -f deploy/demo-app.yaml
	kubectl apply -f deploy/autopilot.yaml
	kubectl apply -f deploy/scaledobject.yaml
