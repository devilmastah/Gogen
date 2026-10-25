#!/usr/bin/env bash
# Lint the hand-maintained web UI JavaScript and the VS Code sidebar
# scripts with a zero-dependency Node script — no npm, no eslint. Runs only
# when `node` is available; `make check` stays green on machines without node.
#
# This script installs nothing. editors/vscode/package.json exists so the
# sidebar can be packaged. It is not an npm dependency of gogen.
set -euo pipefail
cd "$(dirname "$0")/.."

if ! command -v node >/dev/null 2>&1; then
  echo "lint-web: node not found — skipping web lint (the repo has no npm dependency)"
  exit 0
fi

exec node scripts/lint-web.js
