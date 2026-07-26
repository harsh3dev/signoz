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
	ID                  string     `json:"id"`
	CreatedAt           time.Time  `json:"created_at"`
	ExpiresAt           time.Time  `json:"expires_at"`
	Decision            Decision   `json:"decision"`
	CurrentReplicas     int32      `json:"current_replicas"`
	RecommendedReplicas int32      `json:"recommended_replicas"`
	TargetPod           string     `json:"target_pod,omitempty"`
	Reason              string     `json:"reason"`
	Confidence          float64    `json:"confidence"`
	Evidence            []Evidence `json:"evidence,omitempty"`
	PolicyVersion       string     `json:"policy_version"`
}

type Action struct {
	RecommendationID string    `json:"recommendation_id"`
	ApprovedBy         string    `json:"approved_by,omitempty"`
	ApprovedAt         time.Time `json:"approved_at,omitempty"`
	StartedAt          time.Time `json:"started_at,omitempty"`
	CompletedAt        time.Time `json:"completed_at,omitempty"`
	Result             string    `json:"result,omitempty"`
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

// HistoryEntry records one recommendation lifecycle event for the actions
// history API. Outcome is one of: pending, approved, rejected, expired,
// superseded.
type HistoryEntry struct {
	Recommendation Recommendation `json:"recommendation"`
	Action         Action         `json:"action,omitempty"`
	Outcome        string         `json:"outcome"`
	RecordedAt     time.Time      `json:"recorded_at"`
}

// ReplicaStatus is the minimal Kubernetes replica state combined with
// SigNoz signals during evaluation and verification.
type ReplicaStatus struct {
	Current   int32
	Available int32
}
