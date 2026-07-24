package signoz

import (
	"context"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/config"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/kube"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/model"
)

// TargetReader lists owned pods for per-pod telemetry enrichment.
type TargetReader interface {
	Target(ctx context.Context) (kube.TargetState, error)
}

// Bridge combines service-wide SigNoz signals with per-pod metrics from owned pods.
type Bridge struct {
	Client *Client
	Kube   TargetReader
}

func (b *Bridge) Snapshot(ctx context.Context, cfg config.Config, replicas int32) (model.Snapshot, error) {
	snap, err := b.Client.Snapshot(ctx, cfg, replicas)
	if err != nil {
		return snap, err
	}
	if b.Kube == nil {
		return snap, nil
	}
	target, err := b.Kube.Target(ctx)
	if err != nil {
		return snap, nil
	}
	names := make([]string, 0, len(target.Pods))
	for _, pod := range target.Pods {
		names = append(names, pod.Name)
	}
	return b.Client.EnrichPodSnapshots(ctx, cfg, snap, names)
}
