package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/JingxuanKang/cospace/internal/api"
	"github.com/JingxuanKang/cospace/internal/container"
	"github.com/JingxuanKang/cospace/internal/dist"
	"github.com/JingxuanKang/cospace/internal/gateway"
	"github.com/JingxuanKang/cospace/internal/imagesync"
	"github.com/JingxuanKang/cospace/internal/keepalive"
	"github.com/JingxuanKang/cospace/internal/pairing"
	"github.com/JingxuanKang/cospace/internal/power"
	"github.com/JingxuanKang/cospace/internal/spaces"
	"github.com/JingxuanKang/cospace/internal/transport"
	"github.com/JingxuanKang/cospace/internal/usage"
)

const (
	defaultImage = dist.BaseImage
	// Where spaces reach the gateway: the Apple container vmnet gateway on
	// macOS, the Docker bridge gateway on Linux (refined at serve time from
	// the engine's actual bridge configuration).
	appleGatewayURL  = "http://192.168.64.1:18930"
	dockerGatewayURL = "http://172.17.0.1:18930"
)

type config struct {
	data        string
	image       string
	gatewayURL  string
	codexModel  string
	claudeModel string
	// runtime selects the container engine: "apple" (macOS) or "docker"
	// (Linux). Apple container is the only runtime on macOS.
	runtime string
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "cospaced:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	return runWithIO(args, os.Stdin, stdout, os.Stderr)
}

