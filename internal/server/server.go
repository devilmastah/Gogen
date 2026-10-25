package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm"
	"gogen/internal/mcp"
	sesspkg "gogen/internal/session"

	"github.com/gorilla/websocket"
)

// spawnUserTerminal starts the interactive user shell for a WebSocket
// connection (if none is alive) and reports its lifecycle over the socket:
// user_term_opened once the shell is up, user_term_exit when it exits. Output
// is streamed as user_term_output chunks from the PTY read goroutine. The
// shell runs in the workspace's current working directory at spawn time (the
// executor has its own mutex, so no turn lock is needed).
func (s *Server) spawnUserTerminal(ws *wsConn, holder *userTermHolder) {
	if holder.get() != nil {
		return
	}
	wd := s.ws.Exec.GetWorkingDir()
	ut, err := startUserTerminal(wd, func(chunk string) {
		_ = ws.writeJSON(WSMessage{Type: "user_term_output", Content: chunk})
	})
	if err != nil {
		_ = ws.writeJSON(WSMessage{Type: "user_term_exit", Content: "failed to start shell: " + err.Error(), Code: -1})
		return
	}
	// Claim the holder atomically. Two concurrent requests (user_term_request
	// while an earlier spawn is in flight) would otherwise both spawn and the
	// loser would leak a pty; trySet fails only when a terminal is already
	// held, and the loser is closed here.
	if !holder.trySet(ut) {
		_ = ut.Close()
		return
	}
	_ = ws.writeJSON(WSMessage{Type: "user_term_opened", Content: ut.Title(), WorkingDir: wd})
	go func() {
		<-ut.Done()
		code := ut.ExitCode()
		// Only report the exit if this terminal is still the connection's
		// current one (a respawn may already have replaced it).
		if holder.clear(ut) {
			_ = ws.writeJSON(WSMessage{Type: "user_term_exit", Content: fmt.Sprintf("shell exited (%d)", code), Code: code})
		}
	}()
}

type Server struct {
	// ws is the shared workspace (executor, store, config, provider factory,
	// fsMu). registry owns the live sessions (multi-session plan §2).
	ws             *Workspace
	registry       *sessionRegistry
	config         *config.Config
	allowedOrigins map[string]struct{}
	authToken      string
	tlsCertFile    string
	tlsKeyFile     string
	wsConnsMu      sync.Mutex
	wsConns        []*websocket.Conn
	// wsLive counts the live /ws and /ws/editor handler goroutines.
	// net/http stops tracking a connection once the WebSocket upgrade
	// hijacks it, so neither httptest.Server.Close nor the http server's
	// Shutdown waits for these: a handler can still detach and flush a
	// session after the caller believed the server was down. WaitWSHandlers
	// closes that window for teardown paths.
	wsLive wsHandlerTracker
	// httpSrv is the HTTP listener this Server instance is currently serving
	// via Start. ForceClose closes it (without waiting for hijacked WebSocket
	// handlers) on Ctrl+C / ctx cancel. It is per-instance rather than
	// package-global so concurrent Servers — including the parallel tests that
	// each call Start — cannot clobber one another's listener.
	httpSrv        atomic.Pointer[http.Server]
	connLimiter    *rateLimitState
	upgradeLimiter *ipLimiter
	// bootstrapLimiter throttles the unauthenticated ?token= / ?pair=
	// bootstrap endpoints per source IP.
	bootstrapLimiter *ipLimiter
	// pairMu guards the onboarding pairing code (see pairing.go): the code,
	// its expiry, and the number of uses so far.
	pairMu     sync.Mutex
	pairCode   string
	pairExpiry time.Time
	pairMinted time.Time // when the current code was installed (SetPairingCode)
	pairUses   int
	// lastPairIP / lastPairAt remember the most recent accepted pairing
	// exchange (IP + time) so the unauthenticated-page path can recognize
	// "an exchange just succeeded from this device, yet no cookie arrived"
	// — the browser-side cookie failure (different cookie jar or blocked
	// cookies) — and diagnose it in the log and on the sign-in page.
	lastPairIP   string
	lastPairAt   time.Time
	staticAssets staticAssetCache // lazily gzip-compressed embedded assets

	// providerTestBuilder builds a throwaway provider for test_provider
	// (never registered, never wired to a session). Defaults to a real
	// OpenAIProvider; tests inject a mock builder.
	providerTestBuilder func(op ProviderOpRequest, workingDir string) (llm.LLMProvider, error)

	// mcpTestFn probes one MCP stdio server for test_mcp (never registers
	// anything). Defaults to mcp.TestServer; tests inject a stub so no real
	// process is spawned.
	mcpTestFn func(ctx context.Context, server config.MCPServerConfig) ([]llm.Tool, error)

	// reviewTrigger owns the auto-review state (the per-start mutex). nil
	// until installReviewAgent first runs; the trigger itself re-checks
	// the review-agent feature flag on every fire.
	reviewTrigger *boardReviewTrigger

	// automations owns the automation scheduler lifecycle (see
	// automation_sched.go): started with Start / the settings toggle,
	// stopped on toggle-off and shutdown.
	automations automationHost
}

