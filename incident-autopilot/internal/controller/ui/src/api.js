const API_BASE = import.meta.env.VITE_API_BASE || '';

async function request(path, options = {}) {
  const res = await fetch(`${API_BASE}${path}`, options);
  if (!res.ok) {
    const text = await res.text();
    throw new Error(text || res.statusText);
  }
  const contentType = res.headers.get('content-type') || '';
  if (contentType.includes('application/json')) {
    return res.json();
  }
  return res.text();
}

export function getStatus() {
  return request('/api/status');
}

export function listActions() {
  return request('/api/actions');
}

export function getAction(id) {
  return request(`/api/actions/${id}`);
}

export function approveAction(id, token, secret, operator) {
  return request(`/api/actions/${id}/approve`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/x-www-form-urlencoded',
      Authorization: `Bearer ${secret}`,
      'X-Autopilot-Operator': operator,
    },
    body: new URLSearchParams({ token }),
  });
}

export function rejectAction(id, operator) {
  return request(`/api/actions/${id}/reject`, {
    method: 'POST',
    headers: { 'X-Autopilot-Operator': operator },
  });
}

export function startCapacityLoad(params) {
  return request('/api/loadtest/capacity', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(params),
  });
}

export function startBadPodLoad(params) {
  return request('/api/loadtest/badpod', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(params),
  });
}

export function stopLoad() {
  return request('/api/loadtest/stop', { method: 'POST' });
}

export function getLoadStatus() {
  return request('/api/loadtest/status');
}