func runWithIO(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cfg := defaultConfig()
	root := newFlagSet("cospaced")
	addGlobalFlags(root, &cfg)
	if err := root.Parse(args); err != nil {
		return err
	}
	args = root.Args()
	adoptLegacyDataDir(cfg.data)
	if err := validateRuntime(cfg); err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New("missing command")
	}

	switch args[0] {
	case "serve":
		return runServe(args[1:], cfg, stderr)
	case "setup":
		return runSetup(args[1:], stdout)
	case "uninstall":
		return runUninstall(args[1:], cfg, stdout)
	case "invite":
		return runInvite(args[1:], cfg, stdout)
	case "space":
		return runSpace(args[1:], cfg, stdout)
	case "template":
		return runTemplate(args[1:], cfg, stdout)
	case "member":
		return runMember(args[1:], cfg, stdin, stdout)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runServe(args []string, cfg config, stderr io.Writer) error {
	fs := newFlagSet("serve")
	addGlobalFlags(fs, &cfg)
	listen := fs.String("listen", "0.0.0.0:18930", "listen address")
	anthropicRaw := fs.String("anthropic-upstream", "https://api.anthropic.com", "Anthropic upstream URL")
	// ChatGPT-subscription tokens are only accepted by the chatgpt.com codex
	// backend, not api.openai.com — this default makes codex-in-spaces work off
	// the host's subscription with zero extra services. Hosts sponsoring with an
	// API key instead point this at https://api.openai.com/v1.
	openAIRaw := fs.String("openai-upstream", "https://chatgpt.com/backend-api/codex", "OpenAI upstream URL (spaces hit <upstream>/responses; use https://api.openai.com/v1 for API-key sponsoring)")
	subIRaw := fs.String("sub2api-i-upstream", "", "Optional Sub2API I API base URL, including /v1")
	subIIRaw := fs.String("sub2api-ii-upstream", "", "Optional Sub2API II API base URL, including /v1")
	subIKey := fs.String("sub2api-i-keychain", "sub2api-gpt-api-key", "Host Keychain service for Sub2API I")       // Keychain lookup name, not a credential. gitleaks:allow
	subIIKey := fs.String("sub2api-ii-keychain", "sub2api-gpt-api-key-ii", "Host Keychain service for Sub2API II") // Keychain lookup name, not a credential. gitleaks:allow
	xaiRaw := fs.String("xai-upstream", "https://api.x.ai/v1", "xAI upstream URL (accepts the host's grok subscription token directly)")
	idleTimeout := fs.Duration("idle-timeout", 30*time.Minute, "stop spaces after this much inactivity (0 disables)")
	credKeepalive := fs.Duration("cred-keepalive", 30*time.Minute, "refresh sponsor credentials that expire within this window by pinging the vendor CLI with a minimal request (0 disables)")
	consoleAddr := fs.String("console", "127.0.0.1:18931", "local web console + REST API address (empty disables)")
	consoleHosts := fs.String("console-hosts", "", "comma-separated public hostnames the console is published under (e.g. behind a tunnel); loopback is always allowed")
	gatewayAllow := fs.String("gateway-allow", "", "comma-separated CIDRs allowed to reach the gateway besides loopback and the container network (\"any\" disables the check)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return fmt.Errorf("serve: unexpected arguments: %s", strings.Join(pos, " "))
	}
	if *idleTimeout < 0 {
		return errors.New("serve: idle-timeout cannot be negative")
	}

	anthropic, err := parseUpstream("anthropic", *anthropicRaw)
	if err != nil {
		return err
	}
	openAI, err := parseUpstream("openai", *openAIRaw)
	if err != nil {
		return err
	}
	xai, err := parseUpstream("xai", *xaiRaw)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.data, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	if err := ensureContainerSystem(cfg, stderr); err != nil {
		fmt.Fprintln(stderr, err)
	}
	cfg.gatewayURL = resolveGatewayURL(cfg, stderr)
	if cfg.runtime == "docker" {
		if u, err := url.Parse(cfg.gatewayURL); err == nil {
			fmt.Fprintf(stderr, "docker runtime: spaces reach the gateway at %s via docker0; a host firewall with a whitelist INPUT chain needs  iptables -I INPUT -i docker0 -p tcp --dport %s -j ACCEPT\n", cfg.gatewayURL, u.Port())
		}
	}
	m := newManager(cfg)
	logger := log.New(stderr, "", 0)
	// One pairing store per process: the console's Invite and the transport's
	// Pair would otherwise race each other's load→save on invites.json.
	invites := pairing.NewStore(filepath.Join(cfg.data, "invites.json"))
	spaceBackend := &daemonSpaceBackend{
		spaces:     m,
		containers: m.C,
		invites:    invites,
		// Dial the container through a freshly-spawned `nc` subprocess rather
		// than from this long-lived process: a launchd daemon that started
		// before Apple container's vmnet bridge came up gets "no route to host"
		// to 192.168.64.x, but any newly-spawned process routes fine (and this
		// also survives container/vmnet restarts). See DialSSH.
		dial:  spaceDialer(cfg),
		sleep: time.Sleep,
		wait:  30 * time.Second,
	}
	tm := transport.New(cfg.data, spaceBackend)
	defer tm.Close()
	m.OnDeleted = func(name string) {
		if err := tm.DropSpace(name); err != nil {
			logger.Printf("transport: drop %s: %v", name, err)
		}
	}
	// A space whose transport node cannot start (offline, damaged identity)
	// is logged and retried by the maintenance loop; it must never keep the
	// gateway and console down for every other space.
	if err := ensureTransportSpaces(m, tm); err != nil {
		logger.Printf("transport: %v", err)
	}
	// Spaces already running when the daemon starts get a fresh activity stamp,
	// otherwise the reaper would treat them as never-used and never reclaim them.
	if statuses, err := m.List(); err == nil {
		for _, st := range statuses {
			if strings.EqualFold(st.State, "running") {
				tm.Touch(st.Name)
			}
		}
	}
	startSpaceMaintenance(m, tm, *idleTimeout, logger)

	us, err := usage.Open(filepath.Join(cfg.data, "usage.jsonl"))
	if err != nil {
		return fmt.Errorf("open usage store: %w", err)
	}
	us.Logf = logger.Printf
	// Credentials come straight from the host's own CLI credential files, so
	// the gateway always forwards the CURRENT OAuth token (the local CLI keeps
	// them refreshed) instead of a snapshot that expires. A per-provider
	// sponsor toggle gates this — off means spaces get a clean 503.
	sp := newSponsor(filepath.Join(cfg.data, "sponsor.json"), map[string]string{
		"anthropic": defaultCredPath(".claude", ".credentials.json"),
		"openai":    defaultCredPath(".codex", "auth.json"),
		"xai":       defaultCredPath(".grok", "auth.json"),
	})
	fileCreds := gateway.NewFileCreds(sp.paths)
	fileCreds.Enabled = sp.enabled
	routes := map[string]gateway.OpenAIRoute{}
	routeOptions := []api.CodexRouteOption{{ID: "official", Label: "Official account", Configured: true}}
	for _, c := range []struct{ id, label, raw, key string }{{"sub2api-i", "Sub2API I", *subIRaw, *subIKey}, {"sub2api-ii", "Sub2API II", *subIIRaw, *subIIKey}} {
		routeOptions = append(routeOptions, api.CodexRouteOption{ID: c.id, Label: c.label, Configured: c.raw != ""})
		if c.raw == "" {
			continue
		}
		u, err := parseUpstream(c.id, c.raw)
		if err != nil {
			return err
		}
		routes[c.id] = gateway.OpenAIRoute{Upstream: u, Creds: &gateway.KeychainCreds{Service: c.key, Enabled: sp.enabled}}
	}
	g := &gateway.Gateway{
		OpenAIRoutes: routes,
		Auth:         m,
		Creds:        fileCreds,
		Upstreams: map[string]*url.URL{
			"anthropic": anthropic,
			"openai":    openAI,
			"xai":       xai,
		},
		Usage:  teeUsage{store: us, log: logger, touch: tm.Touch},
		Policy: m,
		Logf:   logger.Printf,
	}
	// Seed each current space generation from history. Names are reusable after
	// deletion, while the space token (our internal quota key) is not. The
	// records come straight from disk: the container runtime may not be up yet
	// at boot, and a failed listing must not hand every space a fresh budget.
	records, listErr := m.Records()
	since := make(map[string]time.Time, len(records))
	quotaKeys := make(map[string]string, len(records))
	for _, r := range records {
		since[r.Name] = r.CreatedAt
		quotaKeys[r.Name] = r.QuotaKey()
	}
	if seedBySpace, err := us.SpentBySpaceSince(since); err != nil || listErr != nil {
		if err == nil {
			err = listErr
		}
		return fmt.Errorf("quota seed: %w (refusing to start with empty budgets)", err)
	} else {
		seed := make(map[string]float64, len(seedBySpace))
		for space, amount := range seedBySpace {
			seed[quotaKeys[space]] = amount
		}
		g.InitQuota(seed)
	}

	// Keep the Mac awake while any space runs; reconcile alongside the reaper.
	startPowerKeeper(m, logger)

	// Keep sponsor credentials fresh while the host is away: access tokens
	// live ~hours and the vendor CLIs only renew them when they run, so an
	// idle host would strand every space on a dead token (verified 2026-08-31).
	if *credKeepalive > 0 {
		startCredKeepalive(m, sp, *credKeepalive, cfg, logger)
	}

	image := &imagesync.Syncer{Ref: cfg.image, Exists: m.C.ImageExists, Pull: m.C.Pull, Log: logger}
	image.Start()

	if *consoleAddr != "" {
		console := &api.Server{
			Image:       image,
			CodexRoutes: routeOptions,
			Spaces:      m,
			Usage:       us,
			Inviter:     &transportInviter{tm: tm, invites: invites, spaces: m},
			Sponsor:     sp,
			Templates:   templateStore(cfg),
			MemTotalGB:  hostMemoryGB,
			MemUsedGB:   hostMemoryUsedGB,
			OnBattery:   onBattery,
			StrictHost:  true,
		}
		for _, h := range strings.Split(*consoleHosts, ",") {
			if h = strings.TrimSpace(h); h != "" {
				console.AllowedHosts = append(console.AllowedHosts, h)
			}
		}
		cs := &http.Server{Handler: console.Handler(), ReadHeaderTimeout: 10 * time.Second}
		lns, err := listenConsole(*consoleAddr)
		if err != nil {
			return fmt.Errorf("console listen: %w", err)
		}
		logger.Printf("console on http://%s", *consoleAddr)
		for _, ln := range lns {
			ln := ln
			go func() {
				if err := cs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Printf("console: %v", err)
				}
			}()
		}
	}

	allow, err := gatewayAllowList(*gatewayAllow, cfg.gatewayURL)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              *listen,
		Handler:           restrictRemote(g, allow, logger),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve gateway: %w", err)
	}
	return nil
}

