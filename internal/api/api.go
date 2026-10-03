// Package api serves the host-side console: a localhost REST API plus the
// embedded web UI. It never exposes credentials; tokens returned here are the
// per-space fake tokens only.
package api

import (
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JingxuanKang/cospace/internal/container"
	"github.com/JingxuanKang/cospace/internal/dist"
	"github.com/JingxuanKang/cospace/internal/imagesync"
	"github.com/JingxuanKang/cospace/internal/spaces"
	"github.com/JingxuanKang/cospace/internal/usage"
)

//go:embed web
var webFS embed.FS

// The invite page lives outside web/ so the static file server never exposes
// the raw template.
//
//go:embed invite.html
var inviteHTML string

var inviteTmpl = template.Must(template.New("invite").Parse(inviteHTML))

// Inviter creates a one-time pairing invite for a space. Wired to the
// pairing+transport layer; nil while that layer is absent. Peek resolves an
// outstanding code for the public invite page without consuming it; any error
// means the page must treat the link as dead.
type Inviter interface {
	Invite(space string, ttl time.Duration) (code, command string, expires time.Time, err error)
	Peek(code string) (space, command string, expires time.Time, err error)
}

// Sponsor is the host-wide AI access gate, per provider. State reports
// credential readiness and the gate per provider; Set flips one provider for
// every space; SetAll is the all-on/all-off convenience.
type Sponsor interface {
	State() map[string]ProviderSponsor
	Set(provider string, on bool) error
	SetAll(on bool) error
}

type ProviderSponsor struct {
	Enabled     bool `json:"enabled"`
	CredPresent bool `json:"cred_present"`
}

type Server struct {
	Spaces      *spaces.Manager
	Usage       *usage.Store
	Inviter     Inviter
	Sponsor     Sponsor
	Templates   *spaces.TemplateStore
	MemTotalGB  func() int
	MemUsedGB   func() int
	OnBattery   func() bool
	CodexRoutes []CodexRouteOption
	// Image reports the base image download; nil means always ready.
	Image *imagesync.Syncer
	// AllowedHosts lists the public hostnames the console is published under
	// (e.g. behind a tunnel). Loopback names are always allowed. Any other
	// Host or Origin is refused, which closes DNS rebinding and cross-site
	// request forgery against the mutating API; StrictHost turns the Host
	// check on (the Origin check is always on).
	AllowedHosts []string
	StrictHost   bool

	inviteHits sync.Map // client IP -> *hitWindow, for the public invite page
}

// memberView is the member projection the console needs: the fingerprint
// identifies a guest; the full public key stays in the record.
type memberView struct {
	Name        string    `json:"name"`
	Fingerprint string    `json:"fingerprint"`
	AddedAt     time.Time `json:"added_at"`
}

