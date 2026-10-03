package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/tailscale/tailcat"
)

var errKeyMissing = errors.New("SSH public key is missing")

type pairRequest struct {
	Code     string `json:"code"`
	Member   string `json:"member"`
	PubKey   string `json:"pub_key"`
	Protocol int    `json:"protocol"`
	Version  string `json:"version,omitempty"`
}

// pairRejection is the body the daemon returns with 426 Upgrade Required when
// the client's pair protocol is outside the range it accepts.
type pairRejection struct {
	Error       string `json:"error"`
	MinProtocol int    `json:"min_protocol"`
	MaxProtocol int    `json:"max_protocol"`
}

type pairResponse struct {
	Space       string `json:"space"`
	HostKey     string `json:"host_key"`
	Fingerprint string `json:"fingerprint"`
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, errKeyMissing) {
			fmt.Fprintln(os.Stderr, "cospace:", err)
		}
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("missing command")
	}
	switch args[0] {
	case "connect":
		return runConnect(args[1:], stdin, stdout, stderr)
	case "pair":
		return runPair(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		return runVersion(stdout)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runConnect(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("connect: missing address")
	}
	if len(args) > 2 {
		return fmt.Errorf("connect: unexpected arguments: %s", strings.Join(args[2:], " "))
	}
	port := uint64(22)
	var err error
	if len(args) == 2 {
		port, err = strconv.ParseUint(args[1], 10, 16)
		if err != nil || port == 0 {
			return fmt.Errorf("connect: invalid port %q", args[1])
		}
	}

	client := &tailcat.Client{
		Server: tailcat.ConnBlob(args[0]),
		Logf:   quietLogf(stderr),
	}
	defer client.Close()
	conn, err := client.DialTCPPort(context.Background(), uint16(port))
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()
	go func() {
		_, _ = io.Copy(conn, stdin)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	if _, err := io.Copy(stdout, conn); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	return nil
}

func runPair(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find home directory: %w", err)
	}
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", defaultMemberName(), "member name")
	keyPath := fs.String("key", filepath.Join(home, ".ssh", "id_ed25519.pub"), "SSH public key")
	alias := fs.String("alias", "", "SSH host alias")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return errors.New("pair: missing address")
	}
	if len(pos) < 2 {
		return errors.New("pair: missing code")
	}
	if len(pos) > 2 {
		return fmt.Errorf("pair: unexpected arguments: %s", strings.Join(pos[2:], " "))
	}

	*keyPath = expandHome(*keyPath, home)
	pubKey, err := os.ReadFile(*keyPath)
	if errors.Is(err, os.ErrNotExist) {
		// No SSH key yet — generate one so the guest isn't stopped by a manual
		// ssh-keygen step. Only the default ed25519 key is auto-created.
		privatePath := strings.TrimSuffix(*keyPath, ".pub")
		if err := generateSSHKey(privatePath, stderr); err != nil {
			fmt.Fprintf(stderr, say("Could not create an SSH key automatically. Create one with:\nssh-keygen -t ed25519 -f %s\n", "无法自动创建 SSH 密钥。请手动创建：\nssh-keygen -t ed25519 -f %s\n"), privatePath)
			return errKeyMissing
		}
		pubKey, err = os.ReadFile(*keyPath)
	}
	if err != nil {
		return fmt.Errorf("read public key %q: %w", *keyPath, err)
	}
	if strings.TrimSpace(string(pubKey)) == "" {
		return fmt.Errorf("public key %q is empty", *keyPath)
	}

	tc := &tailcat.Client{
		Server: tailcat.ConnBlob(pos[0]),
		Logf:   quietLogf(stderr),
	}
	defer tc.Close()
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return tc.DialTCPPort(ctx, 80)
		},
	}
	httpClient := &http.Client{Transport: transport}
	resp, err := postPair(context.Background(), httpClient, "http://cospace/pair", pairRequest{
		Code:     pos[1],
		Member:   *name,
		PubKey:   strings.TrimSpace(string(pubKey)),
		Protocol: pairProtocol,
		Version:  version,
	})
	if err != nil {
		return err
	}
	if *alias == "" {
		*alias = resp.Space
	}
	configPath := filepath.Join(home, ".ssh", "config")
	knownHostsPath := filepath.Join(home, ".ssh", "cospace_known_hosts")
	identityFile := strings.TrimSuffix(*keyPath, ".pub")
	hostKeyAlias := managedHostKeyAlias(*alias)
	if err := updateKnownHosts(knownHostsPath, *alias, hostKeyAlias, resp.HostKey); err != nil {
		return err
	}
	if err := updateSSHConfig(configPath, *alias, pos[0], identityFile, knownHostsPath, hostKeyAlias); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, say("paired: %s (%s)\nssh %s\n", "已配对：%s（%s）\n进入空间：ssh %s\n"), resp.Space, resp.Fingerprint, *alias)
	return err
}