// teeUsage records to the persistent store, mirrors a one-line log, and
// counts the request as space activity so an agent working unattended in a
// detached tmux session keeps its space from being reaped as idle.
type teeUsage struct {
	store *usage.Store
	log   *log.Logger
	touch func(space string)
}

func (t teeUsage) Record(space, provider string, status int) {
	t.store.Record(space, provider, status)
	t.log.Printf("space=%s provider=%s status=%d", space, provider, status)
	if t.touch != nil {
		t.touch(space)
	}
}

// gatewayAllowList builds the set of client networks the gateway serves:
// loopback (the host's own CLIs), the container network the gateway URL
// lives on, and whatever the host adds. "any" disables the check.
func gatewayAllowList(extra, gatewayURL string) ([]*net.IPNet, error) {
	if strings.TrimSpace(extra) == "any" {
		return nil, nil
	}
	var nets []*net.IPNet
	add := func(cidr string) error {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("gateway-allow %q: %w", cidr, err)
		}
		nets = append(nets, n)
		return nil
	}
	for _, c := range []string{"127.0.0.0/8", "::1/128"} {
		_ = add(c)
	}
	if u, err := url.Parse(gatewayURL); err == nil {
		if ip := net.ParseIP(u.Hostname()); ip != nil {
			if ip4 := ip.To4(); ip4 != nil {
				_ = add(ip4.Mask(net.CIDRMask(24, 32)).String() + "/24")
			} else {
				_ = add(ip.Mask(net.CIDRMask(64, 128)).String() + "/64")
			}
		}
	}
	for _, c := range strings.Split(extra, ",") {
		if c = strings.TrimSpace(c); c != "" {
			if err := add(c); err != nil {
				return nil, err
			}
		}
	}
	return nets, nil
}

