#!/usr/bin/env node
'use strict';

// Zero-dependency lint for the hand-maintained web UI JS (app.js,
// editor.js, components/*.js) and the VS Code sidebar scripts.
// Runs with plain `node` — no npm, no eslint. Checks:
//   1. Syntax (`node --check`).
//   2. No stray debug logging: console.log/debug/info/trace are flagged;
//      console.warn/error are allowed (the UI's error-reporting path).
//   3. No `debugger` statements.
//   4. Best-effort unused-declaration detection for `const`/`let`/`var` and
//      function declarations whose name never appears elsewhere in the file.
//      Exported names are skipped (module public API consumed by the other
//      file); destructuring and `_`-prefixed names are skipped.
// Exits 1 on any finding. Run via scripts/lint-web.sh.

const { execFileSync } = require('node:child_process');
const fs = require('node:fs');

const WEB = 'internal/server/web';
const componentFiles = fs.existsSync(`${WEB}/components`)
  ? fs.readdirSync(`${WEB}/components`)
      .filter((f) => f.endsWith('.js'))
      .map((f) => `${WEB}/components/${f}`)
      .sort()
  : [];
const vscodeDir = 'editors/vscode';
const vscodeFiles = fs.existsSync(vscodeDir)
  ? fs.readdirSync(vscodeDir)
      .filter((f) => f.endsWith('.js'))
      .map((f) => `${vscodeDir}/${f}`)
      .sort()
  : [];
const FILES = process.argv.slice(2).length > 0
  ? process.argv.slice(2)
  : [`${WEB}/app.js`, `${WEB}/editor.js`, ...componentFiles, ...vscodeFiles];

let problems = 0;

function report(file, line, message) {
  console.error(`${file}:${line}: ${message}`);
  problems++;
}

// --- 1. Syntax check ---
for (const file of FILES) {
  try {
    execFileSync(process.execPath, ['--check', file], { stdio: 'pipe' });
  } catch (err) {
    const msg = (err.stderr && err.stderr.toString().trim()) || `syntax error in ${file}`;
    console.error(`${file}: ${msg}`);
    problems++;
  }
}

// --- 2-4. Source scans ---
function lintFile(file) {
  const src = fs.readFileSync(file, 'utf8');
  const lines = src.split('\n');

  lines.forEach((line, i) => {
    const lineNo = i + 1;
    const m = line.match(/console\.(log|debug|info|trace)\s*\(/);
    if (m) {
      report(file, lineNo, `console.${m[1]}() — stray debug logging; use console.warn/error or remove`);
    }
    if (/^\s*debugger\s*;?\s*$/.test(line)) {
      report(file, lineNo, 'debugger statement');
    }
  });

  const declRe = /^\s*(export\s+)?(?:const|let|var|function|async\s+function)\s+([A-Za-z_$][\w$]*)/;
  lines.forEach((line, i) => {
    const trimmed = line.trim();
    if (trimmed.startsWith('//') || trimmed.startsWith('/*') || trimmed.startsWith('*')) return;
    const m = line.match(declRe);
    if (!m) return;
    const exported = !!m[1];
    const name = m[2];
    if (exported || name.startsWith('_')) return;
    const re = new RegExp('(?<![\\w$])' + name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '(?![\\w$])', 'g');
    let count = 0;
    let mm;
    while ((mm = re.exec(src)) !== null) count++;
    if (count <= 1) {
      report(file, i + 1, `'${name}' is declared but never used`);
    }
  });
}

for (const file of FILES) {
  lintFile(file);
}

if (problems > 0) {
  console.error(`\n${problems} problem(s) in web UI JS.`);
  process.exit(1);
}
console.log(`lint-web: OK (${FILES.join(', ')})`);
