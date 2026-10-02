package spaces

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

func CodexRoutes() []string { return []string{"official", "sub2api-i", "sub2api-ii"} }
func CodexModels() []string {
	return []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}
}

func contains(values []string, s string) bool {
	for _, v := range values {
		if v == s {
			return true
		}
	}
	return false
}

func (r *Space) EffectiveCodexRoute() string {
	if r.CodexRoute == "" {
		return "official"
	}
	return r.CodexRoute
}
func (r *Space) EffectiveNetworkMode() string {
	if r.NetworkMode == "" {
		return "open"
	}
	return r.NetworkMode
}
func (m *Manager) EffectiveCodexModel(r *Space) string {
	if r.CodexModel == "" {
		return m.codexModel()
	}
	return r.CodexModel
}

func (m *Manager) validateSettings(route, model, network string) error {
	if route != "" && !contains(CodexRoutes(), route) {
		return fmt.Errorf("%w: invalid Codex route %q", ErrInvalid, route)
	}
	if model != "" && model != m.codexModel() && !contains(CodexModels(), model) {
		return fmt.Errorf("%w: invalid Codex model %q", ErrInvalid, model)
	}
	if network != "" && network != "open" && network != "gateway-only" {
		return fmt.Errorf("%w: invalid network mode %q", ErrInvalid, network)
	}
	return nil
}

// ExecutionPolicy is read by the gateway on each request, so changing a CLI's
// local config cannot select a different sponsor or bypass the network gate.
// Lock-free like the other per-request lookups; a space mid-upgrade still
// answers from its last saved record (its VM is down anyway).
func (m *Manager) ExecutionPolicy(name string) (route string, internet bool, err error) {
	r, err := m.load(name)
	if err != nil {
		return "", false, err
	}
	return r.EffectiveCodexRoute(), r.EffectiveNetworkMode() == "open", nil
}

func (m *Manager) running(name string) (bool, error) {
	all, err := m.C.List()
	if err != nil {
		return false, err
	}
	for _, st := range all {
		if st.Name == name {
			return st.State == "running", nil
		}
	}
	return false, nil
}

func (m *Manager) SetCodex(name, route, model string) error {
	defer m.lock()()
	if route == "" || model == "" {
		return fmt.Errorf("%w: invalid Codex settings: route and model are required", ErrInvalid)
	}
	if err := m.validateSettings(route, model, ""); err != nil {
		return err
	}
	r, err := m.loadForUpdate(name)
	if err != nil {
		return err
	}
	old := *r
	r.CodexRoute, r.CodexModel = route, model
	// Account routing is host-only; no guest files change when the model is unchanged.
	if m.EffectiveCodexModel(&old) == m.EffectiveCodexModel(r) {
		return m.save(r)
	}
	return m.applyRuntimeSettings(&old, r)
}

// applyRuntimeSettings is called with m.mu held. A failed live update cannot
// become the saved/advertised state; stopped spaces apply it on their next wake.
func (m *Manager) applyRuntimeSettings(old, r *Space) error {
	on, err := m.running(r.Name)
	if err != nil {
		return err
	}
	restore := func(cause error) error {
		if on {
			if rollback := m.syncRuntime(old); rollback != nil {
				stopErr := m.C.Stop(r.Name)
				return fmt.Errorf("settings update failed: %v; restore failed: %v; space stop result: %v", cause, rollback, stopErr)
			}
		}
		return cause
	}
	if on {
		if err := m.syncRuntime(r); err != nil {
			return restore(err)
		}
	}
	if err := m.save(r); err != nil {
		return restore(err)
	}
	return nil
}

func (m *Manager) SetNetwork(name, mode string) error {
	defer m.lock()()
	if mode == "" {
		return fmt.Errorf("%w: invalid network mode: required", ErrInvalid)
	}
	if err := m.validateSettings("", "", mode); err != nil {
		return err
	}
	r, err := m.loadForUpdate(name)
	if err != nil {
		return err
	}
	old := *r
	r.NetworkMode = mode
	if mode == "gateway-only" && r.NetworkControlVersion < 1 {
		return fmt.Errorf("%w: network controls need a space upgrade first", ErrConflict)
	}
	on, err := m.running(name)
	if err != nil {
		return err
	}
	if !on && mode != old.EffectiveNetworkMode() {
		return fmt.Errorf("%w: wake this space before changing Internet access", ErrConflict)
	}
	return m.applyRuntimeSettings(&old, r)
}

// networkRules is an atomic replacement of only CoSpace's table. It never
// flushes other firewalls. The inet family covers IPv4 and IPv6 together.
func networkRules(gatewayURL string) (string, error) {
	u, err := url.Parse(gatewayURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("invalid gateway URL")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil {
		return "", fmt.Errorf("network isolation requires a literal gateway IP")
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("invalid gateway port")
	}
	family := "ip"
	if ip.To4() == nil {
		family = "ip6"
	}
	return fmt.Sprintf(`add table inet cospace_egress
flush table inet cospace_egress
add chain inet cospace_egress output { type filter hook output priority 0; policy drop; }
add rule inet cospace_egress output oifname "lo" accept
add rule inet cospace_egress output %s daddr %s tcp dport %d accept
add rule inet cospace_egress output tcp sport 22 accept
add rule inet cospace_egress output ct direction reply accept
add rule inet cospace_egress output meta l4proto 58 icmpv6 type { nd-neighbor-solicit, nd-neighbor-advert, nd-router-solicit } accept
`, family, ip.String(), n), nil
}

func (m *Manager) syncNetwork(r *Space) error {
	if r.EffectiveNetworkMode() == "open" {
		_, err := m.C.Exec(r.Name, `set -eu
if command -v nft >/dev/null 2>&1 && nft list table inet cospace_egress >/dev/null 2>&1; then nft delete table inet cospace_egress; fi
rm -f /etc/cospace-network.nft
`)
		return err
	}
	rules, err := networkRules(m.GatewayURL)
	if err != nil {
		return err
	}
	// Old spaces keep their original root filesystem. Install the small helper
	// on first use; failing to install/apply fails the UI action, never reports Off.
	// The script itself arrives on stdin, so anything apt might read from stdin
	// gets /dev/null instead of the rest of this script.
	script := `set -eu
if ! command -v nft >/dev/null 2>&1; then
  if ! (DEBIAN_FRONTEND=noninteractive apt-get update -qq </dev/null && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends nftables </dev/null) >/tmp/cospace-network-install.log 2>&1; then
    echo 'Cannot install nftables; see /tmp/cospace-network-install.log' >&2; exit 1
  fi
fi
umask 077
cat > /etc/cospace-network.nft.next <<'COSPACE_NFT'
` + rules + `COSPACE_NFT
nft -f /etc/cospace-network.nft.next
mv /etc/cospace-network.nft.next /etc/cospace-network.nft
`
	_, err = m.C.Exec(r.Name, script)
	if err != nil {
		return fmt.Errorf("apply network policy: %w", err)
	}
	return nil
}

func webSearchConfig(r *Space) string {
	if strings.EqualFold(r.EffectiveNetworkMode(), "gateway-only") {
		return "web_search = \"disabled\"\n"
	}
	return ""
}