// restrictRemote refuses gateway clients outside the allowed networks. The
// gateway listens on every interface so the container network can reach it,
// which also exposes it to the LAN; a space's fake token is readable by
// every guest of that space and never rotates, so a revoked guest on the
// same Wi-Fi must not be able to keep spending through it from outside.
func restrictRemote(next http.Handler, allow []*net.IPNet, logger *log.Logger) http.Handler {
	if allow == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip := net.ParseIP(host)
		ok := false
		for _, n := range allow {
			if ip != nil && n.Contains(ip) {
				ok = true
				break
			}
		}
		if !ok {
			logger.Printf("gateway: refused %s (outside allowed networks)", host)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (t teeUsage) RecordTokens(space, provider string, in, out int) {
	t.store.RecordTokens(space, provider, in, out)
}

func (t teeUsage) RecordCost(space, provider, model string, in, out int, cost float64) {
	t.store.RecordCost(space, provider, model, in, out, cost)
}

// transportInviter adapts the transport addr + pairing store to api.Inviter.
type transportInviter struct {
	tm      *transport.Manager
	invites *pairing.Store
	spaces  *spaces.Manager
}

func (ti *transportInviter) Invite(space string, ttl time.Duration) (string, string, time.Time, error) {
	if _, err := ti.spaces.Get(space); err != nil {
		return "", "", time.Time{}, err
	}
	addr, err := ti.tm.Address(space)
	if err != nil {
		return "", "", time.Time{}, err
	}
	code, err := ti.invites.Create(space, ttl)
	if err != nil {
		return "", "", time.Time{}, err
	}
	return code, fmt.Sprintf("cospace pair %s %s", addr, code), time.Now().Add(ttl), nil
}

// Peek resolves an outstanding code for the public invite page without
// consuming it.
func (ti *transportInviter) Peek(code string) (string, string, time.Time, error) {
	space, expires, err := ti.invites.Lookup(code)
	if err != nil {
		return "", "", time.Time{}, err
	}
	addr, err := ti.tm.Address(space)
	if err != nil {
		return "", "", time.Time{}, err
	}
	return space, fmt.Sprintf("cospace pair %s %s", addr, pairing.Canonical(code)), expires, nil
}

// credPingArgv is the cheapest thing each vendor CLI can do; the point is the
// side effect (the CLI renews its own credential file), not the answer.
// The codex backend rejects model_reasoning_effort="minimal" outright, so
// "low" on the cheapest slug it accepts (gpt-5.6-luna); grok subscriptions
// offer no model cheaper than the default, so it stays unpinned.
func credPingArgv(provider string) []string {
	switch provider {
	case "anthropic":
		return []string{"claude", "--model", "haiku", "-p", "reply with exactly: ok"}
	case "openai":
		return []string{"codex", "exec", "--skip-git-repo-check", "-m", "gpt-5.6-luna", "-c", `model_reasoning_effort="low"`, "reply with exactly: ok"}
	case "xai":
		return []string{"grok", "--disable-web-search", "-p", "reply with exactly: ok"}
	}
	return nil
}

// lookVendorCLI finds a vendor CLI, checking the user-level install dirs a
// launchd daemon's minimal PATH misses (claude lives in ~/.local/bin, grok
// under ~/.grok/bin).
func lookVendorCLI(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	for _, dir := range []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".grok", "bin"),
		"/opt/homebrew/bin",
		"/usr/local/bin",
	} {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s CLI not found on PATH or in user install dirs", name)
}

func runCredPing(ctx context.Context, argv []string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	bin, err := lookVendorCLI(argv[0])
	if err != nil {
		return err
	}
	// The ping is a throwaway agent run: give it an empty scratch directory
	// (never the data dir, which holds transport keys and space records) and
	// nothing from this process's environment but what the CLI needs to find
	// its own login. `claude -p` in particular fails auth outright when USER
	// is missing from the environment.
	dir, err := os.MkdirTemp("", "cospace-keepalive-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	cmd := exec.CommandContext(ctx, bin, argv[1:]...)
	cmd.Dir = dir
	cmd.Env = pingEnv()
	cmd.Stdin = nil
	// Kill the whole process group on timeout and stop waiting for stragglers
	// (hooks, MCP servers) that inherit the output pipe; otherwise one hung
	// child parks the keepalive forever.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		tail := string(out)
		if len(tail) > 300 {
			tail = "…" + tail[len(tail)-300:]
		}
		return fmt.Errorf("%s: %w: %s", argv[0], err, strings.TrimSpace(tail))
	}
	// A refreshed Claude credential may have landed in the Keychain.
	gateway.InvalidateClaudeKeychainCache()
	return nil
}

// pingEnv is the minimal environment for a vendor CLI ping.
func pingEnv() []string {
	var env []string
	for _, k := range []string{"HOME", "USER", "LOGNAME", "PATH", "TMPDIR", "SHELL", "LANG", "TERM"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	if os.Getenv("USER") == "" {
		if u, err := user.Current(); err == nil && u.Username != "" {
			env = append(env, "USER="+u.Username, "LOGNAME="+u.Username)
		}
	}
	return env
}

func startCredKeepalive(m *spaces.Manager, sp *sponsorState, window time.Duration, cfg config, logger *log.Logger) {
	expiries := map[string]func() (time.Time, error){
		// Claude Code keeps its live credential in the Keychain on macOS; the
		// ping renews that copy, so the expiry must be judged from the same
		// place the gateway reads (whichever copy expires last).
		"anthropic": func() (time.Time, error) {
			c, err := gateway.ClaudeOAuth(sp.paths["anthropic"])
			if err != nil {
				return time.Time{}, err
			}
			if c.ExpiresAt.IsZero() {
				return time.Time{}, errors.New("no expiresAt in claude credentials")
			}
			return c.ExpiresAt, nil
		},
		"openai": func() (time.Time, error) { return keepalive.OpenAIExpiry(sp.paths["openai"]) },
		"xai":    func() (time.Time, error) { return keepalive.XAIExpiry(sp.paths["xai"]) },
	}
	var sources []keepalive.Source
	for provider, expiry := range expiries {
		argv := credPingArgv(provider)
		sources = append(sources, keepalive.Source{
			Provider: provider,
			Expiry:   expiry,
			Refresh:  func(ctx context.Context) error { return runCredPing(ctx, argv) },
		})
	}
	k := &keepalive.Keeper{
		Sources:   sources,
		Threshold: window,
		Cooldown:  10 * time.Minute,
		// Only ping providers some space actually enables; a listing error
		// fails open — a spare ping beats stranding spaces on a dead token.
		Needed: func(provider string) bool {
			if !sp.enabled(provider) {
				return false
			}
			statuses, err := m.Records()
			if err != nil {
				return true
			}
			for _, st := range statuses {
				if st.Providers == nil {
					return true
				}
				for _, p := range st.Providers {
					if p == provider {
						return true
					}
				}
			}
			return false
		},
		Logf: logger.Printf,
	}
	go k.Run(context.Background(), 5*time.Minute)
}

func startPowerKeeper(m *spaces.Manager, logger *log.Logger) {
	k := &power.Keeper{}
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			statuses, err := m.List()
			if err == nil {
				anyAwake := false
				for _, s := range statuses {
					if strings.EqualFold(s.State, "running") {
						anyAwake = true
						break
					}
				}
				if err := k.Reconcile(anyAwake); err != nil {
					logger.Printf("power keeper: %v", err)
				}
			}
			<-ticker.C
		}
	}()
}

// listenConsole binds the console so both 127.0.0.1 and ::1 reach it: browsers
// resolve "localhost" to IPv6 first, and Go's net.Listen("tcp","localhost:p")
// binds only the first resolved address, so a loopback console needs an
// explicit listener on each family. An explicit interface address is honored
// as given (single listener).
func listenConsole(addr string) ([]net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		return []net.Listener{ln}, nil
	}
	var lns []net.Listener
	for _, h := range []string{"127.0.0.1", "::1"} {
		ln, err := net.Listen("tcp", net.JoinHostPort(h, port))
		if err != nil {
			if len(lns) == 0 {
				return nil, err
			}
			continue // one family unavailable is fine as long as the other bound
		}
		lns = append(lns, ln)
	}
	return lns, nil
}

