// Chaos & Telemetry Controls Logic
document.addEventListener('DOMContentLoaded', () => {
  const chaosForm = document.getElementById('chaos-config-form');
  const delayRange = document.getElementById('delay-range');
  const delayVal = document.getElementById('delay-val');
  const errorRange = document.getElementById('error-range');
  const errorVal = document.getElementById('error-val');
  const chaosBadge = document.getElementById('chaos-status-badge');
  const disableChaosBtn = document.getElementById('disable-chaos-btn');
  const burstBtn = document.getElementById('burst-btn');
  const terminalConsole = document.getElementById('terminal-console');
  const clearTerminalBtn = document.getElementById('clear-terminal-btn');

  // Load and bind configurations
  fetchChaosConfig();

  delayRange.addEventListener('input', () => {
    delayVal.innerText = delayRange.value;
  });

  errorRange.addEventListener('input', () => {
    errorVal.innerText = errorRange.value;
  });

  // Get active configurations from backend
  async function fetchChaosConfig() {
    try {
      const res = await fetch('/api/chaos/config');
      const config = await res.json();
      
      delayRange.value = config.delayMs;
      delayVal.innerText = config.delayMs;
      
      errorRange.value = config.errorRate;
      errorVal.innerText = config.errorRate;

      updateBadgeUI(config.enabled && (config.delayMs > 0 || config.errorRate > 0));
      logToConsole(`[SYSTEM] Retreived live chaos settings. Delay: ${config.delayMs}ms, Error Rate: ${config.errorRate}%`, 'info');
    } catch (err) {
      logToConsole(`[ERROR] Failed to fetch current chaos configurations.`, 'error');
    }
  }

  // Save updated configurations
  chaosForm.addEventListener('submit', async (e) => {
    e.preventDefault();

    const delayMs = parseInt(delayRange.value, 10);
    const errorRate = parseInt(errorRange.value, 10);

    try {
      const res = await fetch('/api/chaos/config', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ delayMs, errorRate, enabled: true })
      });
      const data = await res.json();
      
      updateBadgeUI(delayMs > 0 || errorRate > 0);
      showToast('Chaos settings saved successfully!', 'success');
      logToConsole(`[CONFIG] Applied latency of ${delayMs}ms and failure rate of ${errorRate}%`, 'warning');
    } catch (err) {
      logToConsole(`[ERROR] Failed to apply chaos configurations.`, 'error');
      showToast('Failed to save configs', 'error');
    }
  });

  // Disable Chaos button click
  disableChaosBtn.addEventListener('click', async () => {
    try {
      const res = await fetch('/api/chaos/config', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ delayMs: 0, errorRate: 0, enabled: false })
      });
      const data = await res.json();

      delayRange.value = 0;
      delayVal.innerText = 0;
      errorRange.value = 0;
      errorVal.innerText = 0;

      updateBadgeUI(false);
      showToast('Chaos simulations disabled.', 'success');
      logToConsole(`[CONFIG] All chaos simulations disabled. System restored to baseline.`, 'success');
    } catch (err) {
      logToConsole(`[ERROR] Failed to disable chaos simulations.`, 'error');
    }
  });

  // Burst loading button simulation (10 parallel requests)
  burstBtn.addEventListener('click', async () => {
    burstBtn.disabled = true;
    burstBtn.innerText = 'Firing 10x Load Spikes...';
    logToConsole('[BURST] Triggering 10 parallel API checkout calls to simulate concurrency...', 'info');

    const promises = [];
    const dummyCart = [
      { id: 'prod-001', quantity: 1 },
      { id: 'prod-002', quantity: 2 }
    ];

    for (let i = 0; i < 10; i++) {
      promises.push(
        fetch('/api/orders', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', 'X-Chaos-Delay': '200' }, // force a minor delay to make charts pretty
          body: JSON.stringify({
            items: dummyCart,
            customerName: `LoadTester #${i + 1}`,
            shippingAddress: 'SigNoz Ingestion Load Test Node'
          })
        })
        .then(async res => {
          const status = res.status;
          const data = await res.json();
          if (res.ok) {
            logToConsole(`[BURST #${i+1}] SUCCESS - Order Created (ID: ${data.orderId})`, 'success');
          } else {
            logToConsole(`[BURST #${i+1}] FAILURE - HTTP ${status}`, 'error');
          }
        })
        .catch(err => {
          logToConsole(`[BURST #${i+1}] FAILED - Connection Refused`, 'error');
        })
      );
    }

    await Promise.all(promises);
    logToConsole('[BURST] Concurrency load complete. Check SigNoz Traces Service Map!', 'info');
    burstBtn.disabled = false;
    burstBtn.innerText = 'Trigger 10x API Load Burst';
  });

  // Emit custom events
  window.sendCustomMetric = async function(eventName, attributes = {}) {
    logToConsole(`[TELEMETRY] Emitting manual event metric: ${eventName}...`, 'info');
    try {
      const res = await fetch('/api/telemetry/event', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: eventName,
          value: 1,
          attributes: attributes
        })
      });
      const data = await res.json();
      if (res.ok) {
        logToConsole(`[TELEMETRY] SigNoz received custom event "${eventName}" +1 successfully.`, 'success');
        showToast(`Emitted "${eventName}" metric!`, 'success');
      } else {
        throw new Error(data.error);
      }
    } catch (err) {
      logToConsole(`[ERROR] Failed to emit custom metric.`, 'error');
    }
  };

  // Helper: append output to local console log element
  function logToConsole(message, type = 'info') {
    const line = document.createElement('div');
    line.className = `terminal-line ${type}`;
    
    const timestamp = new Date().toLocaleTimeString();
    line.innerText = `[${timestamp}] ${message}`;
    
    terminalConsole.appendChild(line);
    terminalConsole.scrollTop = terminalConsole.scrollHeight;
  }

  // Clear Console logs
  clearTerminalBtn.addEventListener('click', () => {
    terminalConsole.innerHTML = '<div class="terminal-line info">[SYSTEM] Console cleared. Awaiting interactions...</div>';
  });

  function updateBadgeUI(isActive) {
    if (isActive) {
      chaosBadge.innerText = 'Chaos Active';
      chaosBadge.className = 'status-badge status-active';
    } else {
      chaosBadge.innerText = 'Standard Baseline';
      chaosBadge.className = 'status-badge status-inactive';
    }
  }

  function showToast(message, type = 'success') {
    const toast = document.getElementById('toast');
    const toastMsg = document.getElementById('toast-message');
    
    toastMsg.innerText = message;
    toast.className = 'toast show';
    
    if (type === 'success') {
      toast.classList.add('success');
      document.querySelector('.toast-icon').innerText = '✓';
    } else {
      toast.classList.add('error');
      document.querySelector('.toast-icon').innerText = '✕';
    }

    setTimeout(() => {
      toast.className = 'toast';
    }, 4000);
  }
});
