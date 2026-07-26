import { useEffect, useState } from 'react';
import { getLoadStatus, getStatus, startBadPodLoad, startCapacityLoad, stopLoad } from '../api';

export default function LoadTest() {
  const [capacity, setCapacity] = useState({ delayMs: 1500, vus: 40, durationSeconds: 300 });
  const [badPod, setBadPod] = useState({ errorRate: 100, targetPod: '' });
  const [pods, setPods] = useState([]);
  const [loadStatus, setLoadStatus] = useState(null);
  const [message, setMessage] = useState(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let alive = true;
    const poll = async () => {
      try {
        const [status, load] = await Promise.all([getStatus(), getLoadStatus()]);
        if (!alive) return;
        setPods(status.pods || []);
        setLoadStatus(load);
      } catch {
        /* ignore transient errors while polling */
      }
    };
    poll();
    const id = setInterval(poll, 4000);
    return () => {
      alive = false;
      clearInterval(id);
    };
  }, []);

  const run = async (fn) => {
    setBusy(true);
    setMessage(null);
    try {
      const result = await fn();
      setMessage(typeof result === 'object' ? JSON.stringify(result) : String(result));
    } catch (err) {
      setMessage(`Error: ${err.message}`);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-10">
      <div>
        <h2 className="text-xl font-semibold mb-1">Load test control</h2>
        <p className="text-sm text-slate-400">Trigger capacity pressure or bad-pod error injection.</p>
      </div>

      {loadStatus && (
        <div className="rounded-xl border border-slate-800 bg-slate-900 p-5">
          <h3 className="text-sm font-semibold text-slate-300 mb-2">Job status</h3>
          <p className="text-lg font-medium">
            {loadStatus.running ? (
              <span className="text-emerald-400">Running</span>
            ) : (
              <span className="text-slate-400">Not running</span>
            )}
          </p>
          {loadStatus.running && (
            <p className="text-sm text-slate-400 mt-1">
              {loadStatus.vus} VUs · {loadStatus.durationSeconds}s duration · {loadStatus.elapsedSeconds}s elapsed
            </p>
          )}
          {loadStatus.injectedPods && Object.keys(loadStatus.injectedPods).length > 0 && (
            <ul className="mt-3 text-xs text-slate-400 space-y-1">
              {Object.entries(loadStatus.injectedPods).map(([pod, desc]) => (
                <li key={pod}><span className="font-mono text-slate-300">{pod}</span>: {desc}</li>
              ))}
            </ul>
          )}
        </div>
      )}

      {message && (
        <div className="rounded-lg border border-slate-700 bg-slate-900 px-4 py-3 text-sm text-slate-300">{message}</div>
      )}

      <section className="rounded-xl border border-slate-800 bg-slate-900 p-6 space-y-4">
        <h3 className="font-semibold">Capacity load</h3>
        <p className="text-sm text-slate-400">Sets inventory delay on all pods and starts the load-generator job.</p>
        <div className="grid grid-cols-1 sm:grid-cols-3 gap-4">
          <label className="block text-sm">
            <span className="text-slate-400">Delay (ms)</span>
            <input
              type="number"
              className="mt-1 w-full rounded-lg bg-slate-950 border border-slate-700 px-3 py-2"
              value={capacity.delayMs}
              onChange={(e) => setCapacity({ ...capacity, delayMs: Number(e.target.value) })}
            />
          </label>
          <label className="block text-sm">
            <span className="text-slate-400">VUs</span>
            <input
              type="number"
              className="mt-1 w-full rounded-lg bg-slate-950 border border-slate-700 px-3 py-2"
              value={capacity.vus}
              onChange={(e) => setCapacity({ ...capacity, vus: Number(e.target.value) })}
            />
          </label>
          <label className="block text-sm">
            <span className="text-slate-400">Duration (s)</span>
            <input
              type="number"
              className="mt-1 w-full rounded-lg bg-slate-950 border border-slate-700 px-3 py-2"
              value={capacity.durationSeconds}
              onChange={(e) => setCapacity({ ...capacity, durationSeconds: Number(e.target.value) })}
            />
          </label>
        </div>
        <div className="flex gap-3">
          <button
            disabled={busy}
            onClick={() => run(() => startCapacityLoad(capacity))}
            className="px-4 py-2 rounded-lg bg-blue-600 hover:bg-blue-500 disabled:opacity-50 font-medium text-sm"
          >
            Start capacity load
          </button>
          <button
            disabled={busy}
            onClick={() => run(() => stopLoad())}
            className="px-4 py-2 rounded-lg bg-slate-700 hover:bg-slate-600 disabled:opacity-50 font-medium text-sm"
          >
            Stop
          </button>
        </div>
      </section>

      <section className="rounded-xl border border-slate-800 bg-slate-900 p-6 space-y-4">
        <h3 className="font-semibold">Bad pod</h3>
        <p className="text-sm text-slate-400">Injects inventory errors on one pod and starts background load.</p>
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <label className="block text-sm">
            <span className="text-slate-400">Error rate (%)</span>
            <input
              type="number"
              className="mt-1 w-full rounded-lg bg-slate-950 border border-slate-700 px-3 py-2"
              value={badPod.errorRate}
              onChange={(e) => setBadPod({ ...badPod, errorRate: Number(e.target.value) })}
            />
          </label>
          <label className="block text-sm">
            <span className="text-slate-400">Target pod (blank = auto-pick)</span>
            <select
              className="mt-1 w-full rounded-lg bg-slate-950 border border-slate-700 px-3 py-2"
              value={badPod.targetPod}
              onChange={(e) => setBadPod({ ...badPod, targetPod: e.target.value })}
            >
              <option value="">Auto-pick last ready pod</option>
              {pods.map((p) => (
                <option key={p} value={p}>{p}</option>
              ))}
            </select>
          </label>
        </div>
        <button
          disabled={busy}
          onClick={() => run(() => startBadPodLoad({
            errorRate: badPod.errorRate,
            ...(badPod.targetPod ? { targetPod: badPod.targetPod } : {}),
          }))}
          className="px-4 py-2 rounded-lg bg-orange-600 hover:bg-orange-500 disabled:opacity-50 font-medium text-sm"
        >
          Start bad-pod test
        </button>
      </section>
    </div>
  );
}
