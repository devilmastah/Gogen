.PHONY: fmt tidy outdated update test test-debug vet staticcheck gocyclo vuln build-nocgo lint-web test-vscode test-web check clean jsdom

# --- Repo-local scratch ---------------------------------------------------
# Build-only go commands (build, vet, staticcheck, gocyclo, vuln, build-nocgo)
# keep their build work dirs in tmp/gotmp instead of /tmp. tmp/ is gitignored
# (.gitignore "/tmp/") and is a nested module boundary (tmp/go.mod declares
# gogen/tmp-scratch), so ./... never descends into it.
#
# This matters because go deletes its work dir only on a CLEAN exit: a build
# killed mid-flight (execute_command's idle timeout kills the whole process
# group with SIGKILL, which skips the go command's deferred cleanup) used to
# leave a /tmp/go-build* dir behind forever. Such leftovers now collect in
# $(SCRATCH), where `make clean` reaps them in one shot.
#
# Tests are deliberately NOT covered by this: t.TempDir() would then live
# inside this repo's git work tree, and the product resolves the project root
# by walking up to the nearest .git/.hg (internal/skills/skills.go), so every
# test temp dir would resolve to THIS project — git-output tests fail with
# "expected error outside a repository" and skills/spill lookups read the real
# .gogen/. Test scratch therefore stays outside the work tree (see
# GOTMPDIR_RUN below).
SCRATCH := $(CURDIR)/tmp/gotmp
export GOTMPDIR := $(SCRATCH)
$(shell mkdir -p "$(SCRATCH)")

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

# Test recipes get their own per-run scratch dir (so concurrent make runs
# cannot collide) OUTSIDE the git work tree — see the scratch note at the top
# for why. Both knobs are pinned to it: GOTMPDIR takes the go command's build
# work dirs and the per-test parents (testing.TempDir() uses
# os.MkdirTemp(os.Getenv("GOTMPDIR"), …)), TMPDIR takes everything the test
# binaries hand to os.TempDir(). The traps remove the dir on exit and on
# interrupt, so a killed run leaves one dir in $(TEST_SCRATCH) instead of
# stray /tmp/Test* and /tmp/go-build* dirs; `make clean` reaps the parent.
TEST_SCRATCH := /tmp/gogen-scratch
GOTMPDIR_RUN = mkdir -p "$(TEST_SCRATCH)"; \
	tmp=$$(mktemp -d "$(TEST_SCRATCH)/run-XXXXXX") || exit 1; \
	trap 'rm -rf "$$tmp"' EXIT; \
	trap 'exit 130' INT; \
	trap 'exit 143' TERM;

test:
	@$(GOTMPDIR_RUN) GOTMPDIR="$$tmp" TMPDIR="$$tmp" go test -race ./...

# Packages with //go:build debug code (view-drift, profiling, WS stall knobs).
test-debug:
	@$(GOTMPDIR_RUN) GOTMPDIR="$$tmp" TMPDIR="$$tmp" go test -race -tags debug ./internal/agent/ ./internal/profiling/ ./internal/server/

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
	node scripts/test_vscode_editor_bridge.js
	node scripts/test_vscode_queue_id.js
	node scripts/test_vscode_pending_sends.js
	node scripts/test_vscode_code_index_view.js

# Runs the hand-maintained web UI regression tests (scripts/test_*.js):
# jsdom harnesses that load the REAL index.html + app.js +
# web/components/*.js (imports stubbed, via scripts/web-harness.js)
# and drive tab switching, background-compact state, and queued
# delete-approval Esc handling, plus the standalone state-machine tests.
# Zero-dependency (plain node + jsdom from the repo-local tmp/gogen-jsdom,
# provisioned by `make jsdom`); skipped when node is unavailable so
# `make check` stays green on machines without it.
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

# Provisions the jsdom dependency of test-web inside tmp/ (gitignored, so it
# stays out of the tree; `make clean` leaves it alone because re-provisioning
# needs npm + network). Needs npm + node.
jsdom:
	mkdir -p tmp/gogen-jsdom
	cd tmp/gogen-jsdom && npm install jsdom --no-fund --no-audit

# Removes build scratch: $(SCRATCH) (go work dirs, including whatever a
# SIGKILLed build left behind) and $(TEST_SCRATCH) (per-run test dirs), plus
# stray test binaries at the repo root. Built gogen binaries and the jsdom
# dependency are NOT removed; `go build -o gogen .` recreates the former in
# seconds, `make jsdom` the latter.
clean:
	rm -rf "$(SCRATCH)"
	rm -rf "$(TEST_SCRATCH)"
	rm -f ./*.test

# Local full check (sequential even under make -j).
# Does not auto-update deps — use 'make update' for that.
check:
	$(MAKE) -j1 fmt tidy test test-debug vet staticcheck vuln build-nocgo lint-web test-web
