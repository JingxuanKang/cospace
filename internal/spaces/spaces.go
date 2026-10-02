// Package spaces manages space lifecycle and membership. Spaces are Apple
// containers plus a per-space state dir under <Dir>/spaces/<name>/ holding
// space.json and the workspace volume. Real credentials never enter a space:
// each space gets an opaque fake token that the gateway swaps at the edge.
package spaces

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/JingxuanKang/cospace/internal/container"
)

// Typed error classes the API maps onto HTTP statuses. Wrap with %w.
var (
	ErrInvalid   = errors.New("invalid request")
	ErrConflict  = errors.New("conflict")
	ErrVMMissing = errors.New("the space's VM no longer exists")
)

// codexModelCatalog is a snapshot of the official codex model catalog
// (gpt-5.6 sol/terra/luna, all verified through the gateway). codex only
// enumerates models for its built-in provider; a custom provider gets an
// empty /model picker unless a model_catalog_json supplies the entries.
// Refresh by copying the wanted entries from a current official catalog.
//
//go:embed codex_models.json
var codexModelCatalog string

var (
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}$`)
	// memberRe bounds guest-supplied member names: they land in space.json,
	// the console, and a URL path segment, never in a shell.
	memberRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)
	// commentRe keeps only an innocuous key comment (user@host style).
	commentRe = regexp.MustCompile(`^[A-Za-z0-9@._+-]{1,64}$`)
)

// ValidName reports whether name is a well-formed space name. Every entry
// point that receives a name from outside (API path, CLI arg, guest pairing)
// must pass this before the name touches the filesystem.
func ValidName(name string) bool { return nameRe.MatchString(name) }

func checkName(name string) error {
	if !ValidName(name) {
		return fmt.Errorf("%w: invalid space name %q", ErrInvalid, name)
	}
	return nil
}

// ValidMemberName reports whether a guest-supplied member name is acceptable.
func ValidMemberName(name string) bool { return memberRe.MatchString(name) }

// ParsePublicKey canonicalizes one OpenSSH public key line. Only the key type
// and blob survive (plus a short sanitized comment): no authorized_keys
// options, no second line, nothing a guest could use to smuggle shell or sshd
// directives into the space. The fingerprint is OpenSSH's SHA256 form.
func ParsePublicKey(raw string) (canonical, fingerprint string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\r\n\x00") {
		return "", "", fmt.Errorf("%w: not an OpenSSH public key (want a single line)", ErrInvalid)
	}
	if len(raw) > 16*1024 {
		return "", "", fmt.Errorf("%w: public key is too long", ErrInvalid)
	}
	pk, comment, options, _, perr := ssh.ParseAuthorizedKey([]byte(raw))
	if perr != nil {
		return "", "", fmt.Errorf("%w: not an OpenSSH public key", ErrInvalid)
	}
	if len(options) > 0 {
		return "", "", fmt.Errorf("%w: public key options are not accepted", ErrInvalid)
	}
	canonical = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))
	if c := strings.TrimSpace(comment); c != "" && commentRe.MatchString(c) {
		canonical += " " + c
	}
	return canonical, ssh.FingerprintSHA256(pk), nil
}

// Normalize turns a human-typed space name into a valid one: lowercase, with
// any run of non-[a-z0-9] characters collapsed to a single hyphen and hyphens
// trimmed from the ends. Returns "" when nothing usable remains.
func Normalize(name string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevHyphen = false
		} else if !prevHyphen && b.Len() > 0 {
			b.WriteByte('-')
			prevHyphen = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 31 {
		out = strings.Trim(out[:31], "-")
	}
	return out
}

type Member struct {
	Name        string    `json:"name"`
	PubKey      string    `json:"pub_key"`
	Fingerprint string    `json:"fingerprint"`
	AddedAt     time.Time `json:"added_at"`
}

type Space struct {
	Name      string    `json:"name"`
	Token     string    `json:"token"`
	MemoryGB  int       `json:"memory_gb"`
	CPUs      int       `json:"cpus"`
	CreatedAt time.Time `json:"created_at"`
	HostKey   string    `json:"host_key,omitempty"`
	HostKeyFP string    `json:"host_key_fp,omitempty"`
	// ManualSleep distinguishes an explicit Host sleep from an idle stop. A
	// manually sleeping space stays down until the Host wakes it; an idle-stopped
	// space may still wake on the next SSH connection.
	ManualSleep bool `json:"manual_sleep,omitempty"`
	// Providers is nil for legacy/unconfigured spaces (default all), while an
	// explicit empty slice means this space has every AI tool disabled.
	Providers []string `json:"providers"`
	// UsdLimit caps the space's cumulative equivalent-dollar spend; 0 = no cap
	// (meter only). MaxConcurrency caps concurrent AI requests; 0 = use the
	// default (DefaultMaxConcurrency).
	UsdLimit       float64 `json:"usd_limit,omitempty"`
	MaxConcurrency int     `json:"max_concurrency,omitempty"`
	// FullAuto preconfigures the space's agents to run without permission
	// prompts (claude bypassPermissions, codex never/danger-full-access). The
	// safety boundary is the space itself — disposable VM, fake token, spend
	// cap — not per-action approval. New spaces default to on; spaces from
	// before the field exists stay off.
	FullAuto              bool   `json:"full_auto,omitempty"`
	CodexRoute            string `json:"codex_route,omitempty"`
	CodexModel            string `json:"codex_model,omitempty"`
	NetworkMode           string `json:"network_mode,omitempty"`
	NetworkControlVersion int    `json:"network_control_version,omitempty"`
	// RecoveryImage names the image a network upgrade built from this space's
	// root filesystem. It is recorded before the old VM is deleted, so a crash
	// or failure between delete and recreate leaves a space that Start can
	// rebuild instead of a permanently missing one.
	RecoveryImage string `json:"recovery_image,omitempty"`
	// WakeBlocked records why the daemon last had to stop this space (a
	// gateway-only space whose firewall could not be applied). While set,
	// wake-on-connect is refused so a reconnecting SSH client cannot spin the
	// VM through start→fail→stop forever; an explicit Host Start clears it.
	WakeBlocked string   `json:"wake_blocked,omitempty"`
	Members     []Member `json:"members"`
}

// DefaultMaxConcurrency bounds concurrent AI requests per space when a space
// doesn't set its own. A space should always have some cap (unbounded
// concurrency is the main trigger for upstream abuse detection).
const DefaultMaxConcurrency = 5

// allProviders is the default provider set for a space that doesn't restrict.
var allProviders = []string{"anthropic", "openai", "xai"}

// providersOf returns the space's enabled providers, defaulting to all only
// when the field is absent (backward compatible with legacy space.json files).
func providersOf(r *Space) []string {
	if r.Providers == nil {
		return allProviders
	}
	return r.Providers
}

func hasProvider(r *Space, name string) bool {
	for _, p := range providersOf(r) {
		if p == name {
			return true
		}
	}
	return false
}

// Status is a space merged with live container state.
type Status struct {
	Space
	State string // running / stopped / missing
	IP    string
}

type Manager struct {
	Dir        string // data root; spaces live under Dir/spaces/<name>
	C          container.Client
	Image      string
	GatewayURL string // reachable from inside spaces, e.g. http://192.168.64.1:18930
	CodexModel string // model name codex uses in spaces (host-specific); default gpt-5.6-sol
	// ClaudeModel, when set, becomes the default model in every space's managed
	// claude settings (e.g. "claude-fable-5[1m]"). Useful because claude's
	// model picker in API-token auth mode doesn't list subscription-tier
	// models even though the gateway serves them fine. Empty = claude default.
	ClaudeModel string
	// OnDeleted, when set, runs after a space is purged (the daemon uses it to
	// release the space's transport node).
	OnDeleted func(name string)

	mu            sync.Mutex
	migrate       sync.Once
	upgrading     map[string]bool
	upgradeErrors map[string]string
}

// lock serializes mutating operations both within this process (m.mu) and
// across processes (an advisory flock on the data dir): the cospaced CLI and a
// running daemon share the same space files, and two load→save cycles racing
// would silently drop a member or a manual_sleep flag.
func (m *Manager) lock() func() {
	m.mu.Lock()
	m.migrateLayout()
	_ = os.MkdirAll(m.Dir, 0o700)
	f, err := os.OpenFile(filepath.Join(m.Dir, ".spaces.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return m.mu.Unlock
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return m.mu.Unlock
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		m.mu.Unlock()
	}
}

func (m *Manager) codexModel() string {
	if m.CodexModel == "" {
		return "gpt-5.6-sol"
	}
	return m.CodexModel
}

func (m *Manager) spaceDir(name string) string  { return filepath.Join(m.Dir, "spaces", name) }
func (m *Manager) spaceFile(name string) string { return filepath.Join(m.spaceDir(name), "space.json") }

// migrateLayout adopts a data dir written before the room→space rename:
// rooms/ becomes spaces/ and each room.json becomes space.json. Runs once per
// Manager; renames are best-effort and idempotent.
func (m *Manager) migrateLayout() {
	m.migrate.Do(func() {
		legacy := filepath.Join(m.Dir, "rooms")
		current := filepath.Join(m.Dir, "spaces")
		if _, err := os.Stat(current); os.IsNotExist(err) {
			if _, err := os.Stat(legacy); err == nil {
				os.Rename(legacy, current)
			}
		}
		entries, err := os.ReadDir(current)
		if err != nil {
			return
		}
		for _, e := range entries {
			old := filepath.Join(current, e.Name(), "room.json")
			if _, err := os.Stat(old); err == nil {
				os.Rename(old, filepath.Join(current, e.Name(), "space.json"))
			}
		}
	})
}

// load reads one space record. The name is validated before it is joined
// into a path, and the record must name itself: a file reached through any
// other route (a guest-planted space.json under a workspace mount, say) is
// treated as corrupt rather than trusted.
func (m *Manager) load(name string) (*Space, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	m.migrateLayout()
	b, err := os.ReadFile(m.spaceFile(name))
	if err != nil {
		return nil, fmt.Errorf("space %s: %w", name, err)
	}
	var r Space
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("space %s: corrupt state: %w", name, err)
	}
	if r.Name != name {
		return nil, fmt.Errorf("space %s: corrupt state: record names %q", name, r.Name)
	}
	return &r, nil
}

func (m *Manager) save(r *Space) error {
	if err := checkName(r.Name); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.spaceFile(r.Name) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.spaceFile(r.Name))
}

// containerState reports the runtime state of a space's VM; found is false
// when the runtime no longer knows the container at all.
func (m *Manager) containerState(name string) (state string, found bool, err error) {
	all, err := m.C.List()
	if err != nil {
		return "", false, err
	}
	for _, st := range all {
		if st.Name == name {
			return st.State, true, nil
		}
	}
	return "", false, nil
}

// CreateOptions carries everything a space can be born with. A nil Providers
// means all providers; zero limits mean uncapped / default concurrency.
type CreateOptions struct {
	CodexRoute     string
	CodexModel     string
	NetworkMode    string
	MemoryGB       int
	CPUs           int
	Providers      []string
	UsdLimit       float64
	MaxConcurrency int
	FullAuto       bool
}

func (m *Manager) Create(name string, memoryGB, cpus int) (*Space, error) {
	return m.CreateWithOptions(name, CreateOptions{MemoryGB: memoryGB, CPUs: cpus, FullAuto: true})
}

func (m *Manager) CreateWithOptions(name string, o CreateOptions) (*Space, error) {
	defer m.lock()()
	name = Normalize(name)
	if err := m.validateSettings(o.CodexRoute, o.CodexModel, o.NetworkMode); err != nil {
		return nil, err
	}
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("%w: invalid space name %q: use at least 2 characters (letters, digits, hyphens)", ErrInvalid, name)
	}
	if o.NetworkMode == "gateway-only" {
		// Fail before a VM exists rather than fail-closing every boot later.
		if _, err := networkRules(m.GatewayURL); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	if _, err := os.Stat(m.spaceFile(name)); err == nil {
		return nil, fmt.Errorf("%w: space %q already exists", ErrConflict, name)
	}
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	providers := o.Providers
	if providers == nil {
		providers = allProviders
	}
	if o.MemoryGB <= 0 {
		o.MemoryGB = 2
	}
	if o.CPUs <= 0 {
		o.CPUs = 4
	}
	r := &Space{
		Name:           name,
		Token:          "cs_" + hex.EncodeToString(tok),
		MemoryGB:       o.MemoryGB,
		CPUs:           o.CPUs,
		CreatedAt:      time.Now().UTC(),
		Providers:      append([]string(nil), providers...),
		UsdLimit:       o.UsdLimit,
		MaxConcurrency: o.MaxConcurrency,
		FullAuto:       o.FullAuto,
		CodexRoute:     o.CodexRoute, CodexModel: o.CodexModel, NetworkMode: o.NetworkMode,
		NetworkControlVersion: 1,
	}
	ws := filepath.Join(m.spaceDir(name), "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return nil, err
	}
	if err := m.save(r); err != nil {
		return nil, err
	}
	err := m.C.RunDetached(container.RunSpec{
		Name:     name,
		Image:    m.Image,
		MemoryGB: o.MemoryGB,
		CPUs:     o.CPUs,
		Mounts:   []container.Mount{{Host: ws, Guest: "/workspace"}},
	})
	if err != nil {
		os.RemoveAll(m.spaceDir(name))
		return nil, fmt.Errorf("start container: %w", err)
	}
	// A space that fails between VM start and runtime sync has no per-space
	// host key yet; leaving it behind would let the next Start adopt the
	// image's shared key as this space's identity. Tear it down completely.
	initialized := false
	defer func() {
		if !initialized {
			_ = m.C.Delete(name)
			os.RemoveAll(m.spaceDir(name))
		}
	}()
	if err := m.C.WaitReady(name); err != nil {
		return nil, fmt.Errorf("space created but not ready: %w", err)
	}
	// The image ships host keys baked in at build time, shared by every space
	// built from it. Replace them with per-space keys before anyone connects.
	out, err := m.C.Exec(name, "rm -f /etc/ssh/ssh_host_* && ssh-keygen -A && kill -HUP 1 && cat /etc/ssh/ssh_host_ed25519_key.pub")
	if err != nil {
		return nil, fmt.Errorf("regenerate host keys: %w", err)
	}
	if err := setHostIdentity(r, out); err != nil {
		return nil, fmt.Errorf("capture space host key: %w", err)
	}
	if err := m.save(r); err != nil {
		return nil, err
	}
	if err := m.syncRuntime(r); err != nil {
		return nil, fmt.Errorf("space created but runtime sync failed: %w", err)
	}
	initialized = true
	return r, nil
}

// syncRuntime pushes the per-space env (gateway URLs + fake token) and the
// current authorized_keys into a running container. Called on create, start,
// and membership changes — container filesystem state must always be
// reproducible from space.json.
func (m *Manager) syncRuntime(r *Space) error {
	if err := m.syncNetwork(r); err != nil {
		return err
	}
	// Guest-controlled bytes never go through a heredoc: the authorized_keys
	// content travels base64-encoded inside single quotes and is decoded in
	// the VM, so no key material can terminate the script early.
	var keys []string
	for _, mem := range r.Members {
		keys = append(keys, mem.PubKey)
	}
	authorizedKeys := base64.StdEncoding.EncodeToString([]byte(strings.Join(keys, "\n") + "\n"))

	// Per-space env: only inject a provider's credentials when this space enables
	// it, so an openai-only space genuinely has no claude access and vice versa.
	var env strings.Builder
	fmt.Fprintf(&env, "export COSPACE_SPACE=%s\n", r.Name)
	if hasProvider(r, "anthropic") {
		fmt.Fprintf(&env, "export ANTHROPIC_BASE_URL=%s/anthropic\n", m.GatewayURL)
		fmt.Fprintf(&env, "export ANTHROPIC_AUTH_TOKEN=%s\n", r.Token)
	}
	if hasProvider(r, "openai") {
		// No /v1 suffix: the endpoint path shape is decided by the upstream URL
		// (chatgpt.com/backend-api/codex has no /v1; api.openai.com carries it
		// in -openai-upstream). Spaces just hit <gateway>/openai/<endpoint>.
		fmt.Fprintf(&env, "export OPENAI_BASE_URL=%s/openai\n", m.GatewayURL)
		fmt.Fprintf(&env, "export OPENAI_API_KEY=%s\n", r.Token)
	}
	if hasProvider(r, "xai") {
		// grok CLI BYOK mode: a custom models base URL switches it to plain
		// Bearer auth with XAI_API_KEY — no `grok login` needed in the space.
		fmt.Fprintf(&env, "export GROK_MODELS_BASE_URL=%s/xai\n", m.GatewayURL)
		fmt.Fprintf(&env, "export XAI_API_KEY=%s\n", r.Token)
	}

	// codex needs a config file (env base_url is ignored) that routes through
	// the gateway over HTTP (no websocket) with the host's codex model. The
	// full-auto keys are top-level TOML and must precede the table header.
	codexBlock := "rm -f /home/space/.codex/config.toml\n"
	if hasProvider(r, "openai") {
		fullAutoKeys := ""
		if r.FullAuto {
			fullAutoKeys = "approval_policy = \"never\"\nsandbox_mode = \"danger-full-access\"\n"
		}
		fullAutoKeys += webSearchConfig(r)
		codexCfg := fmt.Sprintf(`model = "%s"
