.PHONY: fmt tidy outdated update test test-debug vet staticcheck gocyclo vuln build-nocgo lint-web test-vscode test-web check

fmt:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

tidy:
	go mod tidy -diff

# Fail if any direct dependency or declared tool has a newer version.
outdated:
	@echo "Checking for module updates..."
	@out=$$( { \
		go list -m -u -f '{{if and .Update (not .Indirect)}}{{.Path}}: {{.Version}} -> {{.Update.Version}}{{end}}' all; \
		go list -m -u -f '{{if .Update}}{{.Path}}: {{.Version}} -> {{.Update.Version}}{{end}}' \
			honnef.co/go/tools golang.org/x/vuln; \
	} | sed '/^$$/d' | sort -u ); \
	if [ -z "$$out" ]; then \
		echo "All direct modules and tools are up to date."; \
	else \
		echo "$$out"; \
		echo ""; \
		echo "Run 'make update' to upgrade."; \
		exit 1; \
	fi

# Upgrade direct deps + declared tools to newest minor/patch, then tidy.
update:
	go get -u ./...
	go get -u tool
	go mod tidy

# Test recipes run under a per-run scratch GOTMPDIR: the go command's build
# work dirs and t.TempDir() (testing.go reads GOTMPDIR) both land in it, so a
# killed or timed-out run leaves at most one /tmp/gogen-test-* dir instead of
# hundreds of stray /tmp/Test* dirs. Removed on exit and on interrupt.
GOTMPDIR_RUN = tmp=$$(mktemp -d "$${TMPDIR:-/tmp}/gogen-test-XXXXXX") || exit 1; \
	trap 'rm -rf "$$tmp"' EXIT; \
	trap 'exit 130' INT; \
	trap 'exit 143' TERM;

test:
	@$(GOTMPDIR_RUN) GOTMPDIR="$$tmp" go test -race ./...

# Packages with //go:build debug code (view-drift, profiling, WS stall knobs).
test-debug:
	@$(GOTMPDIR_RUN) GOTMPDIR="$$tmp" go test -race -tags debug ./internal/agent/ ./internal/profiling/ ./internal/server/

vet:
	go vet ./...

staticcheck:
	go tool staticcheck ./...

vuln:
	go tool govulncheck ./...

gocyclo:
	go tool gocyclo -over 15 .

# Verifies the documented no-tree-sitter build path still compiles.
build-nocgo:
	CGO_ENABLED=0 go build -o gogen-nocgo .

# Lints the hand-maintained web UI JS (app.js, editor.js, components/*.js)
# with a zero-dependency Node script (no npm, no eslint). Runs only when
# `node` is installed; `make check` stays green on machines without node.
lint-web:
	./scripts/lint-web.sh

# Sidebar helpers that do not need jsdom. The Linux CI job runs these.
# The jsdom web suite stays in test-web and is not part of CI.
test-vscode:
	node scripts/test_vscode_binary.js
	node scripts/test_vscode_template.js
	node scripts/test_vscode_bridge_cancel.js
	node scripts/test_vscode_path.js
	node scripts/test_vscode_nonce.js

# Runs the hand-maintained web UI regression tests (scripts/test_*.js):
# jsdom harnesses that load the REAL index.html + app.js +
# web/components/*.js (imports stubbed, via scripts/web-harness.js)
# and drive tab switching, background-compact state, and queued
# delete-approval Esc handling, plus the standalone state-machine tests.
# Zero-dependency (plain node + jsdom from /tmp/gogen-jsdom); skipped when
# node is unavailable so `make check` stays green on machines without it.
test-web:
	@if ! command -v node >/dev/null 2>&1; then \
		echo "test-web: node not found — skipping web UI tests"; \
		exit 0; \
	fi
	@fail=0; for f in scripts/test_*.js; do \
		echo "== $$f"; \
		if ! node "$$f"; then fail=1; fi; \
	done; \
	if [ $$fail -ne 0 ]; then echo "test-web: FAILED"; exit 1; fi; \
	echo "test-web: all web UI tests passed"

# Local full check (sequential even under make -j).
# Does not auto-update deps — use 'make update' for that.
check:
	$(MAKE) -j1 fmt tidy test test-debug vet staticcheck vuln build-nocgo lint-web test-web
