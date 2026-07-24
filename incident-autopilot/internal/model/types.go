package model

import "time"

type Decision string

const (
	DecisionHold          Decision = "hold"
	DecisionScaleUp       Decision = "scale_up"
	DecisionScaleDown     Decision = "scale_down"
	DecisionQuarantine    Decision = "quarantine_replace"
	DecisionInvestigate   Decision = "investigate"
	DecisionIndeterminate Decision = "indeterminate"
)

type PodSnapshot struct {
	Name        string
	UID         string
	Ready       bool
	RequestRate float64
	P95MS       float64
	ErrorRate   float64
	Restarts    int32
}

type Snapshot struct {
	Service         string
	ObservedAt      time.Time
	CurrentReplicas int32
	Available       int32
	RequestRate     float64
	P95MS           float64
	ErrorRate       float64
	SLI             float64
	Pods            []PodSnapshot
}

type Evidence struct {
	Signal   string  `json:"signal"`
	Observed float64 `json:"observed"`
	Target   float64 `json:"target"`
	Summary  string  `json:"summary"`
}

type Recommendation struct {
	ID                  string
	CreatedAt           time.Time
	ExpiresAt           time.Time
	Decision            Decision
	CurrentReplicas     int32
	RecommendedReplicas int32
	TargetPod           string
	Reason              string
	Confidence          float64
	Evidence            []Evidence
	PolicyVersion       string
}

type Action struct {
	RecommendationID string
	ApprovedBy       string
	ApprovedAt       time.Time
	StartedAt        time.Time
	CompletedAt      time.Time
	Result           string
}

type Verification struct {
	RecommendationID string
	BeforeSLI        float64
	AfterSLI         float64
	BeforeP95MS      float64
	AfterP95MS       float64
	BeforeErrorRate  float64
	AfterErrorRate   float64
	Result           string
}

// ReplicaStatus is the minimal Kubernetes replica state combined with
// SigNoz signals during evaluation and verification.
type ReplicaStatus struct {
	Current   int32
	Available int32
}