// sponsorState persists the host-wide "pay with my subscription" gate. The
// underlying map is retained for compatibility with existing sponsor.json
// files, while SetAll is the product-facing operation.
type sponsorState struct {
	path  string
	paths map[string]string
	mu    sync.Mutex
	on    map[string]bool
}

func newSponsor(path string, paths map[string]string) *sponsorState {
	s := &sponsorState{path: path, paths: paths, on: map[string]bool{}}
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &s.on) != nil {
		// A gate file we cannot read is not an open gate. Everything stays off
		// until the host flips the switches again (which rewrites the file).
		log.Printf("sponsor: %s is unreadable; all providers off until re-enabled", path)
		for p := range paths {
			s.on[p] = false
		}
		s.save()
		return s
	}
	// Default: sponsor whatever is logged in. Also covers providers added in
	// an upgrade — anything absent from a saved sponsor.json starts enabled.
	changed := false
	for p := range paths {
		if _, ok := s.on[p]; !ok {
			s.on[p] = true
			changed = true
		}
	}
	if changed {
		s.save()
	}
	return s
}

func (s *sponsorState) enabled(provider string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.on[provider]
}

func (s *sponsorState) State() map[string]api.ProviderSponsor {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]api.ProviderSponsor{}
	for p, path := range s.paths {
		_, err := os.Stat(path)
		out[p] = api.ProviderSponsor{Enabled: s.on[p], CredPresent: err == nil}
	}
	return out
}

// Set flips the host gate for one provider across every space.
func (s *sponsorState) Set(provider string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.paths[provider]; !ok {
		return fmt.Errorf("unknown provider %q", provider)
	}
	s.on[provider] = on
	return s.save()
}

func (s *sponsorState) SetAll(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for provider := range s.paths {
		s.on[provider] = on
	}
	return s.save()
}

// save writes the gate atomically: a truncated sponsor.json after a crash
// would otherwise be unreadable, and an unreadable gate fails closed.
func (s *sponsorState) save() error {
	b, _ := json.MarshalIndent(s.on, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func defaultCredPath(dir, file string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, dir, file)
}

// ncDial connects to address (host:port) via a freshly-spawned `nc`
// subprocess, returned as a net.Conn over a socketpair. This sidesteps the
// long-lived daemon's inability to route to Apple container's vmnet bridge
// when it started before the bridge existed. network is accepted for the
// dialer signature and assumed tcp.
func ncDial(network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socketpair: %w", err)
	}
	ours := os.NewFile(uintptr(fds[0]), "cospace-ncdial-local")
	theirs := os.NewFile(uintptr(fds[1]), "cospace-ncdial-child")
	conn, err := net.FileConn(ours) // dups the fd
	ours.Close()
	if err != nil {
		theirs.Close()
		return nil, err
	}
	cmd := exec.Command("nc", host, port)
	cmd.Stdin = theirs
	cmd.Stdout = theirs
	if err := cmd.Start(); err != nil {
		theirs.Close()
		conn.Close()
		return nil, fmt.Errorf("spawn nc: %w", err)
	}
	theirs.Close() // the child holds its own dup
	go func() { cmd.Wait() }()
	return conn, nil
}

// execOutput runs a host command and returns its stdout.
func execOutput(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

func runInvite(args []string, cfg config, stdout io.Writer) error {
	fs := newFlagSet("invite")
	addGlobalFlags(fs, &cfg)
	ttl := fs.Duration("ttl", 10*time.Minute, "invite lifetime")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("invite: missing space")
	}
	if len(pos) > 1 {
		return fmt.Errorf("invite: unexpected arguments: %s", strings.Join(pos[1:], " "))
	}
	if *ttl <= 0 {
		return errors.New("invite: ttl must be positive")
	}
	if _, err := newManager(cfg).Get(pos[0]); err != nil {
		return err
	}
	tm := transport.New(cfg.data, nil)
	addr, err := tm.Address(pos[0])
	if err != nil {
		return err
	}
	code, err := pairing.NewStore(filepath.Join(cfg.data, "invites.json")).Create(pos[0], *ttl)
	if err != nil {
		return err
	}
	space := pos[0]
	_, err = fmt.Fprintf(stdout, `Invite for space %q — expires in %s.

Send your guest the installer for their computer (guests who already
have cospace can skip it — pairing tells them if an update is needed):

  macOS / Linux:
       %s

  Windows PowerShell:
       %s

Then they run:
       cospace pair %s %s

  After that, your guest just runs:  ssh %s
  (Cursor / VS Code: Remote-SSH → %s)
`, space, ttl.String(), dist.GuestInstallPOSIX, dist.GuestInstallWindows, addr, code, space, space)
	return err
}

type spaceOperations interface {
	Start(name string) error
	StartOnConnect(name string) error
	ConnectionAllowed(name string) error
	Stop(name string) error
	StopIdle(name string) error
	AddMember(space, memberName, pubKey string) (*spaces.Member, error)
	CanAddMember(space, memberName, pubKey string) error
	HostIdentity(name string) (publicKey, fingerprint string, err error)
	List() ([]spaces.Status, error)
}

type containerRuntime interface {
	List() ([]container.Info, error)
	IP(name string) (string, error)
}