// say picks the English or Chinese form of a user-facing message from the
// guest's locale (LC_ALL / LC_MESSAGES / LANG). Reasons that come from the
// host daemon (pair rejections) are passed through as written.
func say(en, zh string) string {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := strings.ToLower(os.Getenv(k)); v != "" {
			if strings.HasPrefix(v, "zh") {
				return zh
			}
			return en
		}
	}
	return en
}

func postPair(ctx context.Context, client *http.Client, endpoint string, req pairRequest) (pairResponse, error) {
	var zero pairResponse
	body, err := json.Marshal(req)
	if err != nil {
		return zero, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return zero, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return zero, fmt.Errorf("pair request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUpgradeRequired {
		var rej pairRejection
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&rej)
		return zero, protocolError(req.Protocol, rej)
	}
	if resp.StatusCode != http.StatusOK {
		// A valid code that is refused for a reason the guest can fix (their
		// default member name is already taken by another machine, say) comes
		// back with that reason; everything else is a bare rejection.
		var rej pairRejection
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&rej)
		if (resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusBadRequest) && rej.Error != "" {
			return zero, fmt.Errorf(say("pairing rejected: %s", "配对被拒绝：%s"), rej.Error)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			return zero, errors.New(say("pairing rejected: too many attempts, wait a minute and try again", "配对被拒绝：尝试太频繁，等一分钟再试"))
		}
		return zero, errors.New(say("pairing rejected (the code may be wrong, used, or expired — ask the host for a fresh one)", "配对被拒绝（码可能输错、已用过或已过期，请让邀请人重新发一个）"))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&zero); err != nil {
		return zero, fmt.Errorf("decode pair response: %w", err)
	}
	if zero.Space == "" || zero.HostKey == "" || zero.Fingerprint == "" {
		return pairResponse{}, errors.New("pair response is incomplete")
	}
	key, fingerprint, err := normalizeHostKey(zero.HostKey)
	if err != nil {
		return pairResponse{}, fmt.Errorf("pair response host key: %w", err)
	}
	if fingerprint != zero.Fingerprint {
		return pairResponse{}, errors.New("pair response host key fingerprint mismatch")
	}
	zero.HostKey = key
	return zero, nil
}

// protocolError turns a 426 from the daemon into an actionable message: an
// outdated guest tool is told to re-run the installer, a guest tool newer
// than the host's daemon is told the host has to update.
func protocolError(have int, rej pairRejection) error {
	if rej.MinProtocol > 0 && have < rej.MinProtocol {
		return fmt.Errorf(say("this cospace build (%s) is too old for the host — update it by running the installer again:\n  %s", "这个 cospace 版本（%s）对这台主机来说太旧了，重新运行安装命令即可更新：\n  %s"), version, installCommand())
	}
	if rej.MaxProtocol > 0 && have > rej.MaxProtocol {
		return fmt.Errorf(say("this cospace build (%s) is newer than the host's CoSpace daemon — ask the host to update cospaced", "这个 cospace 版本（%s）比主机的 CoSpace daemon 更新，请让主机升级 cospaced"), version)
	}
	return errors.New(say("the host's CoSpace daemon does not support this cospace build", "主机的 CoSpace daemon 不支持这个 cospace 版本"))
}

// forwardedPorts are the common dev-server ports cospace auto-forwards from the
// guest's own localhost into the space, so a service started inside the space
// (e.g. a dev server on :3000) opens directly in the guest's browser with no
// extra command. Ports the guest already uses locally are skipped
// (ExitOnForwardFailure no) instead of failing the whole connection. Any other
// port still works via `ssh -L`, and Cursor/VS Code forwards ports on its own.
// Keep in sync with the console's Access card (internal/api/web/index.html).
var forwardedPorts = []int{3000, 5173, 8000, 8080, 8888}

func forwardBlock() string {
	var b strings.Builder
	b.WriteString("  ExitOnForwardFailure no\n")
	for _, p := range forwardedPorts {
		fmt.Fprintf(&b, "  LocalForward %d localhost:%d\n", p, p)
	}
	return b.String()
}

