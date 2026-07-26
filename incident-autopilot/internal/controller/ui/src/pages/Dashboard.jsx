import { useEffect, useState } from 'react';
import { getStatus } from '../api';

function MetricCard({ label, value, sub, warn }) {
  return (
    <div className={`rounded-xl border p-5 ${warn ? 'border-red-500/50 bg-red-950/30' : 'border-slate-800 bg-slate-900'}`}>
      <p className="text-xs uppercase tracking-wider text-slate-400 font-semibold">{label}</p>
      <p className={`text-2xl font-bold mt-1 ${warn ? 'text-red-400' : 'text-white'}`}>{value}</p>
      {sub && <p className="text-xs text-slate-500 mt-1">{sub}</p>}
    </div>
  );
}

function Badge({ children, tone = 'neutral' }) {
  const tones = {
    neutral: 'bg-slate-800 text-slate-200',
    ok: 'bg-emerald-900/50 text-emerald-300 border border-emerald-700/50',
    warn: 'bg-amber-900/50 text-amber-300 border border-amber-700/50',
    bad: 'bg-red-900/50 text-red-300 border border-red-700/50',
  };
  return (
    <span className={`inline-flex px-3 py-1 rounded-full text-sm font-medium capitalize ${tones[tone]}`}>
      {children}
    </span>
  );
}

export default function Dashboard() {
  const [status, setStatus] = useState(null);
  const [error, setError] = useState(null);

  useEffect(() => {
    let alive = true;
    const poll = async () => {
      try {
        const data = await getStatus();
        if (alive) {
          setStatus(data);
          setError(null);
        }
      } catch (err) {
        if (alive) setError(err.message);
      }
    };
    poll();
    const id = setInterval(poll, 4000);
    return () => {
      alive = false;
      clearInterval(id);
    };
  }, []);

  if (error && !status) {
    return <p className="text-red-400">Failed to load status: {error}</p>;
  }
  if (!status) {
    return <p className="text-slate-400">Loading status…</p>;
  }

  const snap = status.lastSnapshot || {};
  const stale = status.telemetryFreshnessSeconds > status.freshnessLimitSeconds;
  const drift = status.drift;
  const replicaMismatch =
    status.currentReplicas !== status.recommendedReplicas ||
    status.availableReplicas !== status.recommendedReplicas;

  const decisionTone =
    status.lastDecision === 'hold' ? 'ok' :
    status.lastDecision === 'indeterminate' ? 'warn' : 'bad';

  return (
    <div className="space-y-8">
      <div className="flex flex-wrap items-center gap-3">
        <h2 className="text-xl font-semibold">Live status</h2>
        <Badge tone={status.mode === 'approval' ? 'warn' : 'neutral'}>{status.mode} mode</Badge>
        {drift && <Badge tone="bad">Replica drift</Badge>}
        {stale && <Badge tone="warn">Stale telemetry</Badge>}
        {!status.telemetryAvailable && <Badge tone="warn">Telemetry unavailable</Badge>}
      </div>

      {!status.telemetryAvailable && (
        <div className="rounded-lg border border-amber-600/40 bg-amber-950/20 px-4 py-3 text-sm text-amber-200">
          SigNoz metrics are not reaching the controller yet. Replica counts below are live from Kubernetes;
          SLI/P95/error rate will populate once telemetry queries succeed (typically within 1–2 minutes of load).
        </div>
      )}

      {drift && (
        <div className="rounded-lg border border-red-500/40 bg-red-950/20 px-4 py-3 text-sm text-red-200">
          Current replicas ({status.currentReplicas}) differ from KEDA published target ({status.recommendedReplicas}).
          Approve a scale-down or scale-up through Actions — manual kubectl scale will be overridden.
        </div>
      )}

      <div className="grid grid-cols-1 sm:grid-cols-3 gap-4">
        <MetricCard
          label="Current replicas"
          value={status.currentReplicas}
          warn={replicaMismatch}
        />
        <MetricCard
          label="Available replicas"
          value={status.availableReplicas}
          warn={status.availableReplicas !== status.recommendedReplicas}
        />
        <MetricCard
          label="Recommended (KEDA)"
          value={status.recommendedReplicas}
          warn={replicaMismatch}
        />
      </div>

      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
        <MetricCard label="Decision" value={<Badge tone={decisionTone}>{(status.lastDecision || '—').replace(/_/g, ' ')}</Badge>} />
        <MetricCard label="SLI" value={`${((snap.sli ?? 0) * 100).toFixed(1)}%`} />
        <MetricCard label="P95 latency" value={`${Math.round(snap.p95Ms ?? 0)} ms`} />
        <MetricCard label="Request rate" value={`${(snap.requestRate ?? 0).toFixed(1)}/s`} />
      </div>

      <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
        <MetricCard label="Error rate" value={`${((snap.errorRate ?? 0) * 100).toFixed(2)}%`} />
        <MetricCard
          label="Telemetry freshness"
          value={`${Math.round(status.telemetryFreshnessSeconds)}s`}
          sub={`Limit: ${Math.round(status.freshnessLimitSeconds)}s`}
          warn={stale}
        />
      </div>

      {status.pendingRecommendation && (
        <div className="rounded-xl border border-amber-600/40 bg-amber-950/20 p-5">
          <p className="text-sm text-amber-200 font-medium">Pending recommendation — approve or reject on the Actions page.</p>
          <p className="text-xs text-amber-400/80 mt-1 capitalize">
            {(status.pendingRecommendation.decision || '').replace(/_/g, ' ')}:{' '}
            {status.pendingRecommendation.currentReplicas} → {status.pendingRecommendation.recommendedReplicas}
          </p>
        </div>
      )}
    </div>
  );
}
