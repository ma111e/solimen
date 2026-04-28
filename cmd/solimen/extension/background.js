// Background service worker — runs in the extension's origin (chrome-extension://),
// which is not subject to Chrome's Private Network Access restrictions.
//
// Architecture: maintains a persistent WebSocket connection to the Go server.
// When a {type:'scrape'} command arrives, opens a new background tab, injects
// the per-request triggers into the content script via chrome.tabs.sendMessage,
// waits for the DOM, POSTs it back to the server, then closes the tab.

const WS_RECONNECT_BASE_MS = 1000;
const WS_RECONNECT_MAX_MS  = 30000;

let port = null;
let ws   = null;
let reconnectDelay = WS_RECONNECT_BASE_MS;

// tabId → { requestId, triggers, timeoutHandle }
const pendingTabs = new Map();

// ── Startup ──────────────────────────────────────────────────────────────────

// Keep the service worker alive from the moment it starts; Chrome suspends it
// after ~30s of inactivity. An alarm firing every ~24s prevents suspension and
// must be created before any async work so it exists even if config fails.
chrome.alarms.create('keepalive', { periodInMinutes: 0.4 });

(async () => {
  try {
    const resp = await fetch(chrome.runtime.getURL('runtime-config.json'));
    const cfg  = await resp.json();
    port = cfg.port;
    console.log(`[downlink:bg] loaded config, port=${port}`);
  } catch (e) {
    console.error('[downlink:bg] failed to load runtime-config.json:', e);
    return;
  }

  connectWS();
})();

chrome.runtime.onStartup.addListener(() => {
  chrome.tabs.create({ url: chrome.runtime.getURL('landing.html'), active: true });
});

chrome.alarms.onAlarm.addListener(alarm => {
  if (alarm.name === 'keepalive') {
    // No-op — waking up is sufficient to reset the suspension timer.
    // Optionally reconnect if the WS dropped while the SW was suspended.
    if (ws === null && port !== null) {
      console.log('[downlink:bg] keepalive alarm: WebSocket gone, reconnecting');
      connectWS();
    }
  }
});

// ── WebSocket ─────────────────────────────────────────────────────────────────

function connectWS() {
  console.log(`[downlink:bg] connecting WebSocket to ws://127.0.0.1:${port}/ws`);
  ws = new WebSocket(`ws://127.0.0.1:${port}/ws`);

  ws.onopen = () => {
    console.log('[downlink:bg] WebSocket connected');
    reconnectDelay = WS_RECONNECT_BASE_MS;
  };

  ws.onmessage = event => handleWSMessage(event);

  ws.onclose = ws.onerror = () => {
    ws = null;
    console.log(`[downlink:bg] WebSocket disconnected, reconnecting in ${reconnectDelay}ms`);
    setTimeout(connectWS, reconnectDelay);
    reconnectDelay = Math.min(reconnectDelay * 2, WS_RECONNECT_MAX_MS);
  };
}

async function handleWSMessage(event) {
  let msg;
  try {
    msg = JSON.parse(event.data);
  } catch (e) {
    console.error('[downlink:bg] failed to parse WS message:', e);
    return;
  }

  if (msg.type === 'ping') return;
  if (msg.type !== 'scrape') {
    console.warn('[downlink:bg] unknown message type:', msg.type);
    return;
  }

  const { requestId, url, triggers } = msg;
  console.log(`[downlink:bg] scrape ${requestId} → ${url}`);

  // Set up per-tab timeout (slightly under the server's 30s to let it log first).
  const timeoutHandle = setTimeout(() => {
    if (!pendingTabs.has(tabId)) return;
    pendingTabs.delete(tabId);
    console.error(`[downlink:bg] timeout for tab ${tabId} (${requestId})`);
    chrome.tabs.remove(tabId).catch(() => {});
  }, 28000);

  // Open a background tab. Using `let` so the tabId variable is accessible
  // in the closure above via the outer scope after reassignment below.
  let tabId;
  try {
    const tab = await chrome.tabs.create({ url, active: false });
    tabId = tab.id;
  } catch (e) {
    clearTimeout(timeoutHandle);
    console.error('[downlink:bg] failed to create tab:', e);
    return;
  }

  pendingTabs.set(tabId, { requestId, triggers, timeoutHandle });

  // Race-safe init: the tab may already be complete by the time we reach here.
  const currentTab = await chrome.tabs.get(tabId).catch(() => null);
  if (currentTab && currentTab.status === 'complete' && pendingTabs.has(tabId)) {
    sendInitMessage(tabId, pendingTabs.get(tabId));
  }
}

// ── Tab lifecycle ─────────────────────────────────────────────────────────────

// Persistent listener (registered at module load) handles the normal path where
// the tab hasn't finished loading by the time handleWSMessage returns.
chrome.tabs.onUpdated.addListener((tabId, changeInfo) => {
  if (changeInfo.status !== 'complete') return;
  const pending = pendingTabs.get(tabId);
  if (!pending) return;
  sendInitMessage(tabId, pending);
});

async function sendInitMessage(tabId, pending) {
  // Atomically consume the pending entry — JS is single-threaded so this is safe.
  // Prevents double-init if both the onUpdated listener and the post-create check fire.
  if (!pendingTabs.has(tabId)) return;
  pendingTabs.delete(tabId);
  clearTimeout(pending.timeoutHandle);

  const { requestId, triggers } = pending;

  try {
    await chrome.tabs.sendMessage(tabId, { type: 'init', requestId, triggers });
  } catch (err) {
    // Content script injection may not have completed yet; retry once.
    await new Promise(r => setTimeout(r, 200));
    try {
      await chrome.tabs.sendMessage(tabId, { type: 'init', requestId, triggers });
    } catch (err2) {
      console.error(`[downlink:bg] failed to send init to tab ${tabId} (${requestId}):`, err2);
      chrome.tabs.remove(tabId).catch(() => {});
    }
  }
}

// ── Status queries (from landing page) ───────────────────────────────────────

chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
  if (message.type !== 'get-status') return false;
  sendResponse({
    connected:   ws !== null && ws.readyState === WebSocket.OPEN,
    pendingTabs: pendingTabs.size,
  });
  return false; // synchronous — do not keep the channel open
});

// ── DOM receipt ───────────────────────────────────────────────────────────────

chrome.runtime.onMessage.addListener((message, sender, sendResponse) => {
  if (message.type !== 'dom-ready') return;

  const { requestId, state, html } = message;
  const tabId = sender.tab?.id;
  console.log(`[downlink:bg] dom-ready from tab ${tabId} (${requestId}), state=${state}`);

  fetch(`http://127.0.0.1:${port}/dom`, {
    method: 'POST',
    headers: {
      'Content-Type':       'text/html',
      'X-Downlink-Request-Id':  requestId,
      'X-Downlink-State':       state,
    },
    body: html,
  })
    .then(() => {
      console.log(`[downlink:bg] DOM posted for ${requestId}`);
      sendResponse({ ok: true });
    })
    .catch(err => {
      console.error(`[downlink:bg] failed to POST DOM for ${requestId}:`, err);
      sendResponse({ ok: false, error: err.message });
    })
    .finally(() => {
      if (tabId != null) chrome.tabs.remove(tabId).catch(() => {});
    });

  return true; // keep message channel open for the async response
});