func NewServer(a *agent.Agent, cfg *config.Config) *Server {
	allowed := parseAllowedOrigins("")
	token := ""
	tlsCert, tlsKey := "", ""
	if cfg != nil {
		allowed = parseAllowedOrigins(cfg.WebAllowedOrigins)
		token = strings.TrimSpace(cfg.WebAuthToken)
		tlsCert = strings.TrimSpace(cfg.WebTLSCertFile)
		tlsKey = strings.TrimSpace(cfg.WebTLSKeyFile)
	}
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GOGEN_WEB_TOKEN"))
	}
	if tlsCert == "" {
		tlsCert = strings.TrimSpace(os.Getenv("GOGEN_WEB_TLS_CERT"))
	}
	if tlsKey == "" {
		tlsKey = strings.TrimSpace(os.Getenv("GOGEN_WEB_TLS_KEY"))
	}
	ws := newWorkspaceFromAgent(a, cfg)
	maxActive := 0
	if cfg != nil {
		maxActive = cfg.WebMaxActiveSessions
	}
	reg := newSessionRegistry(maxActive)
	if a.SessionID == "" {
		a.SetSessionID(sesspkg.NewID())
	}
	rt := newSessionRuntimeWithHold(a, ws.ApprovalHold())
	reg.register(a.SessionID, rt)
	// In web mode the registry is the sole pruner: Save's
	// internal auto-prune protects only one session and could delete another
	// live session's file; the registry prunes with the full active set.
	if ws.Store != nil {
		ws.Store.SetAutoPrune(false)
	}
	// Wrap FS-mutating tools with the workspace filesystem lock:
	// a streaming turn no longer blocks editor saves except during the actual
	// mutation window of a tool.
	a.SetToolHandlers(ws.buildToolHandlers())
	s := &Server{
		ws:               ws,
		registry:         reg,
		config:           cfg,
		allowedOrigins:   allowed,
		authToken:        token,
		tlsCertFile:      tlsCert,
		tlsKeyFile:       tlsKey,
		connLimiter:      newRateLimitState(defaultMaxWSConns),
		upgradeLimiter:   newIPLimiter(5, 10), // 5 upgrades/sec/IP, burst 10
		bootstrapLimiter: newIPLimiter(1, 5),  // 1 bootstrap/sec/IP, burst 5
	}
	s.providerTestBuilder = func(op ProviderOpRequest, workingDir string) (llm.LLMProvider, error) {
		return llm.NewOpenAIProvider(op.APIKey, op.Model, op.BaseURL, workingDir), nil
	}
	s.mcpTestFn = mcp.TestServer
	// Browser-mode status display: the web server (not main) owns the
	// startup application, because only it can install the relay sink.
	s.applyStatusDisplayStartup()
	// Background model validation for a restored default session runs after
	// the server starts; push the result to the session's clients so the
	// toolbar does not keep showing a model that was cleared or replaced by
	// the validation.
	a.OnModelChanged = func() {
		s.pushConfigForAgent(a)
		s.maybeProbeReasoningEfforts(context.Background(), a)
	}
	// Agent board-tool mutations broadcast a fresh board_state so every open
	// kanban tab stays live, plus a success notice (toast) so the user sees
	// agent-triggered changes even when they didn't initiate them (the
	// initial agent, and every session created later via NewSessionAgent).
	ws.BoardChangedHook = func(msg string) {
		s.broadcastBoardState()
		s.broadcastBoardNotice(msg)
	}
	a.SetOnBoardChanged(ws.BoardChangedHook)
	// Auto review agent: a ticket moved into in_review spawns a headless
	// review session (web host only — the TUI has no session spawner and
	// never installs the trigger). The trigger re-checks the review-agent
	// feature flag on every fire, so the settings toggle is live.
	s.installReviewAgent()
	// The initial agent must read the WORKSPACE's single shared flag store
	// (not the per-process values setup.go seeded from cfg) and use the
	// WORKSPACE's single shared board manager — not the per-process manager
	// setup.go created for it. Two managers over the same board directory
	// would split the in-process serialization (claims/moves/NextID) between
	// the first session and every later session + the web board tab.
	// Re-pointing here also keeps the initial agent consistent when
	// NewServer is fed an agent built outside newAgent (tests/embeds).
	a.SetFeatureFlags(ws.FeatureFlags())
	a.SetBoardManager(ws.GetBoardManager())
	a.SetSkillsManager(ws.skillsManager)
	a.SetTerminalEnabled(ws.terminalEnabled)
	// The subagent spawner needs the registry, so it is installed after the
	// server is constructed; NewSessionAgent seeds it on every later session.
	// The spawner is the host half of the shared continuable-subagent
	// lifecycle, so the lifecycle is constructed around it (the host pointer
	// is required up front).
	spawner := newSubagentSpawner(s)
	ws.SubagentSpawner = spawner
	a.SetSubagentSpawner(ws.SubagentSpawner)
	// A session leaving the registry takes its continuable subagent
	// children with it (cancel + release): a child whose parent is gone
	// cannot deliver report/completion anyway. The EXPLICIT teardown paths
	// (session_close, delete, shutdown) plus registry-cap eviction fire it.
	s.registry.evictHook = func(id string) {
		if sp, ok := ws.SubagentSpawner.(*subagentSpawner); ok {
			sp.CancelAll(id)
		}
	}
	// Background work pins its session: an eviction that only recycles
	// memory (pane switch, page refresh, cap pressure) must not destroy a
	// live background child. Agent.HasBackgroundWork covers shell jobs and
	// terminals; this covers the children cancelAll would kill.
	s.registry.bgWorkHook = func(id string) bool {
		if sp, ok := ws.SubagentSpawner.(*subagentSpawner); ok {
			return sp.HasLiveChildren(id)
		}
		return false
	}
	// Job-completion notices: the deliverer resolves a session id to its
	// live runtime; NewSessionAgent installs per-agent hooks on top of it
	// for later sessions, this install covers the initial agent.
	if ws.jobNotices {
		ws.jobNoticeDeliverer = func(id, summary string) {
			if rt, ok := s.registry.get(id); ok {
				rt.deliverToSession(summary)
			}
		}
		a.SetJobNoticeHook(func(summary string) {
			ws.jobNoticeDeliverer(a.SessionID, summary)
		})
	}
	return s
}

