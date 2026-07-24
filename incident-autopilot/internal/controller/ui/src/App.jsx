import React, { useState, useEffect } from 'react';

function App() {
  const [action, setAction] = useState(null);
  const [error, setError] = useState(null);
  const [operator, setOperator] = useState('');
  const [status, setStatus] = useState(null);

  // Extract ID from URL path (e.g., /actions/rec-12345)
  const pathParts = window.location.pathname.split('/');
  const id = pathParts[pathParts.length - 1];

  useEffect(() => {
    if (!id || id === 'actions') return;
    
    fetch(`/api/actions/${id}`)
      .then(res => {
        if (!res.ok) throw new Error('Recommendation not found or superseded');
        return res.json();
      })
      .then(data => setAction(data))
      .catch(err => setError(err.message));
  }, [id]);

  const handleApprove = async (e) => {
    e.preventDefault();
    try {
      const res = await fetch(`/api/actions/${id}/approve`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/x-www-form-urlencoded',
          'Authorization': `Bearer ${action.secret}`,
          'X-Autopilot-Operator': operator
        },
        body: new URLSearchParams({ token: action.token })
      });
      
      if (!res.ok) {
        const text = await res.text();
        throw new Error(text);
      }
      
      setStatus('Approved successfully!');
    } catch (err) {
      setStatus(`Error: ${err.message}`);
    }
  };

  if (error) return <div className="p-8 text-red-500 font-bold">Error: {error}</div>;
  if (!action) return <div className="p-8 text-gray-500">Loading recommendation...</div>;

  return (
    <div className="min-h-screen bg-gray-50 p-8 font-sans">
      <div className="max-w-2xl mx-auto bg-white rounded-xl shadow-md overflow-hidden border border-gray-200">
        <div className="bg-blue-600 p-4 text-white">
          <h1 className="text-2xl font-bold">Incident Autopilot</h1>
          <p className="text-blue-100 text-sm">Action Approval Required</p>
        </div>
        
        <div className="p-6 space-y-6">
          <div className="grid grid-cols-2 gap-4">
            <div className="bg-gray-50 p-4 rounded-lg border border-gray-100">
              <span className="text-xs text-gray-500 uppercase font-bold tracking-wider">Decision</span>
              <p className="text-lg font-semibold text-gray-900 capitalize">{action.decision.replace(/_/g, ' ')}</p>
            </div>
            <div className="bg-gray-50 p-4 rounded-lg border border-gray-100">
              <span className="text-xs text-gray-500 uppercase font-bold tracking-wider">Replicas</span>
              <p className="text-lg font-semibold text-gray-900">
                {action.currentReplicas} <span className="text-gray-400 mx-2">→</span> <span className="text-blue-600">{action.recommendedReplicas}</span>
              </p>
            </div>
          </div>

          {action.targetPod && (
            <div className="bg-orange-50 p-4 rounded-lg border border-orange-100">
              <span className="text-xs text-orange-600 uppercase font-bold tracking-wider">Target Pod</span>
              <p className="text-md font-mono text-orange-900 mt-1">{action.targetPod}</p>
            </div>
          )}

          <div className="bg-red-50 p-5 rounded-lg border border-red-100">
            <h3 className="text-sm text-red-600 uppercase font-bold tracking-wider mb-2">Why this issue occurred</h3>
            <p className="text-red-900 leading-relaxed">{action.reason}</p>
          </div>

          <div className="text-sm text-gray-500">
            Expires at: {new Date(action.expiresAt).toLocaleString()}
          </div>

          <hr className="border-gray-200" />

          {status ? (
            <div className={`p-4 rounded-lg font-medium ${status.includes('Error') ? 'bg-red-100 text-red-700' : 'bg-green-100 text-green-700'}`}>
              {status}
            </div>
          ) : (
            <form onSubmit={handleApprove} className="space-y-4">
              <div>
                <label className="block text-sm font-medium text-gray-700 mb-1">Operator Name</label>
                <input 
                  type="text" 
                  required
                  value={operator}
                  onChange={e => setOperator(e.target.value)}
                  className="w-full px-4 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500 outline-none transition-all"
                  placeholder="Enter your name to approve"
                />
              </div>
              <button 
                type="submit"
                className="w-full bg-blue-600 hover:bg-blue-700 text-white font-bold py-3 px-4 rounded-lg transition-colors shadow-sm"
              >
                Approve Action
              </button>
            </form>
          )}
        </div>
      </div>
    </div>
  );
}

export default App;
