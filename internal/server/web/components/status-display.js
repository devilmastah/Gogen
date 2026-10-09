// Status-display relay (components/status-display.js).
//
// In `status_display: browser` mode the GoGen server still owns the turn
// state and builds every display row, but it does not POST them itself: the
// rows arrive over the chat socket (`status_display` frames) and THIS browser
// performs the HTTP call. That is what makes the feature work when the
// server is on a different network than the display — the browser only needs
// to reach the display.
//
// The firmware's CORS preflight only allows the Content-Type header, so the
// display token travels as a `?token=` query parameter; a custom
// `X-Status-Token` header would fail the preflight from a browser.
//
// All calls are fire-and-forget: an unreachable display must never affect
// the UI, and the server's next row is a complete snapshot.

let relay = { gate: 'auto', host: '', token: '', source: '' };

// applyStatusDisplayConfig stores the server-pushed settings. Called from
// app.js's config handling (every config push carries these).
export function applyStatusDisplayConfig(data) {
    if (!data) return;
    if (data.statusDisplay !== undefined) relay.gate = data.statusDisplay;
    if (data.statusDisplayHost !== undefined) relay.host = data.statusDisplayHost;
    if (data.statusDisplaySource !== undefined) relay.source = data.statusDisplaySource;
    // Pushed only in browser mode; undefined in the server-side modes.
    if (data.statusDisplayTokenValue !== undefined) relay.token = data.statusDisplayTokenValue;
}

// baseURL mirrors the Go side (statusdisplay.baseURL): a bare host gets
// http://, trailing slashes are trimmed.
function baseURL(host) {
    let h = String(host || '').trim();
    if (!h) return '';
    if (!h.includes('://')) h = 'http://' + h;
    return h.replace(/\/+$/, '');
}

function tokenParam() {
    return relay.token ? 'token=' + encodeURIComponent(relay.token) : '';
}

// handleStatusDisplayFrame relays one server-built display operation. Called
// from app.js's ws dispatch for `status_display` frames; a no-op unless the
// gate is `browser` and a host is configured.
export function handleStatusDisplayFrame(data) {
    if (!data || relay.gate !== 'browser') return;
    const base = baseURL(relay.host);
    if (!base) return;
    const token = tokenParam();
    if (data.statusDisplayClear) {
        const source = data.statusDisplaySource || relay.source || 'gogen';
        const query = 'source=' + encodeURIComponent(source) + (token ? '&' + token : '');
        fetch(base + '/status?' + query, { method: 'DELETE', keepalive: true }).catch(() => {});
        return;
    }
    if (!data.statusDisplayRow) return;
    fetch(base + '/status' + (token ? '?' + token : ''), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: data.statusDisplayRow,
        keepalive: true,
    }).catch(() => {});
}