type daemonSpaceBackend struct {
	spaces     spaceOperations
	containers containerRuntime
	invites    *pairing.Store
	dial       func(network, address string) (net.Conn, error)
	sleep      func(time.Duration)
	wait       time.Duration
}

func (b *daemonSpaceBackend) DialSSH(space string) (net.Conn, error) {
	infos, err := b.containers.List()
	if err != nil {
		return nil, fmt.Errorf("inspect space %s: %w", space, err)
	}
	var info *container.Info
	for i := range infos {
		if infos[i].Name == space {
			info = &infos[i]
			break
		}
	}
	if info == nil {
		return nil, fmt.Errorf("space %s does not exist", space)
	}
	if err := b.spaces.ConnectionAllowed(space); err != nil {
		return nil, err
	}
	if !strings.EqualFold(info.State, "running") {
		if err := b.spaces.StartOnConnect(space); err != nil {
			return nil, fmt.Errorf("wake space %s: %w", space, err)
		}
	}
	wait := b.wait
	if wait <= 0 {
		wait = 30 * time.Second
	}
	sleep := b.sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	deadline := time.Now().Add(wait)
	var ip string
	for {
		ip, err = b.containers.IP(space)
		if err == nil && ip != "" {
			break
		}
		if !time.Now().Before(deadline) {
			if err == nil {
				err = errors.New("container has no address")
			}
			return nil, fmt.Errorf("wait for space %s address: %w", space, err)
		}
		sleep(250 * time.Millisecond)
	}
	dial := b.dial
	if dial == nil {
		dial = net.Dial
	}
	conn, err := dial("tcp", net.JoinHostPort(ip, "22"))
	if err != nil {
		return nil, fmt.Errorf("dial space %s SSH: %w", space, err)
	}
	return conn, nil
}

// Pair checks everything that could reject the guest BEFORE the one-time
// code is consumed: a name collision or a malformed key used to burn the
// invite, leaving the host to issue another with no idea why. The code is
// still verified first (a wrong code learns nothing about the space), and
// only an accepted request redeems it.
func (b *daemonSpaceBackend) Pair(space, code, memberName, pubKey string) (transport.PairResult, error) {
	inviteSpace, _, err := b.invites.Lookup(code)
	if err != nil {
		return transport.PairResult{}, err
	}
	if inviteSpace != space {
		return transport.PairResult{}, errors.New("invite is for a different space")
	}
	if err := b.spaces.CanAddMember(space, memberName, pubKey); err != nil {
		return transport.PairResult{}, pairRejection(err)
	}
	hostKey, hostFP, err := b.spaces.HostIdentity(space)
	if err != nil {
		return transport.PairResult{}, err
	}
	if _, err := b.invites.Redeem(code); err != nil {
		return transport.PairResult{}, err
	}
	if _, err := b.spaces.AddMember(space, memberName, pubKey); err != nil {
		return transport.PairResult{}, pairRejection(err)
	}
	return transport.PairResult{HostKey: hostKey, Fingerprint: hostFP}, nil
}

// pairRejection turns a membership error into a reason the guest tool may
// show. Only the holder of a valid invite code gets this far, so telling
// them "that name is taken, pass -name" reveals nothing a stranger could use.
func pairRejection(err error) error {
	switch {
	case errors.Is(err, spaces.ErrConflict):
		return &transport.PairError{Status: http.StatusConflict, Message: strings.TrimPrefix(err.Error(), spaces.ErrConflict.Error()+": ")}
	case errors.Is(err, spaces.ErrInvalid):
		return &transport.PairError{Status: http.StatusBadRequest, Message: strings.TrimPrefix(err.Error(), spaces.ErrInvalid.Error()+": ")}
	}
	return err
}

type transportManager interface {
	EnsureSpace(space string) (string, error)
	LastActivity(space string) time.Time
}

func ensureTransportSpaces(spaceManager interface {
	List() ([]spaces.Status, error)
}, tm transportManager) error {
	statuses, err := spaceManager.List()
	if err != nil {
		return fmt.Errorf("list spaces for transport: %w", err)
	}
	var joined error
	for _, status := range statuses {
		if _, err := tm.EnsureSpace(status.Name); err != nil {
			joined = errors.Join(joined, fmt.Errorf("ensure transport for %s: %w", status.Name, err))
		}
	}
	return joined
}

type activitySource interface {
	LastActivity(space string) time.Time
}

func reapIdleSpaces(spaceManager interface {
	List() ([]spaces.Status, error)
	StopIdle(name string) error
}, activity activitySource, idleTimeout time.Duration, now time.Time) error {
	if idleTimeout == 0 {
		return nil
	}
	statuses, err := spaceManager.List()
	if err != nil {
		return err
	}
	var joined error
	for _, status := range statuses {
		if !strings.EqualFold(status.State, "running") {
			continue
		}
		last := activity.LastActivity(status.Name)
		if last.IsZero() || now.Sub(last) <= idleTimeout {
			continue
		}
		if err := spaceManager.StopIdle(status.Name); err != nil && !errors.Is(err, spaces.ErrConflict) {
			joined = errors.Join(joined, fmt.Errorf("stop idle space %s: %w", status.Name, err))
		}
	}
	return joined
}

func startSpaceMaintenance(spaceManager spaceOperations, tm transportManager, idleTimeout time.Duration, logger *log.Logger) {
	go func() {
		// space create/delete run in separate CLI processes; this loop is how a
		// running serve picks up a newly created space's transport node, so keep
		// the cadence tight enough that "create then invite" converges quickly.
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for now := range ticker.C {
			if err := ensureTransportSpaces(spaceManager, tm); err != nil {
				logger.Printf("transport maintenance: %v", err)
			}
			if err := reapIdleSpaces(spaceManager, tm, idleTimeout, now); err != nil {
				logger.Printf("idle reaper: %v", err)
			}
		}
	}()
}

