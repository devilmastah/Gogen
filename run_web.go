package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/server"
)

// runWeb runs the web server until the context is cancelled or Start fails.
// It returns Start's error so main can exit non-zero on a real failure
// (e.g. the port is already bound); graceful context cancellation returns
// nil. It owns the server's session-sweep defer (ShutdownSessions); the
// agent-level defers (a.Close, a.FlushPending, MCP shutdown) stay in main so
// the shutdown order is unchanged. MCP init itself is started by startMCP in
// main before the mode branches.
//
// tokenStatePath is where the auto-generated web auth token is persisted
// (see web_token.go); it is only touched for non-loopback binds.
func runWeb(ctx context.Context, a *agent.Agent, cfg *config.Config, restoredModel, tokenStatePath string) error {
	// Determine the listen address first so we can check for loopback
	// and resolve the auth token before creating the server.
	// WebListenEnv is this process's bind only. Leaving cfg.WebBind alone
	// keeps a settings save from storing the sidebar's free port.
	addr := processListenAddr(cfg.WebBind)
	var isLoopback bool
	if addr == "" {
		addr = "127.0.0.1:8081"
		isLoopback = true
	} else {
		if !strings.Contains(addr, ":") {
			addr += ":8081"
		}
		isLoopback = server.IsLoopbackBind(addr)
	}

	// Resolve the effective auth token exactly once, before the server is
	// created, so the pairing setup below and the server's auth checks can
	// never disagree:
	//   1. web_auth_token from the config file — used as-is;
	//   2. GOGEN_WEB_TOKEN env var — used as-is;
	//   3. otherwise the persisted state file (keeps already-paired devices
	//      logged in across restarts), or an ephemeral token for this run
	//      when the state file is unavailable (warned; rotates on restart).
	// Non-loopback binds without any of these fail in Server.Start. Loopback
	// binds only get a token when one was explicitly configured.
	if cfg.WebAuthToken == "" {
		cfg.WebAuthToken = strings.TrimSpace(os.Getenv("GOGEN_WEB_TOKEN"))
	}
	if !isLoopback && cfg.WebAuthToken == "" {
		tok, err := loadOrCreateWebToken(tokenStatePath)
		if err != nil {
			log.Printf("WARNING: web auth token state unavailable (%v); using an ephemeral token for this run", err)
			tok, err = generateToken()
			if err != nil {
				log.Fatalf("failed to generate auth token: %v", err)
			}
		}
		cfg.WebAuthToken = tok
	}

	s := server.NewServer(a, cfg)
	// Flush every registered session agent on shutdown. Both this
	// sweep and the outer defer use the
	// non-forcing FlushPending: the sweep must persist unsaved state,
	// but a forced write on a clean session re-stamps its Updated
	// timestamp with ~now in registry order, which reshuffled the
	// saved-session list on every restart and demoted the session that
	// was active at shutdown. The outer defer still covers the default
	// session idempotently and the TUI/CLI paths (flushAndQuit already
	// forces the TUI write; a dirty session is still written here).
	defer s.ShutdownSessions()

	// Print build identity so a stale binary (whose QR/link format may not
	// match this server's /pair endpoint) is obvious from the startup
	// output.
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev := bi.Main.Version
		for _, st := range bi.Settings {
			if st.Key == "vcs.revision" && len(st.Value) >= 7 {
				rev = st.Value[:7]
			}
		}
		fmt.Printf("build: %s (%s)\n", rev, bi.GoVersion)
	}

	// Print the onboarding link (and QR for phones) before Start so the UI
	// is reachable the moment the listener is up. When token auth is active
	// the link and QR carry a short-lived pairing code — never the long-lived
	// token — so a photographed link/QR goes dead quickly, and the token is
	// never printed or logged. The pairing code is per-boot: it dies on the
	// next restart (rescan the fresh QR); the login itself persists because
	// the token is stored in the state file.
	if cfg.WebAuthToken != "" {
		code, err := generateToken()
		if err != nil {
			log.Fatalf("failed to generate pairing code: %v", err)
		}
		expiry := time.Now().Add(server.PairingTTL)
		s.SetPairingCode(code, expiry)

		fmt.Printf("Open http://%s/pair/%s (this machine)\n", displayAddrFor(addr), code)
		fmt.Printf("Pairing code: %s\n", code)
		fmt.Printf("Note: this link and QR expire at %s or at the next restart — whichever comes first. Scan before then; a fresh link and QR are printed at every server start.\n", expiry.Local().Format("15:04:05"))
		if lan := server.LANHost(addr); lan != "" {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return fmt.Errorf("invalid listen address %q: %w", addr, err)
			}
			// The code lives in the path (/pair/<code>), not the query:
			// phone camera apps and in-app browsers strip query strings
			// when opening a scanned URL, and the path survives.
			pairURL := fmt.Sprintf("http://%s:%s/pair/%s", lan, port, code)
			fmt.Println("Phone: scan the QR code below (or type the pairing code above):")
			if !stdoutIsTerminal() {
				fmt.Printf("(QR not shown — stdout is not a terminal; open %s on the phone)\n", pairURL)
			} else if qr, err := server.RenderQR(pairURL); err != nil {
				fmt.Printf("(QR unavailable: %v)\n", err)
			} else {
				fmt.Println(qr)
			}
		} else if server.IsLoopbackBind(addr) {
			fmt.Println("Phone: the web UI is bound to loopback only — use a non-loopback bind (e.g. --host 0.0.0.0) for phone access.")
		} else {
			fmt.Println("Phone: no LAN address detected — QR unavailable; use the link above on this machine.")
		}
	} else {
		fmt.Printf("Open http://%s\n", displayAddrFor(addr))
	}
	// Listen first so the UI can connect immediately. Provider model
	// validation and context-limit lookup continue in the background.
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Start(ctx, addr)
	}()
	go func() {
		a.ValidateRestoredModel(context.Background(), restoredModel)
		// Publish the resolved model on the workspace (mutex-guarded) so new
		// sessions created while validation is still running seed from the
		// effective model — auto-selected sole model, or "" when a stale
		// restored model was cleared. Never write cfg.OpenAIModel here: the
		// provider factory reads it concurrently on session creation.
		s.SetDefaultModel(a.CurrentModel())
	}()
	// Start returns nil on graceful context cancellation and a real error
	// when the listener fails — propagate it to main's error path.
	return <-errCh
}

// processListenAddr is the address this process binds. When WebListenEnv
// is set, that value wins for the listener and is not written back onto
// the config.
func processListenAddr(cfgBind string) string {
	if v := strings.TrimSpace(os.Getenv(config.WebListenEnv)); v != "" {
		return v
	}
	return cfgBind
}

// displayAddrFor rewrites an unspecified bind host (0.0.0.0 / ::) to
// 127.0.0.1 so the printed link opens in a browser on the server machine.
func displayAddrFor(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && ip.IsUnspecified() {
		return "127.0.0.1:" + port
	}
	return addr
}

// stdoutIsTerminal reports whether stdout is a character device (an
// interactive terminal rather than a pipe or file), so QR art is skipped
// when it would just pollute a log.
func stdoutIsTerminal() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