type spaceView struct {
	Name        string       `json:"name"`
	State       string       `json:"state"`
	IP          string       `json:"ip"`
	ManualSleep bool         `json:"manual_sleep"`
	WakeBlocked string       `json:"wake_blocked,omitempty"`
	MemoryGB    int          `json:"memory_gb"`
	CPUs        int          `json:"cpus"`
	CreatedAt   time.Time    `json:"created_at"`
	HostKeyFP   string       `json:"host_key_fp,omitempty"`
	Providers   []string     `json:"providers"`
	Members     []memberView `json:"members"`
	UsageToday  usage.Day    `json:"usage_today"`
	// SpentUSD is the space's cumulative equivalent-dollar spend (lifetime, never
	// reset). UsdLimit is its spend cap (0 = uncapped); MaxConcurrency is the
	// effective concurrent-request cap.
	SpentUSD            float64 `json:"spent_usd"`
	UsdLimit            float64 `json:"usd_limit"`
	MaxConcurrency      int     `json:"max_concurrency"`
	FullAuto            bool    `json:"full_auto"`
	CodexRoute          string  `json:"codex_route"`
	CodexModel          string  `json:"codex_model"`
	NetworkMode         string  `json:"network_mode"`
	NetworkUpgrading    bool    `json:"network_upgrading"`
	NetworkUpgradeError string  `json:"network_upgrade_error,omitempty"`
	NetworkControlReady bool    `json:"network_control_ready"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/host", s.getHost)
	mux.HandleFunc("GET /api/spaces", s.listSpaces)
	mux.HandleFunc("POST /api/spaces", s.createSpace)
	mux.HandleFunc("POST /api/spaces/{name}/start", s.spaceAction((*spaces.Manager).Start))
	mux.HandleFunc("POST /api/spaces/{name}/stop", s.spaceAction((*spaces.Manager).Stop))
	mux.HandleFunc("DELETE /api/spaces/{name}", s.spaceAction((*spaces.Manager).Delete))
	mux.HandleFunc("DELETE /api/spaces/{name}/members/{member}", s.revokeMember)
	mux.HandleFunc("PUT /api/spaces/{name}/providers", s.setProviders)
	mux.HandleFunc("PUT /api/spaces/{name}/limits", s.setLimits)
	mux.HandleFunc("PUT /api/spaces/{name}/full_auto", s.setFullAuto)
	mux.HandleFunc("GET /api/codex-options", s.codexOptions)
	mux.HandleFunc("PUT /api/spaces/{name}/codex", s.setCodex)
	mux.HandleFunc("PUT /api/spaces/{name}/network", s.setNetwork)
	mux.HandleFunc("POST /api/spaces/{name}/upgrade-network", s.upgradeNetwork)
	mux.HandleFunc("GET /api/templates", s.listTemplates)
	mux.HandleFunc("PUT /api/templates/{name}", s.saveTemplate)
	mux.HandleFunc("DELETE /api/templates/{name}", s.deleteTemplate)
	mux.HandleFunc("GET /api/spaces/{name}/usage", s.spaceUsage)
	mux.HandleFunc("POST /api/spaces/{name}/invites", s.createInvite)
	mux.HandleFunc("GET /i/{code}", s.invitePage)
	mux.HandleFunc("GET /api/sponsor", s.getSponsor)
	mux.HandleFunc("POST /api/sponsor", s.setSponsor)

	web, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServerFS(web))
	return s.guard(mux)
}

// guard is the console's request-origin check. The console has no login of
// its own (it is loopback-only, or behind the host's SSO when tunneled), so
// the browser's ambient authority is all an attacker needs: any web page the
// host visits could POST to 127.0.0.1:18931 (or ride an SSO cookie through
// the tunnel). Non-GET requests must therefore come from the console's own
// origin, and the Host header must be one we serve, which also defeats DNS
// rebinding. Requests with neither Origin nor Sec-Fetch-Site (curl, scripts)
// are not browser-initiated and pass.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.StrictHost && !s.hostAllowed(r.Host) {
			http.Error(w, "unexpected Host header", http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
				u, err := url.Parse(origin)
				if err != nil || !s.originAllowed(u.Host, r.Host) {
					http.Error(w, "cross-origin request refused", http.StatusForbidden)
					return
				}
			} else if origin == "null" {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) hostAllowed(hostport string) bool {
	host := strings.ToLower(hostOnly(hostport))
	if isLoopbackHost(host) {
		return true
	}
	for _, h := range s.AllowedHosts {
		if strings.EqualFold(strings.TrimSpace(h), host) {
			return true
		}
	}
	return false
}

// originAllowed accepts an Origin that is the request's own host (same
// origin, any port) or a configured public host.
func (s *Server) originAllowed(originHost, requestHost string) bool {
	oh, rh := strings.ToLower(hostOnly(originHost)), strings.ToLower(hostOnly(requestHost))
	if oh == "" {
		return false
	}
	if oh == rh || (isLoopbackHost(oh) && isLoopbackHost(rh)) {
		return true
	}
	return s.hostAllowed(originHost)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// maxBodyBytes bounds every JSON request body the console accepts; the
// largest legitimate payload (a create with a template name) is well under 1 KB.
const maxBodyBytes = 64 << 10

// decodeJSON reads a bounded JSON body into v. A false return means a 400 was
// already written.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, want string) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, 400, map[string]string{"error": want})
		return false
	}
	return true
}

// spaceName validates the {name} path segment before it reaches the manager.
// The mux decodes %2F and %2E into the value, so without this a crafted path
// could point the manager at a file under a guest-writable workspace mount.
func spaceName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if !spaces.ValidName(name) {
		writeJSON(w, 400, map[string]string{"error": "invalid space name"})
		return "", false
	}
	return name, true
}

// httpStatus maps manager errors onto HTTP codes: typed errors first, then
// the message fallbacks older call sites still rely on.
func httpStatus(err error) int {
	msg := err.Error()
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, container.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, spaces.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, spaces.ErrConflict), errors.Is(err, spaces.ErrVMMissing):
		return http.StatusConflict
	case strings.Contains(msg, "no member"), strings.Contains(msg, "no template"):
		return http.StatusNotFound
	case strings.Contains(msg, "already exists"), strings.Contains(msg, "already registered"), strings.Contains(msg, "upgrading"), strings.Contains(msg, "wake this space"), strings.Contains(msg, "space upgrade first"):
		return http.StatusConflict
	case strings.Contains(msg, "invalid"), strings.Contains(msg, "not an OpenSSH"):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func fail(w http.ResponseWriter, err error) {
	writeJSON(w, httpStatus(err), map[string]string{"error": err.Error()})
}

func (s *Server) getHost(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.Spaces.List()
	if err != nil {
		fail(w, err)
		return
	}
	awake, memReserved := 0, 0
	for _, st := range statuses {
		if st.State == "running" {
			awake++
			memReserved += st.MemoryGB
		}
	}
	memUsed := 0
	if s.MemUsedGB != nil {
		memUsed = s.MemUsedGB()
	}
	host := map[string]any{
		"memory_total_gb":    s.MemTotalGB(),
		"memory_used_gb":     memUsed,
		"memory_reserved_gb": memReserved,
		"spaces":             len(statuses),
		"awake":              awake,
		"on_battery":         s.OnBattery(),
	}
	if s.Image != nil {
		host["image"] = s.Image.Status()
	}
	writeJSON(w, 200, host)
}

func (s *Server) listSpaces(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.Spaces.List()
	if err != nil {
		fail(w, err)
		return
	}
	today := map[string]usage.Day{}
	spent := map[string]float64{}
	if s.Usage != nil {
		since := make(map[string]time.Time, len(statuses))
		for _, st := range statuses {
			since[st.Name] = st.CreatedAt
		}
		today, _ = s.Usage.TodayBySpaceSince(since)
		spent, _ = s.Usage.SpentBySpaceSince(since)
	}
	views := make([]spaceView, 0, len(statuses))
	for _, st := range statuses {
		members := make([]memberView, 0, len(st.Members))
		for _, mem := range st.Members {
			members = append(members, memberView{Name: mem.Name, Fingerprint: mem.Fingerprint, AddedAt: mem.AddedAt})
		}
		provs := st.Providers
		if provs == nil {
			provs = []string{"anthropic", "openai", "xai"} // default: all
		}
		usdLimit, maxConc := s.Spaces.Limits(st.Name)
		upgrading, upgradeError := s.Spaces.NetworkUpgradeState(st.Name)
		views = append(views, spaceView{
			Name: st.Name, State: st.State, IP: st.IP, ManualSleep: st.ManualSleep, WakeBlocked: st.WakeBlocked,
			MemoryGB: st.MemoryGB, CPUs: st.CPUs, CreatedAt: st.CreatedAt,
			HostKeyFP: st.HostKeyFP, Providers: provs, Members: members, UsageToday: today[st.Name],
			SpentUSD: spent[st.Name], UsdLimit: usdLimit, MaxConcurrency: maxConc,
			FullAuto:   st.FullAuto,
			CodexRoute: st.EffectiveCodexRoute(), CodexModel: s.Spaces.EffectiveCodexModel(&st.Space), NetworkMode: st.EffectiveNetworkMode(),
			NetworkControlReady: st.NetworkControlVersion >= 1,
			NetworkUpgrading:    upgrading, NetworkUpgradeError: upgradeError,
		})
	}
	writeJSON(w, 200, views)
}

func (s *Server) createSpace(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string `json:"name"`
		MemoryGB    int    `json:"memory_gb"`
		CPUs        int    `json:"cpus"`
		Template    string `json:"template"`
		FullAuto    *bool  `json:"full_auto"`
		CodexRoute  string `json:"codex_route"`
		CodexModel  string `json:"codex_model"`
		NetworkMode string `json:"network_mode"`
	}
	if !decodeJSON(w, r, &in, "bad json: want {name, memory_gb, cpus, template?, full_auto?, codex_route?, codex_model?, network_mode?}") {
		return
	}
	if in.MemoryGB < 0 || in.MemoryGB > 512 || in.CPUs < 0 || in.CPUs > 256 {
		writeJSON(w, 400, map[string]string{"error": "memory_gb / cpus out of range"})
		return
	}
	if s.Image != nil && !s.Image.Ready() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "The space image is still downloading (first run only). Create the space once it finishes."})
		return
	}
	opts := spaces.CreateOptions{MemoryGB: in.MemoryGB, CPUs: in.CPUs, FullAuto: true}
	if in.Template != "" {
		if s.Templates == nil {
			writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "templates not configured"})
			return
		}
		t, err := s.Templates.Get(in.Template)
		if err != nil {
			fail(w, err)
			return
		}
		opts = t.Options()
		// An explicit size in the request still wins over the template's.
		if in.MemoryGB > 0 {
			opts.MemoryGB = in.MemoryGB
		}
		if in.CPUs > 0 {
			opts.CPUs = in.CPUs
		}
	}
	if in.FullAuto != nil {
		opts.FullAuto = *in.FullAuto
	}
	if in.CodexRoute != "" {
		opts.CodexRoute = in.CodexRoute
	}
	if in.CodexModel != "" {
		opts.CodexModel = in.CodexModel
	}
	if in.NetworkMode != "" {
		opts.NetworkMode = in.NetworkMode
	}
	if !s.routeConfigured(opts.CodexRoute) {
		writeJSON(w, 400, map[string]string{"error": "Codex route is not configured on this host"})
		return
	}
	space, err := s.Spaces.CreateWithOptions(in.Name, opts)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, space)
}

// setFullAuto flips a space between full-auto agents (no permission prompts)
// and the tools' default approval flows.
func (s *Server) setFullAuto(w http.ResponseWriter, r *http.Request) {
	name, ok := spaceName(w, r)
	if !ok {
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &in, "want {enabled}") {
		return
	}
	if in.Enabled == nil {
		writeJSON(w, 400, map[string]string{"error": "want {enabled}"})
		return
	}
	if err := s.Spaces.SetFullAuto(name, *in.Enabled); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	if s.Templates == nil {
		writeJSON(w, 200, []spaces.Template{})
		return
	}
	ts, err := s.Templates.List()
	if err != nil {
		fail(w, err)
		return
	}
	if ts == nil {
		ts = []spaces.Template{}
	}
	writeJSON(w, 200, ts)
}

// saveTemplate snapshots an existing space's settings under the given template
// name — the console's "Save as template" is its only producer, so a space
// reference is the whole input.
func (s *Server) saveTemplate(w http.ResponseWriter, r *http.Request) {
	if s.Templates == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "templates not configured"})
		return
	}
	var in struct {
		FromSpace string `json:"from_space"`
	}
	if !decodeJSON(w, r, &in, "want {from_space}") {
		return
	}
	if !spaces.ValidName(in.FromSpace) {
		writeJSON(w, 400, map[string]string{"error": "want {from_space}"})
		return
	}
	space, err := s.Spaces.Get(in.FromSpace)
	if err != nil {
		fail(w, err)
		return
	}
	usdLimit, maxConc := s.Spaces.Limits(in.FromSpace)
	t, err := s.Templates.Save(spaces.TemplateFromSpace(r.PathValue("name"), space, usdLimit, maxConc))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, t)
}

func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	if s.Templates == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "templates not configured"})
		return
	}
	if !spaces.ValidName(spaces.Normalize(r.PathValue("name"))) {
		writeJSON(w, 400, map[string]string{"error": "invalid template name"})
		return
	}
	if err := s.Templates.Delete(r.PathValue("name")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) spaceAction(fn func(*spaces.Manager, string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name, ok := spaceName(w, r)
		if !ok {
			return
		}
		if err := fn(s.Spaces, name); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(204)
	}
}

func (s *Server) revokeMember(w http.ResponseWriter, r *http.Request) {
	name, ok := spaceName(w, r)
	if !ok {
		return
	}
	member := r.PathValue("member")
	if !spaces.ValidMemberName(member) {
		writeJSON(w, 400, map[string]string{"error": "invalid member name"})
		return
	}
	if err := s.Spaces.RevokeMember(name, member); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) setProviders(w http.ResponseWriter, r *http.Request) {
	name, ok := spaceName(w, r)
	if !ok {
		return
	}
	var in struct {
		Providers []string `json:"providers"`
	}
	if !decodeJSON(w, r, &in, "want {providers: [...]}") {
		return
	}
	if err := s.Spaces.SetProviders(name, in.Providers); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

// setLimits updates a space's spend cap and concurrency cap. A nil field leaves
// that limit unchanged, so the console can PUT just the one the host edited.
func (s *Server) setLimits(w http.ResponseWriter, r *http.Request) {
	name, ok := spaceName(w, r)
	if !ok {
		return
	}
	var in struct {
		UsdLimit       *float64 `json:"usd_limit"`
		MaxConcurrency *int     `json:"max_concurrency"`
	}
	if !decodeJSON(w, r, &in, "want {usd_limit, max_concurrency}") {
		return
	}
	usd, mc := -1.0, -1
	if in.UsdLimit != nil {
		usd = *in.UsdLimit
		if usd < 0 || usd != usd || usd > 1e9 { // negative, NaN, absurd
			usd = 0
		}
	}
	if in.MaxConcurrency != nil {
		mc = *in.MaxConcurrency
		if mc < 0 {
			mc = 0
		}
		if mc > 1000 {
			mc = 1000
		}
	}
	if err := s.Spaces.SetLimits(name, usd, mc); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) spaceUsage(w http.ResponseWriter, r *http.Request) {
	days := 14
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90 {
			days = n
		}
	}
	name, ok := spaceName(w, r)
	if !ok {
		return
	}
	if s.Usage == nil {
		writeJSON(w, 200, []usage.Day{})
		return
	}
	space, err := s.Spaces.Get(name)
	if err != nil {
		fail(w, err)
		return
	}
	rows, err := s.Usage.DailySince(space.Name, days, space.CreatedAt)
	if err != nil {
		fail(w, err)
		return
	}
	if rows == nil {
		rows = []usage.Day{}
	}
	writeJSON(w, 200, rows)
}

func (s *Server) getSponsor(w http.ResponseWriter, r *http.Request) {
	if s.Sponsor == nil {
		writeJSON(w, 200, map[string]ProviderSponsor{})
		return
	}
	writeJSON(w, 200, s.Sponsor.State())
}

func (s *Server) setSponsor(w http.ResponseWriter, r *http.Request) {
	if s.Sponsor == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "sponsor not configured"})
		return
	}
	var in struct {
		Provider string `json:"provider"`
		Enabled  *bool  `json:"enabled"`
	}
	if !decodeJSON(w, r, &in, "want {enabled} or {provider, enabled}") {
		return
	}
	if in.Enabled == nil {
		writeJSON(w, 400, map[string]string{"error": "want {enabled} or {provider, enabled}"})
		return
	}
	var err error
	if in.Provider == "" {
		err = s.Sponsor.SetAll(*in.Enabled)
	} else {
		err = s.Sponsor.Set(in.Provider, *in.Enabled)
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, s.Sponsor.State())
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) {
	if s.Inviter == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "transport not configured"})
		return
	}
	name, ok := spaceName(w, r)
	if !ok {
		return
	}
	if _, err := s.Spaces.Get(name); err != nil {
		fail(w, err)
		return
	}
	code, command, expires, err := s.Inviter.Invite(name, 10*time.Minute)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"code": code, "command": command, "expires_at": expires,
		"install": map[string]string{"posix": dist.GuestInstallPOSIX, "windows": dist.GuestInstallWindows}})
}

type invitePageData struct {
	Dead           bool
	Space          string
	PairCommand    string
	MinutesLeft    int
	InstallPOSIX   string
	InstallWindows string
	// Lang is "en" or "zh": an explicit ?lang= wins, then Accept-Language.
	Lang string
}

// pageLang picks the invite page language. The guest tool and the console
// carry the same two languages; anything that is not Chinese reads English.
func pageLang(r *http.Request) string {
	switch strings.ToLower(r.URL.Query().Get("lang")) {
	case "zh", "zh-cn", "zh-hans":
		return "zh"
	case "en":
		return "en"
	}
	for _, part := range strings.Split(strings.ToLower(r.Header.Get("Accept-Language")), ",") {
		tag := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if strings.HasPrefix(tag, "zh") {
			return "zh"
		}
		if strings.HasPrefix(tag, "en") {
			return "en"
		}
	}
	return "en"
}

// hitWindow is a fixed-window counter for the public invite page.
type hitWindow struct {
	mu    sync.Mutex
	start time.Time
	count int
}

const (
	inviteWindow = time.Minute
	inviteBurst  = 30
)

// inviteAllowed rate-limits lookups of invite codes per client address. The
// page is the one route exposed without SSO, so brute force is bounded here
// rather than only by the code's entropy.
func (s *Server) inviteAllowed(r *http.Request) bool {
	ip := r.Header.Get("CF-Connecting-IP")
	if ip == "" {
		ip = hostOnly(r.RemoteAddr)
	}
	v, _ := s.inviteHits.LoadOrStore(ip, &hitWindow{})
	hw := v.(*hitWindow)
	hw.mu.Lock()
	defer hw.mu.Unlock()
	now := time.Now()
	if now.Sub(hw.start) > inviteWindow {
		hw.start, hw.count = now, 0
	}
	hw.count++
	return hw.count <= inviteBurst
}

// invitePage is the one route meant to be reachable without console auth: the
// standalone page behind an invite link. It reveals nothing beyond what the
// pasted-into-chat invite message would contain, and every invalid, expired,
// exhausted, or already-used code renders the same dead page. Codes carry 40
// bits of entropy and live minutes, so the URL is as secret as the code
// itself.
func (s *Server) invitePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Add("Vary", "Accept-Language")
	lang := pageLang(r)
	data := invitePageData{Dead: true, Lang: lang}
	status := http.StatusNotFound
	if !s.inviteAllowed(r) {
		status = http.StatusTooManyRequests
	} else if s.Inviter != nil {
		if space, command, expires, err := s.Inviter.Peek(r.PathValue("code")); err == nil {
			left := int(time.Until(expires).Minutes())
			if left < 1 {
				left = 1
			}
			data = invitePageData{Space: space, PairCommand: command, MinutesLeft: left,
				InstallPOSIX: dist.GuestInstallPOSIX, InstallWindows: dist.GuestInstallWindows, Lang: lang}
			status = http.StatusOK
		}
	}
	w.WriteHeader(status)
	inviteTmpl.Execute(w, data)
}