// SetDefaultModel updates the workspace default model that new session
// providers are seeded from. Called by the web startup validation goroutine
// (runWeb) once ValidateRestoredModel has resolved the effective model
// (possibly auto-selected or cleared), so a session created mid-validation
// never reads a half-updated seed. Synchronized on the workspace with
// session creation (ProviderFactory reads DefaultModel).
func (s *Server) SetDefaultModel(name string) {
	s.ws.SetDefaultModel(name)
}

// newSessionRuntimeFor builds a session runtime carrying the server's
// configured approval-hold window (see web_approval_hold_secs / F2). Reads
// the live runtime overlay, so a settings-modal change applies to runtimes
// created afterwards.
func (s *Server) newSessionRuntimeFor(a *agent.Agent) *sessionRuntime {
	return newSessionRuntimeWithHold(a, s.ws.ApprovalHold())
}

// wsRequest is the connection-scoped context for one inbound message: the
// transport (server/conn/request), the decoded message (msg), the id-resolved
// runtime (target — nil for stale ids, which handlers drop), and the
// connection's interactive user terminal (holder, for user_term_*).
//
// pane is the connection's current session and the single source of truth for
// it across the read loop: the dispatch loop builds one wsRequest and reuses
// it for every message, so a handler that re-aligns the pane (the
// fork/edit-resend path) persists that change to the next message. Such a
// handler assigns req.pane directly; when it delegates to a session-ops
// helper that ALSO re-aligns the pane, it passes &req.pane — those helpers
// keep their **sessionRuntime contract, which is shared with the connect
// handshake and other non-dispatch callers.
type wsRequest struct {
	server  *Server
	conn    *wsConn
	request *http.Request
	msg     WSMessage
	target  *sessionRuntime
	pane    *sessionRuntime
	holder  *userTermHolder
}

