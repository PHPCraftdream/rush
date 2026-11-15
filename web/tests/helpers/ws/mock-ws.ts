import { Page } from "@playwright/test";

export interface MockWSMessage {
  type: string;
  payload?: unknown;
  id?: string;
}

/**
 * Intercepts only /ws WebSocket connections while letting other WS (e.g. RSBuild HMR) pass through.
 * Uses a selective mock: only connections to URLs containing "/ws" are mocked.
 *
 * window.__wsSent  — messages sent by the page (client→server)
 * window.__mockWS  — current /ws mock instance
 */
export async function setupMockWS(page: Page) {
  await page.addInitScript(`
    window.__wsSent = [];
    window.__wsReceived = [];
    window.__mockWS = null;

    var _OriginalWebSocket = window.WebSocket;

    function MockAppWebSocket(url) {
      this.url = url;
      this.readyState = 0;
      this.onopen = null;
      this.onmessage = null;
      this.onclose = null;
      this.onerror = null;
      window.__mockWS = this;
      var self = this;
      setTimeout(function() {
        self.readyState = 1;
        if (self.onopen) self.onopen(new Event('open'));
      }, 10);
    }
    MockAppWebSocket.prototype.send = function(data) {
      if (this.readyState !== 1) return;
      try { window.__wsSent.push(JSON.parse(data)); } catch(e) {}
    };
    MockAppWebSocket.prototype.close = function() {
      this.readyState = 3;
      if (this.onclose) this.onclose(new CloseEvent('close', { wasClean: true }));
    };
    MockAppWebSocket.CONNECTING = 0;
    MockAppWebSocket.OPEN = 1;
    MockAppWebSocket.CLOSING = 2;
    MockAppWebSocket.CLOSED = 3;

    window.WebSocket = function SelectiveWebSocket(url, protocols) {
      // Only intercept the app's /ws endpoint; pass all others (e.g. rsbuild HMR) through.
      var urlStr = String(url);
      if (urlStr.indexOf('/ws') !== -1 && urlStr.indexOf('rsbuild') === -1) {
        return new MockAppWebSocket(url);
      }
      return new _OriginalWebSocket(url, protocols);
    };
    window.WebSocket.CONNECTING = 0;
    window.WebSocket.OPEN = 1;
    window.WebSocket.CLOSING = 2;
    window.WebSocket.CLOSED = 3;
    window.WebSocket.prototype = _OriginalWebSocket.prototype;
  `);
}