func rewriteSSHConfig(content []byte, alias, addr, toolPath, identityFile, knownHostsFile, hostKeyAlias string, mux bool) ([]byte, error) {
	if !validSSHAlias(alias) {
		return nil, fmt.Errorf("invalid SSH alias %q", alias)
	}
	if addr == "" || strings.ContainsAny(addr, " \t\r\n") {
		return nil, errors.New("invalid transport address")
	}
	// Use the absolute path to the cospace binary the guest actually ran, so `ssh
	// <alias>` works even when cospace is not on PATH (a downloaded binary usually
	// isn't). Fall back to bare "cospace" only if the path can't be resolved.
	if toolPath == "" || strings.ContainsAny(toolPath, "\r\n") {
		toolPath = "cospace"
	}
	proxyCmd := toolPath
	if strings.ContainsAny(toolPath, " \t") {
		proxyCmd = `"` + toolPath + `"`
	}
	identityLine := ""
	if identityFile != "" && !strings.ContainsAny(identityFile, "\r\n") {
		idf := identityFile
		if strings.ContainsAny(idf, " \t") {
			idf = `"` + idf + `"`
		}
		identityLine = fmt.Sprintf("  IdentityFile %s\n  IdentitiesOnly yes\n", idf)
	}
	if knownHostsFile == "" || strings.ContainsAny(knownHostsFile, "\r\n") || hostKeyAlias == "" || strings.ContainsAny(hostKeyAlias, " \t\r\n") {
		return nil, errors.New("invalid managed host-key settings")
	}
	khf := knownHostsFile
	if strings.ContainsAny(khf, " \t") {
		khf = `"` + khf + `"`
	}
	hostKeyLines := fmt.Sprintf("  HostKeyAlias %s\n  UserKnownHostsFile %s\n  StrictHostKeyChecking yes\n  HostKeyAlgorithms ssh-ed25519\n", hostKeyAlias, khf)
	// Multiplex repeat sessions over one connection: a second `ssh <space>`
	// rides the master instead of racing it for the LocalForward ports (the
	// source of harmless-but-noisy "Address already in use" warnings), and
	// reconnects are faster. Windows OpenSSH has no ControlMaster support.
	muxLines := ""
	if mux {
		muxLines = "  ControlMaster auto\n  ControlPath ~/.ssh/cospace-%C.sock\n  ControlPersist 10m\n"
	}
	start := "# >>> cospace:" + alias + " >>>"
	end := "# <<< cospace:" + alias + " <<<"
	block := fmt.Sprintf("%s\nHost %s\n  User space\n%s%s%s  ProxyCommand %s connect %s 22\n%s%s\n", start, alias, identityLine, hostKeyLines, muxLines, proxyCmd, addr, forwardBlock(), end)

	s := string(content)
	var out strings.Builder
	cursor := 0
	inserted := false
	for {
		relStart := strings.Index(s[cursor:], start)
		if relStart < 0 {
			break
		}
		blockStart := cursor + relStart
		relEnd := strings.Index(s[blockStart+len(start):], end)
		if relEnd < 0 {
			return nil, fmt.Errorf("SSH config has an unterminated cospace block for %q", alias)
		}
		blockEnd := blockStart + len(start) + relEnd + len(end)
		if blockEnd < len(s) && s[blockEnd] == '\n' {
			blockEnd++
		}
		out.WriteString(s[cursor:blockStart])
		if !inserted {
			out.WriteString(block)
			inserted = true
		}
		cursor = blockEnd
	}
	out.WriteString(s[cursor:])
	if inserted {
		return []byte(out.String()), nil
	}
	// First pairing: put the managed block BEFORE the user's first Host /
	// Match / Include. ssh_config takes the first matching value for each
	// option, so a block appended after a typical leading "Host *" with
	// StrictHostKeyChecking no or its own ProxyCommand would silently lose
	// host-key pinning or never connect at all.
	rest := out.String()
	if at := firstHostStanza(rest); at >= 0 {
		var withBlock strings.Builder
		withBlock.WriteString(rest[:at])
		withBlock.WriteString(block)
		withBlock.WriteByte('\n')
		withBlock.WriteString(rest[at:])
		return []byte(withBlock.String()), nil
	}
	if len(s) > 0 && !strings.HasSuffix(s, "\n") {
		out.WriteByte('\n')
	}
	if len(s) > 0 && !strings.HasSuffix(out.String(), "\n\n") {
		out.WriteByte('\n')
	}
	out.WriteString(block)
	return []byte(out.String()), nil
}

// firstHostStanza returns the byte offset of the first line that opens a
// Host / Match stanza or pulls one in via Include, or -1 when there is none.
func firstHostStanza(config string) int {
	offset := 0
	for _, line := range strings.SplitAfter(config, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "host ") || strings.HasPrefix(lower, "host\t") || lower == "host" ||
			strings.HasPrefix(lower, "match ") || lower == "match" ||
			strings.HasPrefix(lower, "include ") {
			return offset
		}
		offset += len(line)
	}
	return -1
}

