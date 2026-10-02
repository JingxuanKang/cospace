# CoSpace 部署与运维

运行环境：Host 为 Apple Silicon + macOS 26+，Apple container ≥1.3；从源码构建需 Go 1.26。

## 安装（Host）

```bash
curl -fsSL https://jingxuankang.github.io/cospace/host.sh | sh
```

`docs/host.sh` 依次：检查机型与系统 → 安装 Apple container（有 Homebrew 用 `brew install container`，否则下载 Apple 签名 pkg 并 `sudo installer`）→ 从 GitHub Releases 下载 `cospaced_darwin_arm64.tar.gz` 装到 `/opt/homebrew/bin`（不可写时依次退到 `/usr/local/bin`、`~/.local/bin`）→ 执行 `cospaced setup`。重复执行即升级。

`cospaced setup` 做的事，也可单独执行（如从源码构建后）：

1. `container system start --enable-kernel-install`（已运行则跳过，首次无交互装内核）；
2. 写 `~/Library/LaunchAgents/dev.cospace.daemon.plist`（Label `dev.cospace.daemon`，RunAtLoad + KeepAlive，PATH 含 container 与 cospaced 所在目录），`launchctl bootout` 旧实例后 `bootstrap`；
3. 等控制台 `http://127.0.0.1:18931` 应答后用浏览器打开（`-no-open` 跳过）。

`--` 之后的参数原样作为 `cospaced serve` 的参数写进 plist，例如：

```bash
cospaced setup -- -idle-timeout 0 -sub2api-i-upstream https://<your-sub2api>/v1
```

一键安装同样可带参数：`curl -fsSL …/host.sh | sh -s -- <参数>`。把控制台发布到公网域名时必须带 `-console-hosts <域名>`（见下文「服务管理」）。

`cospaced uninstall` 停止并移除 launchd 服务；空间、镜像与数据目录保留。

从源码运行：

```bash
go build -o bin/cospaced ./cmd/cospaced
bin/cospaced serve        # 前台调试：网关 0.0.0.0:18930，控制台/API 127.0.0.1:18931
```

## 空间镜像

daemon 使用 `internal/dist` 的 `BaseImage`（`ghcr.io/jingxuankang/cospace-base:<tag>`）。启动时本地没有就在后台 `container image pull --platform linux/arm64`；`GET /api/host` 的 `image` 字段为 `checking / pulling / ready / error` 及进度，控制台显示横幅，就绪前 New Space 不可用、`POST /api/spaces` 返回 409。拉取失败每 30 秒自动重试，原因见日志 `image:` 行。CLI `cospaced space create` 在镜像缺失时同步拉取。

`-image <ref>` 覆盖镜像，例如本地构建的 `cospace-base`。基础镜像包含 nftables、在 SSH 启动前恢复网络规则的 entrypoint，以及系统 `bubblewrap`（Codex 的 Linux sandbox 不退回 bundled helper）。已存在的空间不会因换镜像而换根文件系统，可在保留 workspace 的前提下直接补包：

```bash
container exec <space> sh -lc 'apt-get update && apt-get install -y --no-install-recommends bubblewrap && rm -rf /var/lib/apt/lists/*'
```

## 服务管理

换二进制后用 `launchctl kickstart -k gui/$(id -u)/dev.cospace.daemon` 重启。所有 guest 的 ssh、Cursor Remote SSH 和端口转发都经 daemon 隧道，重启那一刻会全部断开，重连即可；挑空间没人在用的时候做。

macOS 重启后 Apple container 的服务不会自动拉起；`cospaced serve` 启动时先检查 `container system status`，未运行则自行 `container system start`。若 daemon 仍反复退出，手动 `container system start` 后 kickstart，用 `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18931/` 验证。