func runSpace(args []string, cfg config, stdout io.Writer) error {
	fs := newFlagSet("space")
	addGlobalFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return err
	}
	args = fs.Args()
	if len(args) == 0 {
		return errors.New("space: missing command (create, list, start, stop, delete)")
	}
	switch args[0] {
	case "create":
		return runSpaceCreate(args[1:], cfg, stdout)
	case "list":
		return runSpaceList(args[1:], cfg, stdout)
	case "start", "stop", "delete":
		return runSpaceAction(args[0], args[1:], cfg)
	default:
		return fmt.Errorf("unknown space command %q", args[0])
	}
}

func runSpaceCreate(args []string, cfg config, stdout io.Writer) error {
	fs := newFlagSet("space create")
	addGlobalFlags(fs, &cfg)
	memoryGB := fs.Int("memory", 2, "memory limit in GB")
	cpus := fs.Int("cpus", 4, "CPU limit")
	fullAuto := fs.Bool("full-auto", true, "preconfigure agents to run without permission prompts")
	template := fs.String("template", "", "create from a saved template")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("space create: missing name")
	}
	if len(pos) != 1 {
		return fmt.Errorf("space create: unexpected arguments: %s", strings.Join(pos[1:], " "))
	}

	opts := spaces.CreateOptions{MemoryGB: *memoryGB, CPUs: *cpus, FullAuto: *fullAuto}
	if *template != "" {
		t, err := templateStore(cfg).Get(*template)
		if err != nil {
			return err
		}
		opts = t.Options()
		// Flags the user typed still override the template.
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "memory":
				opts.MemoryGB = *memoryGB
			case "cpus":
				opts.CPUs = *cpus
			case "full-auto":
				opts.FullAuto = *fullAuto
			}
		})
	}
	m := newManager(cfg)
	if err := ensureSpaceImage(m.C, cfg.image, stdout); err != nil {
		return err
	}
	r, err := m.CreateWithOptions(pos[0], opts)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout,
		"Name: %s\nToken: %s\nSSH: guests connect to the space IP with ssh space@<space-ip> (find it with \"cospaced space list\").\n",
		r.Name, r.Token)
	return err
}

func runSpaceList(args []string, cfg config, stdout io.Writer) error {
	fs := newFlagSet("space list")
	addGlobalFlags(fs, &cfg)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return fmt.Errorf("space list: unexpected arguments: %s", strings.Join(pos, " "))
	}
	statuses, err := newManager(cfg).List()
	if err != nil {
		return err
	}
	return formatSpaceList(stdout, statuses)
}

func runSpaceAction(action string, args []string, cfg config) error {
	fs := newFlagSet("space " + action)
	addGlobalFlags(fs, &cfg)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return fmt.Errorf("space %s: missing name", action)
	}
	if len(pos) != 1 {
		return fmt.Errorf("space %s: unexpected arguments: %s", action, strings.Join(pos[1:], " "))
	}

	m := newManager(cfg)
	switch action {
	case "start":
		err = m.Start(pos[0])
	case "stop":
		err = m.Stop(pos[0])
	case "delete":
		err = m.Delete(pos[0])
	}
	if err != nil {
		return fmt.Errorf("space %s %q: %w", action, pos[0], err)
	}
	return nil
}

func templateStore(cfg config) *spaces.TemplateStore {
	return spaces.NewTemplateStore(filepath.Join(cfg.data, "templates.json"))
}

func runTemplate(args []string, cfg config, stdout io.Writer) error {
	fs := newFlagSet("template")
	addGlobalFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return err
	}
	args = fs.Args()
	if len(args) == 0 {
		return errors.New("template: missing command (list, save, delete)")
	}
	store := templateStore(cfg)
	switch args[0] {
	case "list":
		ts, err := store.List()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tMEM\tCPUS\tPROVIDERS\tUSD LIMIT\tFULL-AUTO")
		for _, t := range ts {
			fmt.Fprintf(tw, "%s\t%dGB\t%d\t%s\t%.0f\t%v\n",
				t.Name, t.MemoryGB, t.CPUs, strings.Join(t.Providers, ","), t.UsdLimit, t.FullAuto)
		}
		return tw.Flush()
	case "save":
		if len(args) != 3 {
			return errors.New("template save: want <template-name> <space>")
		}
		space, err := newManager(cfg).Get(args[2])
		if err != nil {
			return err
		}
		usdLimit, maxConc := newManager(cfg).Limits(args[2])
		t, err := store.Save(spaces.TemplateFromSpace(args[1], space, usdLimit, maxConc))
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "Saved template %q from space %q.\n", t.Name, space.Name)
		return err
	case "delete":
		if len(args) != 2 {
			return errors.New("template delete: want <template-name>")
		}
		return store.Delete(args[1])
	default:
		return fmt.Errorf("unknown template command %q", args[0])
	}
}

func runMember(args []string, cfg config, stdin io.Reader, stdout io.Writer) error {
	fs := newFlagSet("member")
	addGlobalFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return err
	}
	args = fs.Args()
	if len(args) == 0 {
		return errors.New("member: missing command")
	}
	switch args[0] {
	case "add":
		return runMemberAdd(args[1:], cfg, stdin, stdout)
	case "revoke":
		return runMemberRevoke(args[1:], cfg)
	default:
		return fmt.Errorf("unknown member command %q", args[0])
	}
}