// wsMessageHandler dispatches one inbound message type. A handler's return is
// equivalent to the old switch's `continue`: the message is consumed.
type wsMessageHandler func(req *wsRequest)

// wsHandlerKind tags which handler family serves a message type. Go only
// allows comparing function values to nil, so tests assert socket routing
// through this tag instead of the handler itself.
type wsHandlerKind string

const (
	wsKindChat    wsHandlerKind = "chat"
	wsKindFSRead  wsHandlerKind = "fs-read"
	wsKindFSWrite wsHandlerKind = "fs-write"
)

type wsHandlerEntry struct {
	handle wsMessageHandler
	kind   wsHandlerKind
}

// resolveRuntime returns the session runtime for id. An EMPTY id targets the
// default (first registered) session — legacy messages without a sessionId.
// An id that is not registered (the session was evicted by the active-cap or
// deleted) must NOT fall back to the default: session-scoped operations
// (cancel, session_detach, session_close, set_model/mode/thinking, compact,
// approvals) would otherwise silently hit the WRONG session — e.g. a pane
// closed after eviction sending session_detach for the evicted id would
// detach the default session from the connection, and a cancel for an
// evicted session would kill the default session's turn. Unknown non-empty
// ids return nil; callers treat that as a stale operation and ignore it.
func (s *Server) resolveRuntime(id string) *sessionRuntime {
	if id == "" {
		return s.registry.first()
	}
	rt, ok := s.registry.get(id)
	if !ok {
		return nil
	}
	return rt
}

// attachKnown is the client's conditional-attach claim: the history epoch
// and newest shipped message index its pane has already rendered (from the
// last history payload it applied). Both fields must be present to count.
//
// When they still match the session's HistoryFingerprint at attach time, the
// transcript part of the payload is skipped entirely (or reduced to a
// rewind-only frame) — a pane switch back to a cached, unchanged session
// re-renders nothing and ships nothing.
type attachKnown struct {
	epoch uint64
	index int
}

// attachSession registers a socket as a viewer of the session and sends the
// attach payload: session_state first (turnActive for "resuming…" rendering,
// E28), then — asynchronously, because a running turn holds the session turn
// lock and the handshake must never block on it — the cheap config snapshot,
// the persisted history, and the full config with context stats (mirrors the
// connect handshake; N panes do not serialize N tokenizations). A reconnect
// mid-turn therefore gets session_state immediately, the live stream events,
// and the completed history when the turn ends.
//
// sendHistory selects the payload shape: a full attach (true, used by the
// connect handshake and the active pane) includes the history snapshot +
// rewind so the client can rebuild the transcript; a lightweight re-register
// (false) skips it. The client uses the lightweight form for BACKGROUND panes
// on reconnect, whose transcript re-derives from a full attach when focused —
// the (potentially multi-MB) snapshot would be discarded client-side, so it
// is neither built nor sent.
//
// known carries the conditional-attach claim (nil = absent → always send
// the full snapshot). The history part itself is written by
// sendAttachHistory below.
//
// It returns false when the attach was refused — the runtime is closing (a
// claimed session_close) or already evicted, i.e. leaving memory — and
// NOTHING is sent for it (no state, history, or config): the caller reports
// session_detached instead, so the client drops the pane instead of
// building it on a runtime the imminent eviction removes from under it.
func (s *Server) attachSession(ws *wsConn, r *http.Request, rt *sessionRuntime, sendHistory bool, known *attachKnown) bool {
	if !rt.attach(ws) {
		return false
	}
	s.sendSessionState(ws, rt)
	s.sendAttachPayload(r.Context(), ws, rt, sendHistory, known)
	return true
}