凭证：无需任何注入步骤。网关每次请求直读本机 claude/codex/grok 的登录文件
（`~/.codex/auth.json`、`~/.grok/auth.json`；claude 同时读 `~/.claude/.credentials.json` 与 Keychain 项 "Claude Code-credentials"，取过期最晚的一份——Claude Code 在 macOS 上实际续期的是 Keychain 那份）。
access token 只活数小时且只有 CLI 运行时才续期，所以 daemon 内置**凭证保活**：
到期前 30 分钟（`-cred-keepalive`，0 关闭）自动 spawn 一次最便宜的 CLI 调用
（claude haiku / codex gpt-5.6-luna low-effort / grok 默认模型）触发官方续期，host 整夜不在
空间也不断线；动作与结果见日志里的 `keepalive:` 行。CLI 按 PATH 加
`~/.local/bin`、`~/.grok/bin`、`/opt/homebrew/bin`、`/usr/local/bin` 查找。控制台的赞助开关（`/api/sponsor`，持久化在 `sponsor.json`）
对应控制台首页的 provider 卡片开关——**按 provider 独立**（可只关 claude 一家，全部空间生效，网关对该家返回 503）；API 请求不带 provider 时三家一起设置。开启后，每个空间的 provider 列表作为第二层策略，关闭某工具后网关返回 403，即使 guest 手工构造请求也不能绕过。

空间内 claude 跟随自身默认模型（`-claude-model` 可覆盖）；
statusline 用镜像内预装的 `claude-hud`（旧镜像空间回退 jq 单行）。codex 的
思考强度/摘要等显示偏好在托管 config.toml 内与 host 对齐（xhigh/auto）。

控制台 **Sleep** / `cospaced space stop <name>` 是持久化人工休眠：`space.json` 写入 `manual_sleep`，SSH 重连不会唤醒；**Wake** / `space start` 才清除。idle reaper（`-idle-timeout`，默认 30 分钟，0 关闭）使用独立的 `StopIdle`，此类自动休眠仍保持 wake-on-connect。

## 每空间账号、模型和联网

控制台空间详情 **Model & network** 提供账号、默认模型及 Internet 开关。官方通道默认走 `https://chatgpt.com/backend-api/codex`。可选的 Sub2API I / II 通道用 `-sub2api-i-upstream` / `-sub2api-ii-upstream` 指定 API 基址（含 `/v1`），未配置时前端置灰。

网关从 Host Keychain 读取 `sub2api-gpt-api-key` / `sub2api-gpt-api-key-ii`，可用 `-sub2api-i-keychain` / `-sub2api-ii-keychain` 覆盖服务名。缓存 30 秒，Host OpenAI 开关每次请求检查，关闭立即拒绝新请求；缺少凭证返回 503，不回退到别的账号。真实 key 不进入空间、模板和日志。

空间配置持久化 `codex_route`、`codex_model`、`network_mode`（`open` / `gateway-only`）和 `network_control_version`；旧字段缺省仍为官方、原默认模型、联网。仅切账号直接更新网关路由；其他运行配置同步成功后才保存，恢复旧配置失败则停机。休眠时可保存模型/providers/full-auto，Internet 切换必须先唤醒；不允许只改停机元数据而漏掉下次启动的防火墙。

新 VM 使用 `--cap-add CAP_NET_ADMIN`，仅 root 可管理 nftables，space 用户没有 capability。关闭联网会原子替换 `inet cospace_egress` 表，IPv4/IPv6 均受限；仅保留 loopback、固定 IP/端口的 AI 网关、入站连接回复和必要的 IPv6 邻居发现。持久规则为 root 管理的 `/etc/cospace-network.nft`。镜像 entrypoint 在启动 SSH 前加载规则；受管唤醒再次同步，失败则停机。网关 URL 须使用容器可达的固定 IP（默认 `192.168.64.1:18930`）。

网关的离线模式只接受模型调用/压缩上下文端点，拒绝云端检索、远端 MCP、远程图片/文件 URL 和不透明的 `previous_response_id`；可使用本地 function/custom 工具及内联图片。gzip/zstd 请求均检查解压后的内容。网络权限改变后应重开 agent 会话，使旧工具列表不再发送已禁用工具。已送达上游的请求可完成。