func runMemberAdd(args []string, cfg config, stdin io.Reader, stdout io.Writer) error {
	fs := newFlagSet("member add")
	addGlobalFlags(fs, &cfg)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	switch len(pos) {
	case 0:
		return errors.New("member add: missing space")
	case 1:
		return errors.New("member add: missing member name")
	case 2:
		return errors.New("member add: missing public key file")
	}
	if len(pos) != 3 {
		return fmt.Errorf("member add: unexpected arguments: %s", strings.Join(pos[3:], " "))
	}

	pubKey, err := readPublicKey(pos[2], stdin)
	if err != nil {
		return err
	}
	member, err := newManager(cfg).AddMember(pos[0], pos[1], pubKey)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Fingerprint: %s\n", member.Fingerprint)
	return err
}

func runMemberRevoke(args []string, cfg config) error {
	fs := newFlagSet("member revoke")
	addGlobalFlags(fs, &cfg)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	switch len(pos) {
	case 0:
		return errors.New("member revoke: missing space")
	case 1:
		return errors.New("member revoke: missing member name")
	}
	if len(pos) != 2 {
		return fmt.Errorf("member revoke: unexpected arguments: %s", strings.Join(pos[2:], " "))
	}
	if err := newManager(cfg).RevokeMember(pos[0], pos[1]); err != nil {
		return fmt.Errorf("revoke member %q from %q: %w", pos[1], pos[0], err)
	}
	return nil
}

func formatSpaceList(w io.Writer, statuses []spaces.Status) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tSTATE\tIP\tGUESTS\tMEM\tCPUS\tCREATED"); err != nil {
		return err
	}
	for _, status := range statuses {
		ip := status.IP
		if ip == "" {
			ip = "-"
		}
		created := "-"
		if !status.CreatedAt.IsZero() {
			created = status.CreatedAt.UTC().Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%dGB\t%d\t%s\n",
			status.Name, status.State, ip, len(status.Members), status.MemoryGB, status.CPUs, created); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func readPublicKey(path string, stdin io.Reader) (string, error) {
	var (
		data []byte
		err  error
	)
	if path == "-" {
		data, err = io.ReadAll(stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("read public key %q: %w", path, err)
	}
	return string(data), nil
}

func newManager(cfg config) *spaces.Manager {
	return &spaces.Manager{
		Dir:         cfg.data,
		C:           newRuntime(cfg),
		Image:       cfg.image,
		GatewayURL:  cfg.gatewayURL,
		CodexModel:  cfg.codexModel,
		ClaudeModel: cfg.claudeModel,
	}
}

func defaultConfig() config {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	cfg := config{
		image:      defaultImage,
		codexModel: "gpt-5.6-sol",
		runtime:    defaultRuntime(),
	}
	if runtime.GOOS == "darwin" {
		cfg.data = filepath.Join(home, "Library", "Application Support", "CoSpace")
		cfg.gatewayURL = appleGatewayURL
	} else {
		base := os.Getenv("XDG_DATA_HOME")
		if base == "" {
			base = filepath.Join(home, ".local", "share")
		}
		cfg.data = filepath.Join(base, "cospace")
		cfg.gatewayURL = dockerGatewayURL
	}
	return cfg
}

// adoptLegacyDataDir renames a pre-rename "Guestroom" data dir into place
// the first time the product runs on its default data dir. Only the default
// location is touched: tests and ad-hoc -data runs must never move the real
// state directory.
func adoptLegacyDataDir(data string) {
	if data != defaultConfig().data {
		return
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		return
	}
	legacy := filepath.Join(filepath.Dir(data), "Guestroom")
	if _, err := os.Stat(legacy); err == nil {
		os.Rename(legacy, data)
	}
}

func addGlobalFlags(fs *flag.FlagSet, cfg *config) {
	fs.StringVar(&cfg.data, "data", cfg.data, "data directory")
	fs.StringVar(&cfg.image, "image", cfg.image, "space container image")
	fs.StringVar(&cfg.runtime, "runtime", cfg.runtime, "container engine: apple (macOS) or docker (Linux)")
	fs.StringVar(&cfg.gatewayURL, "gateway-url", cfg.gatewayURL, "gateway URL visible inside spaces")
	fs.StringVar(&cfg.codexModel, "codex-model", cfg.codexModel, "model name codex uses in spaces")
	fs.StringVar(&cfg.claudeModel, "claude-model", cfg.claudeModel, "default claude model in spaces, e.g. claude-fable-5[1m] (empty = claude's own default)")
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var flagArgs, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}

		name := strings.TrimLeft(arg, "-")
		if before, _, ok := strings.Cut(name, "="); ok {
			name = before
		}
		f := fs.Lookup(name)
		if f == nil {
			return nil, fmt.Errorf("flag provided but not defined: -%s", name)
		}
		flagArgs = append(flagArgs, arg)
		if strings.Contains(arg, "=") || isBoolFlag(f) {
			continue
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("flag needs an argument: -%s", name)
		}
		i++
		flagArgs = append(flagArgs, args[i])
	}
	if err := fs.Parse(flagArgs); err != nil {
		return nil, err
	}
	return positional, nil
}

func isBoolFlag(f *flag.Flag) bool {
	type boolFlag interface {
		IsBoolFlag() bool
	}
	b, ok := f.Value.(boolFlag)
	return ok && b.IsBoolFlag()
}

func parseUpstream(provider, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid %s upstream: %w", provider, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid %s upstream %q: want an http or https URL", provider, raw)
	}
	return u, nil
}