model_provider = "cospace"
model_reasoning_effort = "xhigh"
plan_mode_reasoning_effort = "xhigh"
model_reasoning_summary = "auto"
personality = "pragmatic"
review_model = "%s"
project_doc_fallback_filenames = ["AGENTS.md", "CLAUDE.md"]
model_catalog_json = "/home/space/.codex/models.json"
%s[model_providers.cospace]
name = "cospace"
base_url = "%s/openai"
wire_api = "responses"
supports_websockets = false
env_key = "OPENAI_API_KEY"

[projects."/home/space"]
trust_level = "trusted"

[projects."/workspace"]
trust_level = "trusted"
`, m.EffectiveCodexModel(r), m.EffectiveCodexModel(r), fullAutoKeys, m.GatewayURL)
		codexBlock = fmt.Sprintf(`mkdir -p /home/space/.codex
cat > /home/space/.codex/config.toml <<'GREOF'
%sGREOF
cat > /home/space/.codex/models.json <<'GREOF'
%s
GREOF
`, codexCfg, codexModelCatalog)
	}

	// The managed claude settings always carry a default statusline (space
	// name, model, cwd — a fresh VM has no dotfiles to inherit one from) and,
	// for full-auto spaces, the bypassPermissions default. Rewritten or
	// removed wholesale on every sync, like the codex config.
	// Claude settings are MERGED, not overwritten: guests customize their own
	// statusline and permissions inside a space (claude's /statusline rewrites
	// this file), and a wholesale rewrite here would clobber them on every
	// sync. Managed keys: permissions.defaultMode follows full-auto; the
	// statusline and optional default model are seeded only when absent.
	claudeBlock := "rm -f /home/space/.claude/settings.json\n"
	if hasProvider(r, "anthropic") {
		claudeBlock = fmt.Sprintf(`cat > /home/space/.claude/.cospace-managed.json <<'GREOF'