旧 VM 的 capability 不可原地修改。前端 **Enable network controls…** 弹窗明确提示重启，`POST /api/spaces/{name}/upgrade-network` 返回 202；`GET /api/spaces` 的 `network_upgrading` / `network_upgrade_error` 可跟踪结果。升级先停机并导出完整 rootfs，构建恢复镜像成功后才替换旧 VM，挂回原 workspace，保留 SSH host key、软件、成员和原休眠状态。运行进程不保留。失败前未删除旧 VM 时自动尝试恢复运行；删除后的失败保留完整恢复材料并返回路径。

恢复材料在数据目录 `backups/network/<space>-<UTC>/`（rootfs.tar、Dockerfile、entrypoint.sh）以及本机 `cospace-network-<space>-<UTC>` 镜像。它们含空间数据和假 token，只留 Host，不推仓库/镜像仓库。升级成功后不自动删除备份；确认不再需要时由 Host 清理。升级期间避免重启 daemon。

API：`GET /api/codex-options`；`PUT /api/spaces/{name}/codex` 接收 `{route, model}`；`PUT /api/spaces/{name}/network` 接收 `{internet_access: true|false}`。模板保存同样的选择。

## 健康检查与日志

- `curl -s http://127.0.0.1:18931/api/host` 返回 JSON 即活；其中 `image.state` 为 `ready` 才能建空间
- 日志：launchd 模式在 `~/Library/Logs/CoSpace/cospaced.log`；网关每请求一行（space/provider/status），镜像拉取为 `image:` 行
- 数据目录：`~/Library/Application Support/CoSpace/`（spaces/、invites.json、usage.jsonl、templates.json、transport/）；若该目录不存在而旧的 `…/Guestroom/` 存在，daemon 启动时自动整体接管改名

## Linux 主机（Docker 后端）

daemon 也能跑在装了 Docker 的 Linux 服务器上：`-runtime docker`（Linux 上的默认值），每个空间是一个 `--cap-add NET_ADMIN` 的容器，镜像与 Mac 相同（ghcr 多架构）。网关地址默认取 docker bridge 的网关 IP（通常 `172.17.0.1`，启动时从 `docker network inspect bridge` 读取，`-gateway-url` 可覆盖）；数据目录默认 `~/.local/share/cospace`；没有 caffeinate，电池检测读 `/sys/class/power_supply`。Sub2API 这类 host 密钥在 Linux 上读 `~/.config/cospace/secrets/<service名>`（0600 文件），对应 macOS 的 Keychain 项。

```bash
# 以能用 docker 的普通用户执行（在 docker 组里）
cospaced setup -- -console-hosts <公网域名可选>     # 写 ~/.config/systemd/user/cospaced.service 并 enable --now，开启 linger
journalctl --user -u cospaced -f                   # 日志
cospaced uninstall                                 # 只删服务，保留空间与数据
```

控制台仍只监听 loopback，用 SSH 隧道或 Tailscale 访问。Mac 上不提供 docker 后端（`-runtime docker` 会被拒绝）。

**主机防火墙**：空间通过 docker0 访问宿主的 `18930`。宿主 INPUT 链若是白名单加末尾 REJECT（常见的加固做法），容器内会得到连接失败、空间里的 claude/codex 报网关不可达。放行只限 docker0 的一条规则即可，不要对公网开 18930：

```bash
sudo iptables -I INPUT -i docker0 -p tcp --dport 18930 -j ACCEPT   # 持久化按发行版（iptables-persistent / nftables.conf）
```

`cospaced serve` 在 docker 运行时启动时会打印这条提醒。真机验证（2026-10-02，OCI ARM64 Ubuntu 24.04 + Docker 29）：建空间、成员身份、撤销轮换、停启、控制台、邀请全部通过；网关连通性正是被 INPUT 白名单挡住，加上述规则后通。信任模型差异：真凭证会放在这台服务器上（`~/.claude`、`~/.codex`、`~/.grok` 的登录文件），而不是你自己的 Mac；headless 机器上 claude / codex 的 device-code 登录与 Linux 真机端到端验证见 DESIGN.md §14。

## 控制台与网关的来源限制

