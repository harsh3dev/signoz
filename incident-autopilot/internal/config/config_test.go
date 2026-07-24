package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadValidatesReplicaBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(path, []byte(`
signoz:
  url: http://signoz:8080
target:
  service: checkout-api
  namespace: demo
  deployment: checkout-api
policy:
  min_replicas: 6
  max_replicas: 2
controller:
  mode: dry-run
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Load(path)
	if err == nil {
		t.Fatal("expected invalid replica bounds")
	}
}

func TestLoadRejectsUnknownMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(path, []byte(`
signoz:
  url: http://signoz:8080
target:
  service: checkout-api
  namespace: demo
  deployment: checkout-api
policy:
  min_replicas: 2
  max_replicas: 10
controller:
  mode: unsafe
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Load(path)
	if err == nil {
		t.Fatal("expected unknown mode error")
	}
}

func TestLoadRejectsMissingIdentifiers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(path, []byte(`
signoz:
  url: http://signoz:8080
target:
  service: ""
  namespace: demo
  deployment: checkout-api
policy:
  min_replicas: 2
  max_replicas: 10
controller:
  mode: dry-run
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Load(path)
	if err == nil {
		t.Fatal("expected missing identifier error")
	}
}

func TestLoadRejectsInvalidSLIObjective(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(path, []byte(`
signoz:
  url: http://signoz:8080
target:
  service: checkout-api
  namespace: demo
  deployment: checkout-api
signals:
  sli_objective: 1.5
policy:
  min_replicas: 2
  max_replicas: 10
controller:
  mode: dry-run
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Load(path)
	if err == nil {
		t.Fatal("expected invalid SLI objective error")
	}
}

func TestLoadAcceptsValidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(path, []byte(`
signoz:
  url: http://signoz:8080
  api_key_env: SIGNOZ_API_KEY
target:
  service: checkout-api
  namespace: demo
  deployment: checkout-api
signals:
  sli_objective: 0.99
policy:
  min_replicas: 2
  max_replicas: 10
controller:
  mode: approval
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("expected valid config to load, got error: %v", err)
	}
	if cfg.Target.Service != "checkout-api" {
		t.Fatalf("expected service checkout-api, got %q", cfg.Target.Service)
	}
}