%s
GREOF
python3 - <<'PYEOF'
import json
managed = json.load(open('/home/space/.claude/.cospace-managed.json'))
p = '/home/space/.claude/settings.json'
try:
    with open(p) as f:
        d = json.load(f)
except Exception:
    d = {}
perms = d.setdefault('permissions', {})
if managed.get('permissions'):
    perms['defaultMode'] = managed['permissions']['defaultMode']
elif perms.get('defaultMode') == 'bypassPermissions':
    del perms['defaultMode']
if 'statusLine' not in d and 'statusLine' in managed:
    d['statusLine'] = managed['statusLine']
if 'model' not in d and 'model' in managed:
    d['model'] = managed['model']
with open(p, 'w') as f:
    json.dump(d, f)
PYEOF
`, claudeSettingsJSON(r.FullAuto, m.ClaudeModel))
	}

	// grok switches to plain API-key auth (no login) only when pinned to it;
	// XAI_API_KEY + GROK_MODELS_BASE_URL alone still demand `grok login`.
	grokBlock := "rm -f /home/space/.grok/config.toml\n"
	if hasProvider(r, "xai") {
		grokBlock = `mkdir -p /home/space/.grok
cat > /home/space/.grok/config.toml <<'GREOF'
[auth]
preferred_method = "api_key"
GREOF
`
	}

	// Pre-accept claude's folder-trust and bypass-permissions dialogs for the
	// two directories every guest lands in: the boundary here is the space
	// itself, so per-folder trust prompts are pure friction (and confusing
	// when several guests each hit them). Merge, never overwrite — claude
	// keeps its own state in the same file.
	// Containers built before the room→space rename still carry the Linux user
	// "room". Rename it in place (same uid, home moved, old path symlinked) the
	// first time the daemon syncs such a VM; every later line assumes "space".
	script := fmt.Sprintf(`set -e
