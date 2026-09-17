'use strict';
// Regression test for colorize LAYOUT PARITY (the "coloring jumps" class of
// bug): applying or removing Monaco's colorization must not change the
// element's layout — only its colors. The styler used to flip
// `.tool-result-content.monaco-colorized` to `white-space: normal`, so a
// tool result collapsed its runs of spaces and leading indentation the
// moment the color landed (and again on expand/collapse) while the plain
// state preserved them: the block visibly reflowed.
//
// Loads the real styles.css into jsdom and compares the computed
// layout-affecting properties of the plain and colorized states for both
// colored surfaces: a markdown fenced code block (`.message.md pre code`)
// and a tool-result body (`.tool-result-content`).
//
// Run: node scripts/test_color_parity.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('/tmp/gogen-jsdom/node_modules/jsdom');

const ROOT = path.join(__dirname, '..');
const CSS = fs.readFileSync(path.join(ROOT, 'internal/server/web/styles.css'), 'utf8');

// Properties that change how the text FLOWS (height/width). Paint-only
// properties (color, background) are deliberately excluded: colorizing is
// supposed to change those.
const LAYOUT_PROPS = [
  'white-space', 'display', 'font-family', 'font-size', 'font-weight',
  'font-style', 'line-height', 'letter-spacing', 'word-spacing',
  'padding-top', 'padding-bottom', 'padding-left', 'padding-right',
  'margin-top', 'margin-bottom', 'border-top-width', 'border-bottom-width',
  'overflow-wrap', 'word-break', 'tab-size',
];

let failures = 0;
function check(desc, ok, detail) {
  console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}${ok || detail === undefined ? '' : ' — ' + detail}`);
  if (!ok) failures++;
}

function main() {
  const dom = new JSDOM(`<!doctype html><html><head><style>${CSS}</style></head><body></body></html>`, {
    url: 'http://localhost/',
    pretendToBeVisual: true,
  });
  const { window } = dom;
  const doc = window.document;

  const snap = (el) => {
    const cs = window.getComputedStyle(el);
    const out = {};
    for (const p of LAYOUT_PROPS) out[p] = cs.getPropertyValue(p);
    return out;
  };
  const diff = (a, b) => LAYOUT_PROPS.filter((p) => a[p] !== b[p])
    .map((p) => `${p}: ${JSON.stringify(a[p])} vs ${JSON.stringify(b[p])}`);

  // ── 1. Markdown fenced code block ──
  {
    const mk = (colorized) => {
      const msg = doc.createElement('div');
      msg.className = 'message assistant md';
      const wrap = doc.createElement('div');
      wrap.className = 'msg-text';
      const codeWrap = doc.createElement('div');
      codeWrap.className = 'code-block-wrap';
      const pre = doc.createElement('pre');
      const code = doc.createElement('code');
      code.className = 'language-go' + (colorized ? ' monaco-colorized' : '');
      code.textContent = 'func main() {\n\tprintln("hi")\n}';
      if (colorized) code.dataset.monacoColorized = '1';
      pre.appendChild(code);
      codeWrap.appendChild(pre);
      wrap.appendChild(codeWrap);
      msg.appendChild(wrap);
      doc.body.appendChild(msg);
      return code;
    };
    const plain = snap(mk(false));
    const colored = snap(mk(true));
    const d = diff(plain, colored);
    check('markdown code block: colorized layout matches the plain state', d.length === 0, d.join('; '));
    check('markdown code block: white-space is pre-wrap once colored', colored['white-space'] === 'pre-wrap', colored['white-space']);
  }

  // ── 2. Tool-result body ──
  {
    const mk = (colorized) => {
      const card = doc.createElement('div');
      card.className = 'tool-card';
      const body = doc.createElement('div');
      body.className = 'tool-result-body';
      const content = doc.createElement('div');
      content.className = 'tool-result-content' + (colorized ? ' monaco-colorized' : '');
      content.textContent = 'ok      pkg/a   0.123s\n--- FAIL: TestX\n    x_test.go:12: want 1, got 2';
      if (colorized) content.dataset.monacoColorized = '1';
      body.appendChild(content);
      card.appendChild(body);
      doc.body.appendChild(card);
      return content;
    };
    const plain = snap(mk(false));
    const colored = snap(mk(true));
    const d = diff(plain, colored);
    check('tool result: colorized layout matches the plain state', d.length === 0, d.join('; '));
    check('tool result: white-space stays pre-wrap once colored', colored['white-space'] === 'pre-wrap', colored['white-space']);
  }

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main();
