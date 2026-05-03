// Content script — injected into every page at document_idle.
//
// Waits for the background service worker to send an {type:'init'} message
// containing the requestId and per-hostname triggers for this tab.
// Once received, uses a MutationObserver to detect when the configured CSS
// selectors appear, then sends the full DOM back to the background worker.

chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
  if (message.type !== 'init') return;

  const { requestId, triggers = {} } = message;
  const selectorList = value => Array.isArray(value) ? value : [];
  const loaded = selectorList(triggers.loaded);
  const failed = selectorList(triggers.failed);

  console.log(`[downlink] init received, requestId=${requestId}`,
    'loaded:', loaded, 'failed:', failed);

  let exported = false;

  // Returns true only if every CSS selector in the list matches at least one element.
  function allMatch(selectors) {
    if (selectors.length === 0) return false;
    return selectors.every(sel => document.querySelector(sel) !== null);
  }

  function exportDOM(state) {
    if (exported) return;
    exported = true;
    console.log(`[downlink] exporting DOM, state=${state}, requestId=${requestId}`);
    chrome.runtime.sendMessage(
      { type: 'dom-ready', requestId, state, html: document.documentElement.outerHTML },
      response => {
        if (chrome.runtime.lastError) {
          console.warn('[downlink] sendMessage error:', chrome.runtime.lastError.message);
          return;
        }
        console.log('[downlink] dom-ready ack:', response);
      }
    );
  }

  function check() {
    if (allMatch(failed)) { exportDOM('failed'); return true; }
    if (allMatch(loaded)) { exportDOM('loaded'); return true; }
    if (loaded.length === 0) { exportDOM('loaded'); return true; }
    return false;
  }

  // Ack the init message synchronously so background.js doesn't time out.
  sendResponse({ ok: true });

  // Check immediately in case the content is already rendered.
  if (check()) return;

  // Watch for DOM mutations and re-check.
  const observer = new MutationObserver(() => {
    if (check()) observer.disconnect();
  });
  observer.observe(document.body || document.documentElement,
    { childList: true, subtree: true });
});