控制台只接受来自自身 origin 的写请求（`Sec-Fetch-Site` / `Origin` 校验）并校验 `Host`，防 CSRF 与 DNS rebinding。把控制台发布到公网域名时必须加 `-console-hosts <域名>`（逗号分隔多个），否则该域名下的所有写操作会被 403。网关 `0.0.0.0:18930` 默认只答应 loopback 和 `-gateway-url` 所在的 /24（容器网段）；别的来源加 `-gateway-allow <CIDR,...>`，`-gateway-allow any` 关闭限制。

## VM 丢失的空间

`container delete` 在 daemon 之外删掉了 VM、或 runtime 重置后，控制台把该空间显示为 **VM missing**：`workspace/` 还在数据目录里，Wake 会报错，Delete 会连 workspace 一起清掉。想保留文件就先把 `spaces/<name>/workspace` 拷走再 Delete，然后重建同名空间把文件放回。网络升级中途失败（旧 VM 已删、新 VM 没起来）的空间记录了恢复镜像，直接 Wake 会从快照重建。gateway-only 空间启动时防火墙应用失败会停机并记录 `wake_blocked`，连接不再反复唤醒它，Host 点 Wake 清除。

## 回滚

回滚时先暂停受影响空间，再换回旧二进制及对应 launchd 参数（重新执行旧版 `cospaced setup -- <参数>`）。旧版 daemon 不识别账号路由和网络开关，不能把这些字段当作仍生效；尤其不可把已经关闭联网的空间直接交给不理解该策略的旧网关。升级快照可用目录内 Dockerfile 重建恢复镜像，再以原 workspace 挂载、原资源尺寸及 CAP_NET_ADMIN 重建 VM。

## 端到端冒烟

```bash
./scripts/smoke.sh                 # 默认用 18930；生产 daemon 在跑时用 PORT=28930 ./scripts/smoke.sh
```

## 发布

所有分发都在 GitHub，项目不运营下载服务器：

| 产物 | 位置 | 真源 / 工具 |
|---|---|---|
| 安装脚本 `install.sh` / `install.ps1` / `host.sh` | GitHub Pages（main 分支 `docs/`） | `docs/` |
| `cospace`（全平台）、`cospaced`（darwin/arm64）、`cospace-relay`（linux）、`VERSION` | GitHub Releases | `scripts/release.sh`（goreleaser） |
| guest 的 Homebrew cask | `JingxuanKang/homebrew-tap` | goreleaser 生成 |
| 空间镜像 | `ghcr.io/jingxuankang/cospace-base:<tag>`（linux/arm64 + linux/amd64） | `.github/workflows/space-image.yml`，由 `scripts/publish-image.sh` 触发 |

安装命令与下载地址的单一源是 `internal/dist`；改了 Pages 或仓库位置先改它。

发版：提仓库根 `VERSION`（经 ldflags 注入二进制，并作为 `releases/latest/download/VERSION` 发布，安装器据此判断"已最新"；不提就发版会让已装的人永远拿不到新版），commit 后运行：

```bash
scripts/release.sh --dry-run    # 本地构建全部产物到 dist/，不发布
scripts/release.sh              # 打 tag v$VERSION、推 tag、goreleaser 发布到 GitHub
```

镜像有改动：先提 `internal/dist` 的 `BaseImage` tag（已发布的 daemon 继续拉它验证过的那一版），commit 并 push 到 master，再：

```bash
scripts/publish-image.sh            # 触发 GitHub Actions 构建 arm64+amd64 并推 ghcr，等待完成
scripts/publish-image.sh --build    # 只在本机构建 arm64 版本地测试
```

workflow 用自身的 `GITHUB_TOKEN` 推送，不需要个人 token 的 write:packages。已存在的 tag 默认拒绝覆盖（`--force` 才覆盖）。

已知限制：
- `cospace` / `cospaced` 未签名/未公证：安装器与 cask 清除 quarantine 标记绕过 Gatekeeper；正式对外前应改为 Apple Developer ID 签名 + 公证。
- 大陆网络访问 GitHub / ghcr.io 可能很慢，暂无镜像加速。
- Windows 安装器尚未做真机端到端验证。
