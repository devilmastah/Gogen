'use strict';
// Regression test: the status-display relay (components/status-display.js).
//
// In `status_display: browser` mode the GoGen server builds the rows and
// sends `status_display` frames over the chat socket; this browser performs
// the actual HTTP call to the display. The token must travel as `?token=`
// because the firmware's CORS preflight only allows the Content-Type header
// (a custom X-Status-Token header would fail the preflight).
//
// Loads the REAL module into jsdom with a recording fetch stub and drives:
//   1. browser mode → POST of the server-built row with the token query
//   2. clear → DELETE with source + token
//   3. other gates (auto/on/off) ignore frames
//   4. host normalization (full URL, trailing slash) and no-token requests
//   5. no host → no call
//
// Run: node scripts/test_status_display_relay.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('/tmp/gogen-jsdom/node_modules/jsdom');
const { ROOT, stripModuleSyntax } = require('./web-harness');

let failures = 0;
function check(cond, msg) {
  if (cond) {
    console.log('  ok   ' + msg);
  } else {
    failures++;
    console.error('  FAIL ' + msg);
  }
}

const dom = new JSDOM('<!doctype html><html><body></body></html>', { url: 'http://localhost/', runScripts: 'dangerously' });
const { window } = dom;
const calls = [];
window.fetch = (url, opts) => {
  calls.push({ url, opts });
  return Promise.resolve({ ok: true });
};
window.eval(stripModuleSyntax(fs.readFileSync(path.join(ROOT, 'internal/server/web/components/status-display.js'), 'utf8')));
const { applyStatusDisplayConfig, handleStatusDisplayFrame } = window;

// 1. Browser mode + a bare mDNS host: POST the row, token as a query param.
applyStatusDisplayConfig({
  statusDisplay: 'browser',
  statusDisplayHost: 'statusscreen.local',
  statusDisplayTokenValue: 'sekret',
  statusDisplaySource: 'gogen-ttgo',
});
handleStatusDisplayFrame({
  statusDisplayRow: '{"source":"gogen-ttgo","state":"running"}',
  statusDisplaySource: 'gogen-ttgo',
});
check(calls.length === 1, 'browser mode relays the row');
check(calls[0] && calls[0].url === 'http://statusscreen.local/status?token=sekret',
  'POST url is http://<host>/status?token=… (got ' + (calls[0] && calls[0].url) + ')');
check(calls[0] && calls[0].opts.method === 'POST', 'row uses POST');
check(calls[0] && calls[0].opts.body === '{"source":"gogen-ttgo","state":"running"}',
  'row body is the server-built JSON verbatim');
check(calls[0] && calls[0].opts.headers && calls[0].opts.headers['Content-Type'] === 'application/json',
  'row carries the JSON content type');
check(calls[0] && !calls[0].opts.headers['X-Status-Token'],
  'no custom token header (would fail the firmware preflight)');

// 2. Clear: DELETE with the source and the token.
handleStatusDisplayFrame({ statusDisplayClear: true, statusDisplaySource: 'gogen-ttgo' });
check(calls.length === 2 && calls[1].url === 'http://statusscreen.local/status?source=gogen-ttgo&token=sekret',
  'clear is DELETE /status?source=…&token=… (got ' + (calls[1] && calls[1].url) + ')');
check(calls[1] && calls[1].opts.method === 'DELETE', 'clear uses DELETE');

// 3. Other gates ignore the frames entirely.
applyStatusDisplayConfig({ statusDisplay: 'auto' });
handleStatusDisplayFrame({ statusDisplayRow: '{"x":1}' });
check(calls.length === 2, 'non-browser gates ignore frames');

// 4. Host normalization: a full URL with a trailing slash, and no token.
applyStatusDisplayConfig({ statusDisplay: 'browser', statusDisplayHost: 'http://192.168.1.205/', statusDisplayTokenValue: '' });
handleStatusDisplayFrame({ statusDisplayRow: '{"x":1}' });
check(calls.length === 3 && calls[2].url === 'http://192.168.1.205/status',
  'full URL is normalized and the token query is omitted (got ' + (calls[2] && calls[2].url) + ')');

// 5. No host: nothing to relay to.
applyStatusDisplayConfig({ statusDisplayHost: '' });
handleStatusDisplayFrame({ statusDisplayRow: '{"x":1}' });
check(calls.length === 3, 'no host means no call');

console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
process.exit(failures === 0 ? 0 : 1);