if ! getent passwd space >/dev/null 2>&1 && getent passwd room >/dev/null 2>&1; then
  usermod -l space -d /home/space -m room
  getent group room >/dev/null 2>&1 && groupmod -n space room
  ln -sfn /home/space /home/room
fi
rm -f /etc/profile.d/guestroom.sh
cat > /etc/profile.d/cospace.sh <<'GREOF'
%sGREOF
mkdir -p /home/space/.ssh /home/space/.codex /home/space/.grok /home/space/.claude
%s%s%spython3 - <<'PYEOF'
import json
p = '/home/space/.claude.json'
try:
    with open(p) as f:
        d = json.load(f)
except Exception:
    d = {}
d['hasCompletedOnboarding'] = True
d['bypassPermissionsModeAccepted'] = True
projects = d.setdefault('projects', {})
for path in ('/home/space', '/workspace'):
    projects.setdefault(path, {})['hasTrustDialogAccepted'] = True
with open(p, 'w') as f:
    json.dump(d, f)
PYEOF
chown space:space /home/space/.claude.json
printf '%%s' '%s' | base64 -d > /home/space/.ssh/authorized_keys
chown -R space:space /home/space/.ssh /home/space/.codex /home/space/.grok /home/space/.claude
chmod 700 /home/space/.ssh
chmod 600 /home/space/.ssh/authorized_keys
`, env.String(), codexBlock, grokBlock, claudeBlock, authorizedKeys)
	_, err := m.C.Exec(r.Name, script)
	return err
}

// SetProviders replaces a space's enabled AI providers and re-syncs the space.
func (m *Manager) SetProviders(space string, providers []string) error {
	defer m.lock()()
	r, err := m.loadForUpdate(space)
	if err != nil {
		return err
	}
	valid := map[string]bool{"anthropic": true, "openai": true, "xai": true}
	clean := make([]string, 0, len(providers))
	seen := map[string]bool{}
	for _, p := range providers {
		if !valid[p] {
			return fmt.Errorf("%w: unknown provider %q", ErrInvalid, p)
		}
		if !seen[p] {
			clean = append(clean, p)
			seen[p] = true
		}
	}
	old := *r
	r.Providers = clean
	return m.applyRuntimeSettings(&old, r)
}

// claudeSettingsJSON renders the managed claude settings for a space. The
// statusline prints a space-name line and then runs claude-hud (model,
// reasoning effort, cwd, context — the same HUD hosts typically run), falling
// back to a jq one-liner on images built before claude-hud was baked in.
func claudeSettingsJSON(fullAuto bool, model string) string {
	// The space-name prefix is spliced into the first output line: claude
	// renders only a couple of statusline lines, so a standalone prefix line
	// gets pushed out when the renderer prints two of its own. The renderer
	// is the script the claude-hud npm installer drops at statusline-command.sh
	// (baked into the image); never invoke `claude-hud` itself here — it is an
	// INSTALLER that reinstalls and rewrites config on every run, and a
	// failing statusline command makes claude self-heal settings.json.
	const jqFallback = `jq -r '(.model.display_name // "claude") + "  \u00b7  " + (.workspace.current_dir // ".")'`
	command := `pfx="$(printf '\033[1;38;2;245;158;11m[%s]\033[0m' "${COSPACE_SPACE:-space}")"; { if [ -x "$HOME/.claude/statusline-command.sh" ]; then bash "$HOME/.claude/statusline-command.sh"; else ` + jqFallback + `; fi; } | awk -v p="$pfx" 'NR==1{print p "  " $0; next} {print}'`
	settings := map[string]any{
		"statusLine": map[string]any{"type": "command", "command": command, "padding": 0},
	}
	if model != "" {
		settings["model"] = model
	}
	if fullAuto {
		settings["permissions"] = map[string]string{"defaultMode": "bypassPermissions"}
	}
	b, _ := json.Marshal(settings)
	return string(b)
}

// SetFullAuto switches a space's agents between full-auto (no permission
// prompts) and the tools' default approval flows, then re-syncs the space.
func (m *Manager) SetFullAuto(space string, on bool) error {
	defer m.lock()()
	r, err := m.loadForUpdate(space)
	if err != nil {
		return err
	}
	old := *r
	r.FullAuto = on
	return m.applyRuntimeSettings(&old, r)
}

// ProviderEnabled is the gateway-side enforcement boundary for per-space tool
// selection. Removing env/config from a container is only UX; this check also
// prevents a guest from manually calling a disabled provider with the space's
// fake token.
//
// Read-only policy lookups deliberately take no lock: space.json is replaced
// atomically, so a reader sees either the old or the new record, and the
// gateway's per-request checks must never queue behind a VM boot that holds
// the manager lock for seconds.
func (m *Manager) ProviderEnabled(space, provider string) bool {
	r, err := m.load(space)
	return err == nil && hasProvider(r, provider)
}

func (m *Manager) Get(name string) (*Space, error) {
	return m.load(name)
}

// HostIdentity returns the exact ED25519 host key guests must pin. Spaces made
// before host-key pinning are upgraded lazily from the running container; a
// stopped legacy space is woken once so its existing key can be persisted,
// unless the Host put it to sleep on purpose.
func (m *Manager) HostIdentity(name string) (string, string, error) {
	defer m.lock()()
	r, err := m.loadForUpdate(name)
	if err != nil {
		return "", "", err
	}
	if r.HostKey != "" {
		if fp, err := fingerprint(r.HostKey); err == nil && fp == r.HostKeyFP {
			return r.HostKey, r.HostKeyFP, nil
		}
	}
	on, err := m.running(name)
	if err != nil {
		return "", "", err
	}
	if !on {
		if r.ManualSleep {
			return "", "", fmt.Errorf("%w: space %s is sleeping until the Host wakes it", ErrConflict, name)
		}
		// startLocked captures and persists the host key of a legacy space.
		if err := m.startLocked(r); err != nil {
			return "", "", fmt.Errorf("wake space for host key: %w", err)
		}
	}
	if r.HostKey == "" {
		out, err := m.C.Exec(name, "cat /etc/ssh/ssh_host_ed25519_key.pub")
		if err != nil {
			return "", "", fmt.Errorf("read space host key: %w", err)
		}
		if err := setHostIdentity(r, out); err != nil {
			return "", "", err
		}
		if err := m.save(r); err != nil {
			return "", "", err
		}
	}
	return r.HostKey, r.HostKeyFP, nil
}

// Limits implements gateway.SpacePolicy: the space's spend cap (0 = uncapped) and
// effective concurrency cap (defaulting when unset). A missing space falls back
// to uncapped spend + the default concurrency so a lookup race never blocks.
func (m *Manager) Limits(space string) (usdLimit float64, maxConcurrency int) {
	r, err := m.load(space)
	if err != nil {
		return 0, DefaultMaxConcurrency
	}
	mc := r.MaxConcurrency
	if mc <= 0 {
		mc = DefaultMaxConcurrency
	}
	return r.UsdLimit, mc
}

// QuotaKey identifies one concrete space generation. Tokens are regenerated on
// delete + recreate, so quota state cannot leak between spaces that reuse a
// display name. The key remains internal to the daemon and is never logged.
func (m *Manager) QuotaKey(space string) string {
	r, err := m.load(space)
	if err != nil {
		return space
	}
	return r.Token
}

// SetLimits updates a space's spend cap and concurrency cap. These are enforced
// entirely at the gateway, so no container re-sync is needed. A negative value
// leaves that field unchanged.
func (m *Manager) SetLimits(space string, usdLimit float64, maxConcurrency int) error {
	defer m.lock()()
	r, err := m.loadForUpdate(space)
	if err != nil {
		return err
	}
	if usdLimit >= 0 {
		r.UsdLimit = usdLimit
	}
	if maxConcurrency >= 0 {
		r.MaxConcurrency = maxConcurrency
	}
	return m.save(r)
}

// checkMembership validates a prospective member against the record without
// changing anything. existing is non-nil when the same guest (name AND key) is
// already a member, which callers treat as an idempotent success.
func checkMembership(r *Space, memberName, fp string) (existing *Member, err error) {
	if !ValidMemberName(memberName) {
		return nil, fmt.Errorf("%w: invalid member name %q (letters, digits, . _ -; max 32)", ErrInvalid, memberName)
	}
	for i := range r.Members {
		mem := &r.Members[i]
		if mem.Name == memberName && mem.Fingerprint == fp {
			return mem, nil
		}
		if mem.Name == memberName {
			return nil, fmt.Errorf("%w: member %q already exists in %s with a different key; pair again with -name <other-name>", ErrConflict, memberName, r.Name)
		}
		if mem.Fingerprint == fp {
			return nil, fmt.Errorf("%w: key %s already registered as %q", ErrConflict, fp, mem.Name)
		}
	}
	return nil, nil
}

// CanAddMember runs every check AddMember would, without mutating anything,
// so the pairing flow can refuse a bad request before it burns the one-time
// code.
func (m *Manager) CanAddMember(space, memberName, pubKey string) error {
	_, fp, err := ParsePublicKey(pubKey)
	if err != nil {
		return err
	}
	r, err := m.loadForUpdate(space)
	if err != nil {
		return err
	}
	_, err = checkMembership(r, memberName, fp)
	return err
}

// AddMember registers a guest key. The running VM is updated first and the
// record persisted only when that succeeds, so the console never shows a
// member the space itself does not accept.
func (m *Manager) AddMember(space, memberName, pubKey string) (*Member, error) {
	defer m.lock()()
	memberName = strings.TrimSpace(memberName)
	canonical, fp, err := ParsePublicKey(pubKey)
	if err != nil {
		return nil, err
	}
	r, err := m.loadForUpdate(space)
	if err != nil {
		return nil, err
	}
	if existing, err := checkMembership(r, memberName, fp); err != nil {
		return nil, err
	} else if existing != nil {
		// Re-pairing the same guest is idempotent: re-sync so the VM is
		// certainly current, and report the existing membership as success.
		if on, _ := m.running(space); on {
			if err := m.syncRuntime(r); err != nil {
				return nil, fmt.Errorf("update the space's authorized keys: %w", err)
			}
		}
		return existing, nil
	}
	mem := Member{Name: memberName, PubKey: canonical, Fingerprint: fp, AddedAt: time.Now().UTC()}
	r.Members = append(r.Members, mem)
	if on, err := m.running(space); err != nil {
		return nil, err
	} else if on {
		if err := m.syncRuntime(r); err != nil {
			return nil, fmt.Errorf("update the space's authorized keys: %w", err)
		}
	}
	// A stopped space gets its keys on the next Start (startLocked syncs).
	if err := m.save(r); err != nil {
		return nil, err
	}
	return &mem, nil
}

// RevokeMember removes a guest. The record is updated first (the safe
// direction: a revoked member must never reappear), then the VM; if the VM
// cannot be updated the error says so, because "revoked" must mean the key
// is gone from authorized_keys now, not on the next wake.
func (m *Manager) RevokeMember(space, memberName string) error {
	defer m.lock()()
	r, err := m.loadForUpdate(space)
	if err != nil {
		return err
	}
	kept := r.Members[:0]
	found := false
	for _, mem := range r.Members {
		if mem.Name == memberName {
			found = true
			continue
		}
		kept = append(kept, mem)
	}
	if !found {
		return fmt.Errorf("no member %q in %s", memberName, space)
	}
	r.Members = kept
	if err := m.save(r); err != nil {
		return err
	}
	on, err := m.running(space)
	if err != nil || !on {
		return err // a stopped space syncs its keys on the next Start
	}
	if err := m.syncRuntime(r); err != nil {
		return fmt.Errorf("member removed from the record, but the space's authorized keys could not be updated (it is applied on the next wake): %w", err)
	}
	// Revocation must take effect NOW, not just for new connections. All guests
	// share the `space` user, so we can't target one session — drop every active
	// ssh session (the sshd -D listener survives; valid guests reconnect).
	// Legacy containers from before the rename still run their sessions as
	// the old "room" user; kill both spellings.
	m.C.Exec(r.Name, "pkill -KILL -f 'sshd: (room|space)' || true")
	return nil
}

// Start is the explicit Host wake. It clears both a manual sleep and a
// fail-closed wake block, because the Host is choosing to try again.
func (m *Manager) Start(name string) error {
	defer m.lock()()
	r, err := m.loadForUpdate(name)
	if err != nil {
		return err
	}
	if err := m.startLocked(r); err != nil {
		return err
	}
	if !r.ManualSleep && r.WakeBlocked == "" {
		return nil
	}
	r.ManualSleep = false
	r.WakeBlocked = ""
	return m.save(r)
}

// StartOnConnect starts an idle-stopped space, but never overrides an explicit
// Host sleep or a fail-closed stop. This is the transport-side wake-on-connect
// entry point.
func (m *Manager) StartOnConnect(name string) error {
	defer m.lock()()
	r, err := m.loadForUpdate(name)
	if err != nil {
		return err
	}
	if err := connectionAllowed(r); err != nil {
		return err
	}
	return m.startLocked(r)
}

func connectionAllowed(r *Space) error {
	if r.ManualSleep {
		return fmt.Errorf("%w: space %s is sleeping until the Host wakes it", ErrConflict, r.Name)
	}
	if r.WakeBlocked != "" {
		return fmt.Errorf("%w: space %s needs the Host's attention before it can wake: %s", ErrConflict, r.Name, r.WakeBlocked)
	}
	return nil
}

// ConnectionAllowed enforces a manual sleep even if the underlying container
// was started outside CoSpace. StartOnConnect repeats the check under the
// same manager lock for the normal stopped-space path.
func (m *Manager) ConnectionAllowed(name string) error {
	r, err := m.load(name)
	if err != nil {
		return err
	}
	return connectionAllowed(r)
}

// startLocked boots a space's VM and brings its runtime state up to date.
// Called with the manager lock held.
func (m *Manager) startLocked(r *Space) error {
	name := r.Name
	state, found, err := m.containerState(name)
	if err != nil {
		return err
	}
	if !found {
		if r.RecoveryImage == "" {
			return fmt.Errorf("%w: %s has no VM; delete the space, or recreate it and copy its workspace back", ErrVMMissing, name)
		}
		// A network upgrade got as far as deleting the old VM. Rebuild it from
		// the snapshot image it recorded; the workspace mount is untouched.
		if err := m.C.RunDetached(m.runSpec(r, r.RecoveryImage)); err != nil {
			return fmt.Errorf("recreate %s from recovery image %s: %w", name, r.RecoveryImage, err)
		}
	} else if state != "running" {
		if err := m.C.Start(name); err != nil {
			return err
		}
	}
	// A gateway-only space must not stay up without its firewall. Stop it and
	// record why, so wake-on-connect does not relaunch the same failure every
	// time the guest's SSH client retries.
	failClosed := func(cause error) error {
		if r.EffectiveNetworkMode() == "gateway-only" {
			_ = m.C.Stop(name)
			r.WakeBlocked = cause.Error()
			_ = m.save(r)
		}
		return cause
	}
	if err := m.C.WaitReady(name); err != nil {
		return failClosed(err)
	}
	if r.HostKey == "" {
		out, err := m.C.Exec(name, "cat /etc/ssh/ssh_host_ed25519_key.pub")
		if err != nil {
			return fmt.Errorf("read space host key: %w", err)
		}
		if err := setHostIdentity(r, out); err != nil {
			return err
		}
		if err := m.save(r); err != nil {
			return err
		}
	}
	if err := m.syncRuntime(r); err != nil {
		return failClosed(err)
	}
	if r.NetworkControlVersion < 1 && r.RecoveryImage != "" {
		// Recreated from a network-upgrade snapshot: the new VM has the
		// capability the upgrade was for.
		r.NetworkControlVersion = 1
		return m.save(r)
	}
	return nil
}

// runSpec is the container launch description for a space on a given image.
func (m *Manager) runSpec(r *Space, image string) container.RunSpec {
	return container.RunSpec{
		Name: r.Name, Image: image, MemoryGB: r.MemoryGB, CPUs: r.CPUs,
		Mounts: []container.Mount{{Host: filepath.Join(m.spaceDir(r.Name), "workspace"), Guest: "/workspace"}},
	}
}

// Stop is an explicit Host sleep. Persisting ManualSleep prevents reconnecting
// SSH/Remote SSH clients from immediately waking the space again. A VM that
// has already vanished still records the sleep, so an externally recreated
// container does not auto-wake either.
func (m *Manager) Stop(name string) error {
	defer m.lock()()
	r, err := m.loadForUpdate(name)
	if err != nil {
		return err
	}
	if err := m.C.Stop(name); err != nil {
		return err
	}
	if r.ManualSleep {
		return nil
	}
	r.ManualSleep = true
	return m.save(r)
}

// StopIdle releases an inactive space without disabling wake-on-connect.
func (m *Manager) StopIdle(name string) error {
	defer m.lock()()
	if _, err := m.loadForUpdate(name); err != nil {
		return err
	}
	return m.C.Stop(name)
}

// Delete removes the container and purges the space dir (workspace included).
// A VM that is already gone does not block the purge.
func (m *Manager) Delete(name string) error {
	defer m.lock()()
	if _, err := m.loadForUpdate(name); err != nil {
		return err
	}
	if err := m.C.Delete(name); err != nil {
		return err
	}
	if err := os.RemoveAll(m.spaceDir(name)); err != nil {
		return err
	}
	if m.OnDeleted != nil {
		m.OnDeleted(name)
	}
	return nil
}

// records returns every space record on disk, without consulting the
// container runtime. Entries that fail validation are skipped.
func (m *Manager) records() ([]Space, error) {
	m.migrateLayout()
	entries, err := os.ReadDir(filepath.Join(m.Dir, "spaces"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Space
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		r, err := m.load(e.Name())
		if err != nil {
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}

// Records lists the spaces on disk with no runtime state attached. It never
// shells out, so it is safe wherever the container runtime may be down
// (quota seeding at startup, keepalive provider checks).
func (m *Manager) Records() ([]Space, error) {
	return m.records()
}

func (m *Manager) List() ([]Status, error) {
	defer m.lock()()
	recs, err := m.records()
	if err != nil {
		return nil, err
	}
	infos, err := m.C.List()
	if err != nil {
		return nil, err
	}
	byName := map[string]container.Info{}
	for _, in := range infos {
		byName[in.Name] = in
	}
	var out []Status
	for _, r := range recs {
		st := Status{Space: r, State: "missing"}
		if in, ok := byName[r.Name]; ok {
			st.State, st.IP = in.State, in.IP
		}
		out = append(out, st)
	}
	return out, nil
}

// LookupToken resolves a space fake token, for the gateway's auth check. It
// reads the records straight from disk: the gateway calls this on every AI
// request, and it must never wait on the container CLI or on a VM boot that
// holds the manager lock.
func (m *Manager) LookupToken(token string) (string, bool) {
	recs, err := m.records()
	if err != nil {
		return "", false
	}
	for _, s := range recs {
		if subtle.ConstantTimeCompare([]byte(s.Token), []byte(token)) == 1 {
			return s.Name, true
		}
	}
	return "", false
}

// fingerprint is the OpenSSH SHA256 fingerprint of a "type base64" key line.
func fingerprint(pubKey string) (string, error) {
	_, fp, err := ParsePublicKey(pubKey)
	return fp, err
}

// setHostIdentity extracts the space's ED25519 host public key from command
// output. The output is not just the key: `ssh-keygen -A` prints its
// "generating new host keys" notice to stdout, and container exec merges
// streams — so scan for the key line instead of parsing the whole blob.
func setHostIdentity(r *Space, out string) error {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "ssh-ed25519" {
			continue
		}
		key := fields[0] + " " + fields[1]
		fp, err := fingerprint(key)
		if err != nil {
			return err
		}
		r.HostKey = key
		r.HostKeyFP = fp
		return nil
	}
	return errors.New("space did not provide an ED25519 host key")
}