// sendAttachPayload ships the attach sequence's second half — the history
// snapshot (full or conditional), the config pair, and the reasoning-effort
// probe — ASYNCHRONOUSLY, so a running turn (which holds the session turn
// lock for its whole duration) can never delay the handshake or a pane
// switch. Callers send session_state first (sendSessionState) and are
// responsible for the attach bookkeeping (rt.attach/switchPane).
//
// Shared by the socket handshake (attachSession) and sessionResume: a
// resume's reply IS the attach sequence (the pane re-keys off session_state
// and the history/config frames re-derive its transcript), so the two must
// stay byte-identical — one code path, no drift.
func (s *Server) sendAttachPayload(ctx context.Context, ws *wsConn, rt *sessionRuntime, sendHistory bool, known *attachKnown) {
	go func() {
		if sendHistory {
			s.sendAttachHistory(ws, rt, known)
		}
		// Config echo: no turnMu — every field is internally synchronized
		// (agentConfigMsgBasic), so a mid-turn attach gets the session's
		// identity/toolbar state immediately instead of when the turn ends.
		// Only the context-stats badge may lag (tokenization of a freshly
		// restored session runs in agentConfigMsg below). For a lightweight
		// re-register (sendHistory=false) the session_state sent above already
		// told the client whether the session is mid-turn; these frames just
		// refresh the pane's toolbar/context mirrors.
		basic := agentConfigMsgBasic(rt.agent)
		full := agentConfigMsg(ctx, rt)
		s.decorateConfig(&basic)
		s.decorateConfig(&full)
		_ = ws.writeJSON(basic)
		_ = ws.writeJSON(full)
		// Derive llama.cpp reasoning-effort options for the (possibly
		// restored) model and push a correction when they differ from the
		// fallback set the configs above carried.
		s.maybeProbeReasoningEfforts(ctx, rt.agent)
	}()
}

// sendAttachHistory writes the history part of an attach payload: the full
// snapshot, a rewind-only frame, or nothing at all.
//
// The conditional path (known != nil) compares the client's fingerprint
// claim against agent.HistoryFingerprint() — one consistent (epoch, last
// shipped index) read. Equality proves the client's rendered transcript is
// still current: the epoch covers wholesale reshapes (compaction/restore/
// rollback/fork) and the shipped index covers appends, which never bump the
// epoch. A matched idle session then needs NO history frame at all; with an
// in-flight round's partial output, only the rewind ships (rewind-only) so
// the client merges it onto its cached transcript. Everything else takes the
// full-snapshot path unchanged.
func (s *Server) sendAttachHistory(ws *wsConn, rt *sessionRuntime, known *attachKnown) {
	epoch, lastShipped := rt.agent.HistoryFingerprint()
	current := known != nil && known.epoch == epoch && known.index == lastShipped && lastShipped >= 0
	if current {
		// The client's transcript is current. With no in-flight round output
		// and no turn, neither build (SnapshotMessages deep clone) nor ship
		// the snapshot at all; with round output, ship ONLY the rewind so
		// the client merges it onto its cached transcript instead of
		// clearing and replaying everything.
		//
		// The rewind is only meaningful while a round is IN FLIGHT: an idle
		// session's round buffer still holds the LAST round's output (it is
		// cleared at the next turn's start), and shipping it to a client
		// whose fingerprint says it is current would make that client merge
		// output it already rendered. Idle + current → nothing to send.
		if active, _ := rt.turnState(); !active {
			return
		}
		rewind := rt.live.Snapshot()
		if rewind != nil {
			_ = ws.writeJSON(WSMessage{
				Type:         "history",
				RewindOnly:   true,
				HistoryEpoch: epoch,
				Rewind:       rewind,
				SessionID:    rt.agent.SessionID,
			})
			return
		}
		// Mid-turn with an empty round buffer: either between rounds or
		// before the round's first token. Re-read the fingerprint: appends
		// that landed behind the first read (the same window the plain
		// attach has between its session_state write and the
		// SnapshotMessages clone) must fall back to the full snapshot, not
		// a skip that would strand the client without them.
		if epoch2, lastShipped2 := rt.agent.HistoryFingerprint(); epoch2 == epoch && lastShipped2 == lastShipped {
			return
		}
	}
	// Snapshot and send history FIRST, without the turn lock. A running
	// turn holds turnMu for its ENTIRE duration (startTurn defers the
	// unlock), so taking turnMu.RLock here would leave a mid-turn page
	// open / reconnect with an empty transcript until the turn finishes
	// — minutes for a long agent run, or indefinitely for a stuck turn.
	// SnapshotMessages deep-clones under its own statsMu (its documented
	// contract: web history snapshots never hold turnMu), so the
	// transcript is consistent with the live stream and paints at once.
	msgs := rt.agent.SnapshotMessages()
	if len(msgs) > 0 {
		_ = ws.writeJSON(WSMessage{
			Type:         "history",
			History:      historyEntries(msgs),
			HistoryEpoch: rt.agent.HistoryEpoch(),
			// The in-flight turn's partial output, so a mid-turn attach
			// shows the current reply instead of "Resuming…" with no
			// context until the turn ends (nil between rounds).
			Rewind:    rt.live.Snapshot(),
			SessionID: rt.agent.SessionID,
		})
	}
}

