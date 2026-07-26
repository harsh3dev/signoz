import { useEffect, useState } from 'react';
import { approveAction, getAction, getStatus, listActions, rejectAction } from '../api';

function outcomeBadge(outcome) {
  const tones = {
    approved: 'bg-emerald-900/50 text-emerald-300',
    rejected: 'bg-red-900/50 text-red-300',
    pending: 'bg-amber-900/50 text-amber-300',
    superseded: 'bg-slate-800 text-slate-400',
    expired: 'bg-slate-800 text-slate-400',
  };
  return (
    <span className={`px-2 py-0.5 rounded text-xs font-medium ${tones[outcome] || tones.superseded}`}>
      {outcome}
    </span>
  );
}

export default function Actions() {
  const [pending, setPending] = useState(null);
  const [actionDetail, setActionDetail] = useState(null);
  const [history, setHistory] = useState([]);
  const [operator, setOperator] = useState('');
  const [statusMsg, setStatusMsg] = useState(null);
  const [secret, setSecret] = useState(import.meta.env.VITE_APPROVAL_SECRET || '');

  const refresh = async () => {
    const [st, hist] = await Promise.all([getStatus(), listActions()]);
    setHistory(hist || []);
    const p = st.pendingRecommendation;
    setPending(p || null);
    if (p?.id) {
      try {
        const detail = await getAction(p.id);
        setActionDetail(detail);
        if (detail.secret && !secret) setSecret(detail.secret);
      } catch {
        setActionDetail(p);
      }
    } else {
      setActionDetail(null);
    }
  };

  useEffect(() => {
    refresh().catch(() => {});
    const id = setInterval(() => refresh().catch(() => {}), 4000);
    return () => clearInterval(id);
  }, []);

  const handleApprove = async (e) => {
    e.preventDefault();
    if (!pending?.id || !actionDetail?.token) return;
    setStatusMsg(null);
    try {
      await approveAction(pending.id, actionDetail.token, secret, operator);
      setStatusMsg('Approved successfully.');
      await refresh();
    } catch (err) {
      setStatusMsg(`Error: ${err.message}`);
    }
  };

  const handleReject = async () => {
    if (!pending?.id || !operator) {
      setStatusMsg('Enter an operator name before rejecting.');
      return;
    }
    setStatusMsg(null);
    try {
      await rejectAction(pending.id, operator);
      setStatusMsg('Rejected.');
      await refresh();
    } catch (err) {
      setStatusMsg(`Error: ${err.message}`);
    }
  };

  const rec = actionDetail || pending;

  return (
    <div className="space-y-10">
      <div>
        <h2 className="text-xl font-semibold mb-1">Approvals</h2>
        <p className="text-sm text-slate-400">Review pending recommendations and action history.</p>
      </div>

      {rec ? (
        <div className="rounded-xl border border-slate-800 bg-slate-900 overflow-hidden">
          <div className="bg-blue-950/50 border-b border-slate-800 px-6 py-4">
            <h3 className="font-semibold">Pending recommendation</h3>
            <p className="text-sm text-slate-400 capitalize mt-0.5">{(rec.decision || '').replace(/_/g, ' ')}</p>
          </div>
          <div className="p-6 space-y-5">
            <div className="grid grid-cols-2 gap-4">
              <div>
                <p className="text-xs text-slate-500 uppercase font-semibold">Replicas</p>
                <p className="text-lg font-medium mt-1">
                  {rec.currentReplicas} <span className="text-slate-500">→</span>{' '}
                  <span className="text-blue-400">{rec.recommendedReplicas}</span>
                </p>
              </div>
              {rec.targetPod && (
                <div>
                  <p className="text-xs text-slate-500 uppercase font-semibold">Target pod</p>
                  <p className="font-mono text-sm mt-1 text-orange-300">{rec.targetPod}</p>
                </div>
              )}
            </div>
            <div className="rounded-lg bg-red-950/30 border border-red-900/50 p-4">
              <p className="text-xs text-red-400 uppercase font-semibold mb-1">Reason</p>
              <p className="text-sm text-red-100">{rec.reason}</p>
            </div>
            {rec.expiresAt && (
              <p className="text-xs text-slate-500">Expires: {new Date(rec.expiresAt).toLocaleString()}</p>
            )}

            {statusMsg && (
              <div className={`p-3 rounded-lg text-sm ${statusMsg.startsWith('Error') ? 'bg-red-950 text-red-300' : 'bg-emerald-950 text-emerald-300'}`}>
                {statusMsg}
              </div>
            )}

            {!statusMsg?.includes('Approved') && !statusMsg?.includes('Rejected') && (
              <div className="space-y-4">
                <label className="block text-sm">
                  <span className="text-slate-400">Operator name</span>
                  <input
                    required
                    value={operator}
                    onChange={(e) => setOperator(e.target.value)}
                    className="mt-1 w-full rounded-lg bg-slate-950 border border-slate-700 px-3 py-2"
                    placeholder="Your name"
                  />
                </label>
                {!secret && (
                  <label className="block text-sm">
                    <span className="text-slate-400">Approval secret</span>
                    <input
                      type="password"
                      value={secret}
                      onChange={(e) => setSecret(e.target.value)}
                      className="mt-1 w-full rounded-lg bg-slate-950 border border-slate-700 px-3 py-2"
                      placeholder="AUTOPILOT_APPROVAL_SECRET"
                    />
                  </label>
                )}
                <div className="flex gap-3">
                  <form onSubmit={handleApprove} className="flex-1">
                    <button
                      type="submit"
                      disabled={!operator || !secret}
                      className="w-full py-2.5 rounded-lg bg-blue-600 hover:bg-blue-500 disabled:opacity-50 font-medium"
                    >
                      Approve
                    </button>
                  </form>
                  <button
                    type="button"
                    onClick={handleReject}
                    disabled={!operator}
                    className="flex-1 py-2.5 rounded-lg bg-slate-700 hover:bg-slate-600 disabled:opacity-50 font-medium"
                  >
                    Reject
                  </button>
                </div>
              </div>
            )}
          </div>
        </div>
      ) : (
        <div className="rounded-xl border border-slate-800 bg-slate-900 p-8 text-center text-slate-400">
          No pending recommendation. Trigger a load test to generate one.
        </div>
      )}

      <section>
        <h3 className="font-semibold mb-4">History</h3>
        <div className="overflow-x-auto rounded-xl border border-slate-800">
          <table className="w-full text-sm">
            <thead className="bg-slate-900 text-slate-400 text-left">
              <tr>
                <th className="px-4 py-3 font-medium">Time</th>
                <th className="px-4 py-3 font-medium">Decision</th>
                <th className="px-4 py-3 font-medium">Replicas</th>
                <th className="px-4 py-3 font-medium">Outcome</th>
                <th className="px-4 py-3 font-medium">Operator</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-800">
              {history.length === 0 && (
                <tr>
                  <td colSpan={5} className="px-4 py-6 text-center text-slate-500">No history yet</td>
                </tr>
              )}
              {history.map((entry) => {
                const r = entry.recommendation || {};
                return (
                  <tr key={`${r.id}-${entry.recorded_at}`} className="bg-slate-950/50">
                    <td className="px-4 py-3 text-slate-400 whitespace-nowrap">
                      {entry.recorded_at ? new Date(entry.recorded_at).toLocaleString() : '—'}
                    </td>
                    <td className="px-4 py-3 capitalize">{(r.decision || '—').replace(/_/g, ' ')}</td>
                    <td className="px-4 py-3 font-mono text-xs">
                      {r.current_replicas ?? '?'} → {r.recommended_replicas ?? '?'}
                      {r.target_pod ? (
                        <span className="block text-orange-400/80">{r.target_pod}</span>
                      ) : null}
                    </td>
                    <td className="px-4 py-3">{outcomeBadge(entry.outcome)}</td>
                    <td className="px-4 py-3 text-slate-400">{entry.action?.approved_by || '—'}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </section>
    </div>
  );
}