func updateSSHConfig(path, alias, addr, identityFile, knownHostsFile, hostKeyAlias string) error {
	content, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read SSH config: %w", err)
	}
	toolPath := ""
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			toolPath = resolved
		} else {
			toolPath = exe
		}
	}
	toolPath = sshConfigPath(toolPath, runtime.GOOS)
	identityFile = sshConfigPath(identityFile, runtime.GOOS)
	knownHostsFile = sshConfigPath(knownHostsFile, runtime.GOOS)
	updated, err := rewriteSSHConfig(content, alias, addr, toolPath, identityFile, knownHostsFile, hostKeyAlias, runtime.GOOS != "windows")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create SSH directory: %w", err)
	}
	return writeAtomic(path, updated, ".config-*.tmp")
}

func updateKnownHosts(path, alias, hostKeyAlias, hostKey string) error {
	content, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read CoSpace known_hosts: %w", err)
	}
	updated, err := rewriteKnownHosts(content, alias, hostKeyAlias, hostKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create SSH directory: %w", err)
	}
	return writeAtomic(path, updated, ".known-hosts-*.tmp")
}

func writeAtomic(path string, content []byte, pattern string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", filepath.Base(path), err)
	}
	return nil
}

func rewriteKnownHosts(content []byte, alias, hostKeyAlias, hostKey string) ([]byte, error) {
	if !validSSHAlias(alias) || hostKeyAlias == "" || strings.ContainsAny(hostKeyAlias, " \t\r\n") {
		return nil, errors.New("invalid managed host-key alias")
	}
	key, _, err := normalizeHostKey(hostKey)
	if err != nil {
		return nil, err
	}
	start := "# >>> cospace:" + alias + " >>>"
	end := "# <<< cospace:" + alias + " <<<"
	block := fmt.Sprintf("%s\n%s %s\n%s\n", start, hostKeyAlias, key, end)
	s := string(content)
	startAt := strings.Index(s, start)
	if startAt >= 0 {
		endRel := strings.Index(s[startAt+len(start):], end)
		if endRel < 0 {
			return nil, fmt.Errorf("CoSpace known_hosts has an unterminated block for %q", alias)
		}
		endAt := startAt + len(start) + endRel + len(end)
		if endAt < len(s) && s[endAt] == '\n' {
			endAt++
		}
		return []byte(s[:startAt] + block + s[endAt:]), nil
	}
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	if s != "" && !strings.HasSuffix(s, "\n\n") {
		s += "\n"
	}
	return []byte(s + block), nil
}

func normalizeHostKey(pubKey string) (string, string, error) {
	fields := strings.Fields(pubKey)
	if len(fields) < 2 || fields[0] != "ssh-ed25519" {
		return "", "", errors.New("expected an ED25519 OpenSSH public key")
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return "", "", fmt.Errorf("bad host-key encoding: %w", err)
	}
	sum := sha256.Sum256(blob)
	fingerprint := "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
	return fields[0] + " " + fields[1], fingerprint, nil
}

func managedHostKeyAlias(alias string) string {
	sum := sha256.Sum256([]byte(alias))
	return "cospace-" + hex.EncodeToString(sum[:8])
}

func validSSHAlias(alias string) bool {
	if len(alias) == 0 || len(alias) > 128 {
		return false
	}
	for i, r := range alias {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || ((r == '.' || r == '_' || r == '-') && i > 0) {
			continue
		}
		return false
	}
	return true
}

func sshConfigPath(path, goos string) string {
	if goos == "windows" {
		return strings.ReplaceAll(path, `\`, "/")
	}
	return path
}

// generateSSHKey creates a passwordless ed25519 keypair at privatePath using
// the system ssh-keygen, so a first-time guest doesn't have to run it manually.
func quietLogf(stderr io.Writer) func(string, ...any) {
	// tailcat/wireguard are extremely chatty; hide their internals from guests
	// unless COSPACE_DEBUG is set, so only cospace's own messages show.
	if os.Getenv("COSPACE_DEBUG") != "" {
		return log.New(stderr, "cospace: ", 0).Printf
	}
	return func(string, ...any) {}
}

func generateSSHKey(privatePath string, stderr io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(privatePath), 0o700); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "No SSH key found — creating one at %s\n", privatePath)
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", privatePath, "-q")
	cmd.Stdout, cmd.Stderr = stderr, stderr
	return cmd.Run()
}

func defaultMemberName() string {
	for _, key := range []string{"USER", "USERNAME"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return "guest"
}

func expandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
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
		if strings.Contains(arg, "=") {
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
