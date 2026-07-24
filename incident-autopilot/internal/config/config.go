package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	SigNoz struct {
		URL       string `yaml:"url"`
		APIKeyEnv string `yaml:"api_key_env"`
	} `yaml:"signoz"`
	Target struct {
		Service       string `yaml:"service"`
		Environment   string `yaml:"environment"`
		Namespace     string `yaml:"namespace"`
		Deployment    string `yaml:"deployment"`
		ReadinessGate string `yaml:"readiness_gate"`
	} `yaml:"target"`
	Signals struct {
		RequestRateQuery string        `yaml:"request_rate_query"`
		P95LatencyQuery  string        `yaml:"p95_latency_query"`
		ErrorRateQuery   string        `yaml:"error_rate_query"`
		SLIQuery         string        `yaml:"sli_query"`
		SLIObjective     float64       `yaml:"sli_objective"`
		FreshnessLimit   time.Duration `yaml:"freshness_limit"`
	} `yaml:"signals"`
	Policy struct {
		MinReplicas              int32         `yaml:"min_replicas"`
		MaxReplicas              int32         `yaml:"max_replicas"`
		TargetRequestsPerReplica float64       `yaml:"target_requests_per_replica"`
		LatencyTargetMS          float64       `yaml:"latency_target_ms"`
		ErrorRateTarget          float64       `yaml:"error_rate_target"`
		MaxScaleUpStep           int32         `yaml:"max_scale_up_step"`
		MaxScaleDownStep         int32         `yaml:"max_scale_down_step"`
		Cooldown                 time.Duration `yaml:"cooldown"`
		PodOutlier               struct {
			MinimumRequests        int     `yaml:"minimum_requests"`
			ErrorRateMultiplier    float64 `yaml:"error_rate_multiplier"`
			LatencyMultiplier      float64 `yaml:"latency_multiplier"`
			ConsecutiveEvaluations int     `yaml:"consecutive_evaluations"`
		} `yaml:"pod_outlier"`
	} `yaml:"policy"`
	Controller struct {
		Mode               string        `yaml:"mode"`
		EvaluationInterval time.Duration `yaml:"evaluation_interval"`
		VerificationWindow time.Duration `yaml:"verification_window"`
		StatePath          string        `yaml:"state_path"`
	} `yaml:"controller"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.SigNoz.URL == "" || c.Target.Service == "" ||
		c.Target.Namespace == "" || c.Target.Deployment == "" {
		return fmt.Errorf("signoz URL and target identifiers are required")
	}
	if c.Policy.MinReplicas < 1 || c.Policy.MaxReplicas < c.Policy.MinReplicas {
		return fmt.Errorf("invalid replica bounds")
	}
	switch c.Controller.Mode {
	case "dry-run", "approval", "automatic":
	default:
		return fmt.Errorf("unsupported controller mode %q", c.Controller.Mode)
	}
	if c.Signals.SLIObjective <= 0 || c.Signals.SLIObjective > 1 {
		return fmt.Errorf("SLI objective must be in (0,1]")
	}
	return nil
}
