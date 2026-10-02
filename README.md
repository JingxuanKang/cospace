<div align="center">

<img src="assets/hero.svg" alt="CoSpace: co-op mode for AI coding — shared spaces on your own Mac" width="100%">

# CoSpace — Co-op mode for AI coding.

![host](https://img.shields.io/badge/host-macOS%2026%2B%20·%20Apple%20Silicon-1F2937?style=flat-square)
![guests](https://img.shields.io/badge/guests-macOS%20·%20Linux%20·%20Windows-2563EB?style=flat-square)
![go](https://img.shields.io/badge/Go-1.26-00ADD8?style=flat-square)
![runtime](https://img.shields.io/badge/runtime-Apple%20container-F59E0B?style=flat-square)
![license](https://img.shields.io/badge/license-MIT-16A34A?style=flat-square)

**English** | [简体中文](README.zh-CN.md)

</div>

---

CoSpace is a macOS daemon and a single-file guest CLI that turn your Mac's compute and AI subscriptions into shared Linux dev spaces. It is for developers and coding agents that need to work in one place — same files, same Claude Code sessions, same memory — and for hosts who hand people they trust a ready-to-run agent environment with no signup, no API keys, and no cloud bill.

Each space is a Linux VM (Apple container) on the host's Mac. Guests join over plain `ssh`; `claude`, `codex`, and `grok` inside are already signed in through a credential gateway on the Mac. Real tokens never enter a space — each space holds only a revocable fake token.

<img src="assets/console.jpg" alt="Space detail in the host console: guests with their key fingerprints, a 14-day token chart, the budget card, per-space model and network settings, and the ssh access card with the pinned host key" width="100%">

## Install

**Host** — an Apple Silicon Mac on macOS 26 or later. One command:

```bash
curl -fsSL https://jingxuankang.github.io/cospace/host.sh | sh
```

It installs Apple `container` (through Homebrew when present, otherwise Apple's signed package), downloads the prebuilt `cospaced` daemon, registers it as a login item, and opens the console at `http://127.0.0.1:18931`. On first run the daemon downloads the space image once in the background (about 600 MB) and the console shows the progress; after that every new space starts in seconds. Re-running the command upgrades in place; `cospaced uninstall` removes the service and keeps your spaces. Sign in to `claude`, `codex`, or `grok` on the Mac for whichever AI tools you want spaces to use.

**Guest** — one command, no account (installs `cospace`, alias `co`):

```bash
curl -fsSL https://jingxuankang.github.io/cospace/install.sh | sh     # macOS / Linux
irm https://raw.githubusercontent.com/JingxuanKang/cospace/master/docs/install.ps1 | iex          # Windows PowerShell (needs OpenSSH Client; cross-compiled, not yet verified on a real Windows machine)
```

The same command is the updater: on a machine that already has `cospace` it exits immediately when the installed build matches the published version and replaces it in place otherwise (a Homebrew install — `brew install jingxuankang/tap/cospace` — is left to `brew upgrade`). Guests who already have the tool go straight to `cospace pair`; if their build is too old for your daemon, pairing says so and points them at the installer. `cospace version` prints the build.

Everything is published on GitHub — install scripts on GitHub Pages, binaries on Releases, the space image on ghcr.io — so there is no download server to run.

## Quick start

In the console: **New Space** → name it → **Invite a Guest**, then send your guest the invite link (or the three lines it shows). They paste them once, then:

```bash
ssh acme        # they're in — claude / codex / grok work immediately
```

From the CLI instead: `cospaced space create acme`, then `cospaced invite acme` prints the lines to send.

## One space, people and agents together

Claude Code keeps sessions and memory as plain files in the home directory, so one shared home *is* the collaboration layer — no product features needed on top.

| You want | You do |
|---|---|
| See a teammate's agent runs | `claude --resume` lists everyone's sessions |
| Take over while they sleep | Resume their session with full context |
| Watch or pair live | `tmux attach` |
| Keep commits attributed | Set `git config user.name` once per guest (automatic per-key attribution is planned, not built) |

## A minute to a working Claude Code

Guests never sign up, install an IDE, or touch an API key — the space is pre-authenticated against the host's own accounts. The host stays in control the whole time: a per-space dollar cap and concurrency cap (over-limit requests get a clean 429), per-space provider switches, host-wide provider switches, and revocation that cuts a guest's access the moment you click it. Usage draws on your subscription and its rate limits, so invite people you trust.

Spaces stay signed in while you're away: provider tokens only live for hours, so the daemon watches their expiry and pings each vendor CLI with a minimal cheapest-model request just before they go stale.

## Full-auto spaces

New spaces start full-auto: `claude` uses `bypassPermissions` and `codex` uses `approval_policy = "never"`. Agents run as the unprivileged space user inside a VM, with per-space budget and concurrency limits. They can modify files shared in that space; the host account credentials remain outside it.

Toggle it per space in the console ("AI in this space" → Full-auto agents). Configuration changes are saved for sleeping spaces and applied on wake; a failed live update restores the previous settings.

## Models and Internet access

The space detail's **Model & network** card selects the Codex account (official, optional Sub2API I or II) and default model (GPT-6 Astra or GPT-5.6 Sol/Terra/Luna). Account changes affect new requests; a new Codex session picks up the default model. Sub2API keys stay in the host's Keychain; setup is in [Deploy.md](Deploy.md).

**Internet access** defaults on. Turning it off blocks outgoing IPv4/IPv6 connections except the AI gateway and replies to incoming connections; SSH stays available. The gateway also rejects hosted search, remote MCP and remote media URLs. Local tools and inline images still work. Wake a sleeping space before changing this switch, and restart the agent session after changing its network permissions.

Older spaces show **Enable network controls…** for a one-time upgrade. It saves a complete filesystem snapshot, preserves the workspace, tools and SSH identity, and restarts the VM. Running programs stop. Progress remains visible after a console refresh.

## Templates and budgets

Save any space's setup — size, AI selection, account/model, Internet access, budget, full-auto — as a named template ("Save as Template" in the space detail), then stamp out identical spaces from the create dialog or `cospaced space create -template <name>`. The dollar cap uses estimated token costs, not the account’s actual bill.

## Architecture

```
guest: plain ssh / Cursor Remote SSH
   │
   ├─ tailcat (embedded Tailscale lib: P2P, DERP fallback)   ── no server needed
   └─ your own VPS relay (dumb encrypted pipe)               ── experimental: built and tested, not yet selectable in `cospaced serve`
   ▼
cospaced on the Mac ── credential gateway (token swap + metering + caps)
   ▼                    web console (go:embed, vanilla JS)
Apple container: one Linux VM per space, persistent volume
```

The vendor runs no service: connectivity is peer-to-peer or through the host's own VPS. Full design (Chinese): [DESIGN.md](DESIGN.md).

## Security

- Real OAuth/API tokens live only in the gateway process on the Mac; spaces get per-space fake tokens, swapped at the edge and revocable instantly.
- Spaces cannot see the Mac's home directory, other spaces, or the host's git/ssh identity; per-space ED25519 host keys are pinned by the guest CLI in a dedicated managed `known_hosts`.
- The host can see everything inside a space — it is a collaboration space, not a private VM. Guests spend the host's quota; both facts are by design and disclosed.
- The VPS relay (experimental, not yet wired into `cospaced serve`) forwards encrypted SSH bytes and sees only connection metadata. Its control channel is not yet TLS-pinned; see DESIGN.md §14.
- The console refuses cross-site requests and unexpected Host headers. If you publish it behind a tunnel, pass its hostname with `cospaced serve -console-hosts <host>`; the gateway only answers loopback and the container network unless `-gateway-allow` adds more.

## Development

```bash
go build ./... && go test ./...       # all packages, no root needed
scripts/publish-image.sh --build      # build the space image locally
go run ./cmd/cospaced -image ghcr.io/jingxuankang/cospace-base:1 serve
./scripts/smoke.sh                    # end-to-end on a real Mac (creates and deletes a space)
```

Go 1.26; the console frontend is a single dependency-free HTML file embedded into the daemon. Releases: bump `VERSION`, then `scripts/release.sh`; image changes: bump the tag in `internal/dist`, then `scripts/publish-image.sh` (see [Deploy.md](Deploy.md)).

## License

[MIT](LICENSE).