func (s *Server) handleWSSessionAction(ws *wsConn, ctx context.Context, pane **sessionRuntime, msg WSMessage) {
	var cmd string
	switch msg.Type {
	case "session_new":
		cmd = "/new"
	case "session_resume", "session_delete":
		if strings.TrimSpace(msg.SessionID) == "" {
			_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: sessionId is required"})
			return
		}
		if msg.Type == "session_resume" {
			cmd = "resume " + strings.TrimSpace(msg.SessionID)
		} else {
			cmd = "resume del " + strings.TrimSpace(msg.SessionID)
		}
	case "session_fork":
		forkArg := fmt.Sprintf("%d", msg.MessageIndex)
		if msg.MessageIndex < 0 {
			forkArg = "last"
		}
		cmd = "fork " + forkArg
	}

	// Registry lifecycle ops: no turn lock needed — new/resume/
	// fork leave the previous session running (continuation), delete drains
	// its own target.
	result, _, err := s.runSessionCommand(ctx, ws, pane, cmd)
	s.writeSessionCommandResult(ws, ctx, *pane, result, err)
}

// Start serves the web UI until ctx is cancelled or the listener fails.
// On cancel it force-closes WebSockets and the HTTP listener so shutdown
// is not blocked by hijacked connections, then returns so the caller can
// FlushSession (Ctrl+C / SIGTERM in --web mode).
func (s *Server) Start(ctx context.Context, addr string) error {
	if !IsLoopbackBind(addr) {
		if s.authToken == "" {
			return fmt.Errorf("non-loopback bind %q requires an auth token; set GOGEN_WEB_TOKEN or web_auth_token", addr)
		}
		if s.tlsCertFile == "" || s.tlsKeyFile == "" {
			log.Printf("WARNING: non-loopback bind %q without TLS — auth token is sent in plain text. Set GOGEN_WEB_TLS_CERT and GOGEN_WEB_TLS_KEY (or web_tls_cert_file / web_tls_key_file) for encryption.", addr)
		}
		log.Printf("listening on non-loopback %s with token auth", addr)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.HandleWS)
	mux.HandleFunc("/ws/editor", s.HandleWSEditor)
	mux.HandleFunc("/", s.HandleStatic)
	// Slowloris hardening for the plain HTTP surface (static assets and the
	// pairing redirect/page). ReadHeaderTimeout bounds how long a client may
	// take to send its request headers; IdleTimeout bounds how long an idle
	// keep-alive connection is held between requests. Neither applies to the
	// WebSocket endpoints: the upgrade hijacks the connection during the
	// handshake, after which the http.Server no longer manages its
	// deadlines (gorilla/websocket sets its own). ReadTimeout/WriteTimeout
	// are deliberately left unset so no whole-request deadline can interfere
	// with a hijacked connection's own framing.
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		var err error
		if s.tlsCertFile != "" && s.tlsKeyFile != "" {
			err = srv.ListenAndServeTLS(s.tlsCertFile, s.tlsKeyFile)
		} else {
			err = srv.ListenAndServe()
		}
		errCh <- err
	}()

	s.trackHTTPServer(srv)
	defer s.untrackHTTPServer()

	// Automation scheduler: sweep the global store while the server runs
	// (only when the automations feature flag is on; the settings toggle
	// can start/stop it live). Nil-safe for bare-Server tests.
	if s.ws != nil && s.ws.GetAutomationsEnabled() {
		s.startAutomations()
	}
	defer s.stopAutomations()

	select {
	case <-ctx.Done():
		s.ForceClose()
		select {
		case err := <-errCh:
			if err == nil || errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-time.After(100 * time.Millisecond):
			return nil
		}
	case err := <-errCh:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
