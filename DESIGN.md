# CoSpace 设计文档（v1 单一真源）

> **CoSpace：把你 Mac 上的算力和 AI 订阅，变成别人一条 ssh 命令就能进来的共享 AI 开发空间。**

术语：**host**（管理者，Mac 拥有者）/ **guest**（成员，受邀者）/ **space**（空间，隔离的 Linux 开发环境）。品牌名是 CoSpace；产品上下一律用 space：UI、CLI、API 路径 `/api/spaces`、磁盘 `space.json`、容器内 Linux 用户 `space`、Go 标识符。改名前的旧数据（rooms/、room.json、usage.jsonl 旧字段）由 daemon 启动时自动迁移；改名前建的容器内仍是旧 Linux 用户 `room`，`syncRuntime` 首次同步时就地改名为 `space`（`usermod -l`，同 uid，`/home/room` 符号链接保留）。

对外宣传优先级（README/邀请页照此排布）：① 多人共享 agent 状态的协作（同 home、session/memory 互见、`--resume` 接管）；② 60 秒零注册即用；③ 安全狂跑 agent（full-auto + VM 边界 + 预算闸）。"凭证不出 Mac"作为支撑性信任文案，不做标题；对外不使用"共享/出借订阅给陌生人"类表述，一律说"邀请你信任的人"。

本文档是产品与架构的单一真源。历史讨论与变更叙事不在此记录（归 git）。

---

## 1. 两个核心痛点（产品原点）

1. **让别人以最简单的方式用上 Claude Code / Codex**：不注册、不登录、打开即用。凭证由 host 预先解决，guest 进来敲 `claude` 就能干活。
2. **多人合作时 agent 上下文割裂**：session、memory、文件散在各自机器上。解法是大家进同一个环境——Claude Code / Codex 的 session、memory、resume 列表全是本地文件，同一个 home 意味着 `claude --resume` 互见、memory 共享、文件共享，**协作原语是共享的 agent 状态目录，不是任何产品功能**。

## 2. 两个核心场景（并列，同一机制）

- **共建空间**：多人进同一空间，同一代码库、同一堆 agent session 一起干活。
- **借人空间**：host 把预认证好的 agent 环境借给朋友/学生，各干各的。

区别只在空间里放什么、请谁进来。前提是**开空间足够便宜快捷**。

## 3. 用户体验

### Guest（一次安装、零注册）

1. 用一条 shell 或 PowerShell 命令安装单文件 `cospace`，再运行 host 发来的一次性配对命令完成 SSH 公钥登记；也可打开邀请网页看图文指引（含无 ssh key 的分支指导）。
2. 之后 `ssh <空间名>` / Cursor Remote SSH 直连空间；进去 `claude`/`codex`/`grok` 已登录，打开即用。
3. Web 项目**自动端口转发**：`cospace pair` 写的 ssh config 预置了常见 dev 端口（3000/5173/8000/8080/8888）的 `LocalForward`（`ExitOnForwardFailure no`，本地占用则跳过不断连），空间里起的 dev server 在 guest 本地同端口直接点开；其它端口 `ssh -L <port>:localhost:<port> <空间名>`，Cursor Remote SSH 则自动转发任意端口。不同 guest 的转发绑在各自本机、互不冲突；同机多开会话经 `ControlMaster auto` 复用首条连接（pair 写入，Windows OpenSSH 不支持故不写），既无端口告警也更快。真机验证：`ssh <空间名>` 经 cospace/tailcat 隧道 + 自动 `LocalForward` → guest `curl localhost:8000` 取到空间内服务。端口列表在 `cmd/cospace` 与控制台 Access 卡片两处，改动需同步。

### Host

1. 一行 `curl -fsSL https://jingxuankang.github.io/cospace/host.sh | sh` 装好：自动装 Apple container、下载预编译的无界面引擎 `cospaced`（单二进制，不需要 Go）、注册为登录自启服务并打开 localhost 控制台。空间镜像首次在后台下载（一次性，几百 MB），控制台显示进度，下完之后每个新空间几秒开出。建空间、邀请、停启、撤销、看额度和资源账，全在网页上；CLI 只是薄壳。
2. 凭证是 host 自己本机登录的 claude/codex（订阅或 API key），产品只在网关内读用、绝不复制进空间、不上传。控制台首页的 **AI for all spaces** 可统一暂停三家赞助；开启后每个空间再单独选择 provider，实际可用条件是两层同时开启。
3. **空间模板**：把一个空间的创建参数（内存/CPU、AI 选装、预算与并发上限、full-auto）存成命名模板，一键按模板开新空间——训练营一人一个同款空间、一实验一个标准空间。控制台空间详情页 "Save as Template"，新建空间弹窗选模板；CLI `cospaced template list/save/delete` 与 `space create -template`。模板只含创建参数，不含成员与状态，存于数据目录 `templates.json`。

## 4. 架构

```text
guest 的标准 ssh / Cursor Remote SSH
        ↓
公网入口（传输抽象，二选一）
  ├─ tailcat 模式（默认快速通道）：Tailscale 开源库（BSD-3）嵌入 daemon，免账号、双方零服务器；
  │    能打洞即 WireGuard P2P 直连，失败走 Tailscale 公共 DERP 兜底（限速、无 SLA，故不可为唯一路线）。
  │    guest 首次粘贴一条命令：从 GitHub（Pages 上的安装脚本 + Releases 产物）下载 `cospace` 小工具、走配对、自动写 ~/.ssh/config 的
  │    ProxyCommand Host 条目，并把空间 host key 固定到专用 `~/.ssh/cospace_known_hosts`；此后
  │    `ssh <空间名>`，Cursor 直接可选。大陆 guest 拉 GitHub 可能受阻。
  └─ BYO VPS 哑中转（稳定通道，紧随实现）：host 自备任意廉价 VPS，产品一键部署；只转发加密 TCP，
       看不到内容。guest 真正零安装（纯系统 ssh，全平台），顺带托管邀请网页。有 VPS 的 host 的升级路线。
        ↑ Mac 主动出站隧道（yamux 多路复用）
Mac 上的 daemon（单二进制，含 go:embed 控制台前端）
  ├─ 容器生命周期 reconcile（Apple container 无自愈能力，daemon 扶）
  ├─ 凭证代理（真 token 只在此进程；换头转发 + 计量）
  ├─ 空闲休眠 / ssh 到来自动唤醒 / Host 人工休眠锁
  ├─ 公钥同步、caffeinate、资源记账
  └─ 本地 Web 控制台
        ↓
Apple container：每空间一个 Linux 轻量 VM + 持久 volume
  （OpenSSH + git + tmux + Node + Python + claude + codex）
```

**产品方不运营任何服务。** VPS 是 host 自己的（最低配即可，内存占用几十 MB），邀请页由 VPS 上的中转顺带托管。

### daemon 存在的理由（四件长驻活）

1. Apple container 无 restart policy，重启/睡醒后网络会挂，需要 reconcile 自愈；
2. 凭证代理需常驻在请求路径上；
3. 空闲空间休眠、来连接时唤醒，需要守门进程；
4. 到 VPS 的出站隧道需要保活重连；
5. 凭证保活：provider access token 只活数小时且只有 vendor CLI 运行时才续期，daemon 要在到期前替 host "用一下"（见 §14 凭证保活）。

### 凭证代理（已实现 ✅）

- 真 token（订阅 OAuth 或 API key）永远只在 Mac 上的网关进程；空间内注入**每空间独立的假 token** + `ANTHROPIC_BASE_URL`/`OPENAI_BASE_URL` 指向网关；网关验证假 token 后换真 token 转发。
- 收益：凭证翻遍空间也拿不走；**撤销 = 网关拒绝该空间**，即刻生效；请求路径上精确计量额度（SSE token 解析）。
- 对上游无区别：同账号、同出口 IP（空间流量本来就从 Mac 出去），等同 host 本机多开并行 session。
- **凭证来源 = 直读 host CLI 自己维护的凭证（`internal/gateway/creds_file.go` / `creds_claude.go`）**：codex 读 `~/.codex/auth.json` 的 `tokens.access_token`，grok 读 `~/.grok/auth.json` 中 `exp` 最晚的那条，按 mtime 缓存、变更即重读。claude 在 macOS 上有两份独立副本：Keychain 项 "Claude Code-credentials" 与 `~/.claude/.credentials.json`，Claude Code 用哪份就续哪份，两份过期时间各不相同；网关两份都读、取过期最晚的一份（Keychain 读取缓存 30 秒），keepalive 用同一个来源判断是否续期成功。早期的 learn 模式（偷看一次流量拍快照）已废弃，因为快照会随本地续期而失效。
- **两层 AI 开关（`sponsor.json` + `/api/sponsor` + `Space.Providers`）**：Host 层**按 provider 独立**——首页每张 provider 卡一个开关，可单独把 Claude/Codex/Grok 对所有空间统一暂停（例如只关 claude）；UI 无 master 总开关（`/api/sponsor` 不带 provider 时仍是三家一起设置）。空间详情页再独立开关本空间的三项，允许全关。有效条件是 `host_provider_on && space_provider_on`。空间层除同步移除 env/config 外，网关还按 space/provider 强制校验，手工拿空间假 token 请求已关闭 provider 会得到 403；“所有空间的 AI”关闭则得到 503。
- **本机端到端实测（2026-08-29）**：赞助开关默认 ON、建空间后空间内 `claude -p` 直接成功（无需任何 learn 步骤），用的是本地实时 token。codex（ChatGPT 登录）走 `openai_base_url` + Bearer JWT 换头，M0 已验证上游 200。

### 空间模型与联网策略

- Host 在空间详情页选择 Codex 通道（官方、Sub2API I、Sub2API II）和默认模型（GPT-6 Astra、GPT-5.6 Sol/Terra/Luna）。通道由网关按空间强制选取；模型写入空间内托管配置，新 Codex 会话使用该默认值。已有空间缺省保持官方通道及原默认模型。
- Sub2API 通道为 Host 可选配置，真实 key 从 macOS Keychain 读取，仅在网关进程使用；空间继续只持有自己的可撤销代理 token。Host 的 OpenAI 总开关和空间 Codex 开关仍然优先。
- Internet access 开关保存为 `open` / `gateway-only`。关闭时由空间内 root 管理的 nftables 同时限制 IPv4/IPv6 出站，只保留 loopback、AI 网关及入站 SSH/预览连接的回复；普通 space 用户不能修改规则。关闭不影响模型 API 调用。
- 关闭出网还会在网关拒绝云端搜索、URL 图片和远端 MCP 等外部检索请求；单纯关闭 CLI 的 web search 不作为执行边界。开启允许正常下载、浏览和包安装。
- 配置随空间持久化，模板包含这些选择。新空间镜像在启动 SSH 前加载已保存的 nft 规则；CoSpace 管理的唤醒再次校准。Internet 开关要求空间处于运行状态，避免只修改休眠空间的元数据却留下开机出网窗口；账号、模型、providers 和 full-auto 可在休眠时保存，下次唤醒生效。
- 配置变更先应用运行环境，再保存状态；失败尝试恢复旧配置，恢复失败则停机，前端显示错误。只切换 Codex 通道时直接更新网关策略，无需重写空间配置。大配置通过标准输入注入，避免 shell 单参数长度限制。
- 新空间初始化失败时停机，保留状态供修复；API 不把尚未完成的环境报成可用。
- 新空间给 VM 内 root 增加 `CAP_NET_ADMIN`，普通 space 用户保持无 capability。旧空间前端提供一次性升级：先停机、导出完整 rootfs、构建恢复镜像，再用原 workspace 重新创建；保留 SSH host key 和已装工具。快照在 Host 数据目录 `backups/network/`，不发布。升级异步运行，前端可刷新查看状态；同空间其他写操作被拒绝。Host 管理员仍拥有容器和网络控制权。

### 隔离与注入纪律

- 空间看不到 Mac 主目录、看不到其他空间（VM 级隔离）。
- **只注入凭证相关文件（或代理假 token），绝不复制整个 `~/.claude`、`~/.codex`**——memory、skill、session 历史全是本地文件，不注入即天然隔离。
- 空间内部：所有 guest 共享同一份 memory / session / 文件（特性，非缺陷）。

### 成员身份

所有 guest 共用一个 Linux 用户（避免文件权限问题，符合"同一份文件"语义）。身份靠 authorized_keys 的 per-key 选项 `environment="SPACE_MEMBER=<name>"`（设计项，尚未实现）：登录 shell 据此自动设置 `GIT_AUTHOR_NAME/EMAIL`（保证 commit 归属），控制台据此显示"谁最后连过"。

### git push 身份

每空间自生成 ssh key，host 在控制台引导下加为仓库 deploy key；不挂任何人的个人 git 凭证。guest 想用自己身份可自行 agent forwarding（默认关闭）。

## 5. 配对与邀请

- 控制台生成一次性配对码（嵌入可粘贴命令）：一次性、绑定空间、限时、服务端只存 hash、成功即销毁；`/pair` 与邀请页按来源限速（每空间 / 每客户端每分钟），错误猜测不改变码本身的状态。配对顺序是**先校验再消费**：码有效 → 成员名与公钥通过 `spaces.CanAddMember`（名字 `^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`，公钥经 `ssh.ParseAuthorizedKey` 规范化为单行、拒绝任何 options）→ 取 host key → 才 `Redeem` → `AddMember`。名字撞车、key 坏掉不会烧码，并以 409/400 带原因返回 guest（提示 `-name`）；码本身错误一律 403 不泄露。
- 配对入口是同一条 tailcat 地址的受限 HTTP `/pair` 路由（仅端口 80、仅接受一次性码）；码的验证与 guest 公钥落库全部发生在 Mac 侧 daemon，不经过半可信的空间容器。请求带 `protocol`（wire 格式版本，当前 1；缺省视为 1，因为字段引入前的格式与 1 相同）和 `version`（构建版本）；daemon 在查码**之前**按 `[minPairProtocol, maxPairProtocol]` 校验，越界返回 426 与两端边界，不消耗码也不泄露码的有效性。`cospace` 据此区分"自己太旧 → 重跑安装命令"与"host daemon 太旧 → 请 host 升级"。改 wire 格式时提 `maxPairProtocol`，旧格式无法再服务时才提 `minPairProtocol`。
- 配对成功响应返回空间 ED25519 host public key 与 fingerprint；`cospace` 本地复算 fingerprint 后，把 key 写进自己的 managed known_hosts，并让该 Host 条目启用 `StrictHostKeyChecking yes`。删除后重建同名空间再配对会只轮换这个受管条目，不碰用户的普通 `known_hosts`。
- 撤销成员 = 移除其公钥，即刻无法建立新连接。
- **邀请链接页 `GET /i/<code>`**（模板 `internal/api/invite.html` 单独 embed，不走静态文件服务器）：把同一条一次性码渲染成图文邀请页——三步命令（macOS/Linux ↔ Windows 切换、逐条复制/Copy all）+ Claude Code / Codex 桌面端连接图示（内嵌 SVG，空间名直接渲染进图）。查询用只读 peek（`pairing.Lookup`）：不消耗码、不计尝试次数；无效/过期/超次/已用一律渲染同一张失效页（404），不可区分。码 40-bit 熵 + 分钟级 TTL，URL 的保密级别与聊天里发码相同。控制台邀请弹窗在非 localhost origin 时附带该链接（localhost 下 guest 反正打不开，隐藏）；聊天粘贴三步仍是主流程，链接页只对把控制台发上公网的 host 有意义（个人部署见 Deploy.md 的 CF Access bypass）。

## 6. 额度与资源

- **额度 = 等价美元计量 + 软管控（空间粒度）**：网关路径实时计量（`internal/usage`，JSONL 持久化 + SSE 里解析 input/output_tokens **和 model**），控制台按空间/按日展示 token、请求数与**累计花费 $**。计费把 token 数乘一张按模型族的 list-price 价表（`internal/gateway/pricing.go`，opus/sonnet/haiku/gpt-5/grok，未知模型走 provider 默认再兜底）折算成等价美元——host 付的是订阅月费不是按量，这个美元数是**通用标尺不是真账单**。
  - **两道软闸**（`internal/gateway/quota.go`，`SpacePolicy` 由 `spaces.Manager` 实现）：**累计花费上限**（每空间 `usd_limit`，0=不限）与**并发上限**（`max_concurrency`，默认 `DefaultMaxConcurrency=5`）。请求进来先抢并发槽、再比累计花费，任一超限返回 **429**（并发满 / 花费到顶）。
  - **累计花费覆盖当前空间这一代的完整生命周期**：`spent` 每完成一个请求累加，重启时从历史 JSONL 按当前空间 `created_at` 重新播种，limit 跨重启仍成立；删除空间后重建同名空间不会继承旧一代的图表或额度。花费在流式响应结束才知道，所以是**预检对已累计值**——正在跑的那一个可越顶，下一个才被拒（至多一个请求的超调）。
  - 控制台每空间 Budget 卡展示"已用 $ / 上限"进度条 + 并发上限，`Edit limits…` 弹窗改 `usd_limit` 与 `max_concurrency`（`PUT /api/spaces/{name}/limits`）。硬预算（预扣、按成员计量）留给将来"API key"的企业模式（架构已留口）。
- **内存**：每空间 VM 内存实占，默认 2G 可调；**空闲自动休眠**（idle reaper 走 `StopIdle`，无活跃连接超 `-idle-timeout` 后停机，内存归零文件全留）+ **连接自动唤醒**（仅对 idle-stopped 空间，DialSSH 发现未运行即 `StartOnConnect` 并轮询 IP）。Host 在控制台点击 **Sleep** 或 CLI 执行 `space stop` 则持久化 `manual_sleep`，现有 SSH 断开且重连不能唤醒，必须由 Host **Wake** / `space start` 清除锁。控制台明确区分 `asleep · wakes on connect` 与 `sleeping · Host wake required`。
- **磁盘**：每空间上限（默认 64G 可调），删空间即回收。
- **网络**：SSH/Cursor 流量 KB/s 级，家庭上行足够。
- **宿主现实**：Mac 须开机在线。`caffeinate`（`internal/power`，有空间运行时自动按住）防**闲置**睡眠。合盖是否睡由 macOS 决定：**插电 + 接了外接显示器 = 合盖模式(clamshell)，合盖不睡**（理想宿主形态）；用电池 或 无外接屏时合盖会睡。控制台检测电池供电即警告。最佳宿主：Mac mini / Studio / 合盖模式的笔记本。

## 7. 信任边界（明说，不粉饰）

- host 可见空间内一切（文件、session、用量）——空间是协作空间，不是对 host 保密的私人 VM。
- 邀请 = 交出订阅使用权：guest 在空间内可任意使用 host 的额度；订阅 rate limit 全账号共享，多人猛跑会互相挤。只请信任的人。
- 出口流量走 host 的 IP，滥用责任在 host（可按空间关闭出网，见 §4 的空间模型与联网策略）。
- VPS 中转只见连接元数据与流量大小，看不到 SSH 内容。
- guest 不可见：Mac 主目录、真凭证、其他空间、host 个人 git/ssh 凭据。

## 8. 镜像

单一基础镜像（`image/Dockerfile`，Debian bookworm-slim）。哲学：**够 agent 起步就行，进去之后 agent 自己会装。** 内置：
- **AI CLI**：claude / codex / grok（三家走 host 订阅经网关，见 §13）
- **VCS / 网络**：git、openssh-client+server（git over ssh / deploy key 必需，不只是 server）、gh（GitHub CLI）、curl、wget、dnsutils(dig)、netcat
- **构建 / 运行时**：build-essential(gcc/make，npm node-gyp & pip C 扩展必需)、Node 22（Claude Code 依赖）、Python3+venv、uv（现代 python 装包，装在 /usr/local/bin 供空间用户）
- **日常**：tmux、vim、nano、less、ripgrep(rg)、fd、tree、jq、unzip/zip、htop、procps(ps)
- **不装**：docker（Apple container 内跑不了）、Go/Rust 等重型运行时（按需，默认不带）

建空间可选项：clone 一个 repo；注入代理设置（国内 Mac 场景，容器出网走 Mac，需把代理配进空间）。

已知限制：空间内不能再跑 Docker（VM 内嵌套，v1 不支持）。

**分发**：镜像预构建后推到 `ghcr.io/jingxuankang/cospace-base:<tag>`（`scripts/publish-image.sh`），host 不在本机构建。daemon 固定使用 `internal/dist` 的 `BaseImage` tag：启动时若本地没有就在后台 `container image pull --platform linux/arm64`，`/api/host` 的 `image` 字段带进度，控制台显示横幅并在就绪前禁用 New Space（API 返回 409）；CLI `space create` 同步拉取。Dockerfile 一改就提 tag，已发布的 daemon 继续拉它验证过的那一版。npm 下载缓存在同一层清掉，避免每个 host 白下约 230 MB。开发时可用 `-image cospace-base` 指向本地构建。

## 9. 技术栈

- **Go**：daemon、relay、空间内 pair 小件。单静态二进制 brew 分发；relay 交叉编译 Linux；凭证代理基于标准库 `httputil.ReverseProxy`；隧道多路复用 hashicorp/yamux；容器与系统操作 shell out（`container`、`caffeinate`）。
- **TypeScript**：仅 Web 控制台前端，构建产物 `go:embed` 进 daemon 二进制——**一个文件就是整个产品**。
- **Runtime = Apple container，唯一后端**：官方、免费、可随产品分发、VM 级隔离。OrbStack（第三方付费 license）不可作为产品地基。要求 Apple Silicon + macOS 26+。

## 10. 明确不做（v1）

- session 管理命令面（tmux wrapper / sessions / watch / take）——同 home 下原生 tmux 已支持共享终端，协作方式由用户自定
- 产品方托管的 relay 与任何在线服务
- 硬预算管控（留给 API-key 模式）
- Web IDE / 浏览器文件管理器 / 公网开发预览 URL
- 多 Mac 调度、快照迁移、企业 SSO/RBAC、商业计费与公开注册
- Tailscale 之外的 VPN 集成、OrbStack/Docker 后端

## 11. 里程碑

- **M0 spikes（2026-08-29 全部完成 ✅）**：
  - claude 订阅走代理 ✅；codex ChatGPT 登录走代理 ✅（`openai_base_url` 覆盖 + Bearer JWT 换头，上游 200）
  - Apple container ✅（brew 装 1.3.1 + kata 内核；debian+sshd 镜像 build、限额运行、Mac 直连容器 IP ssh、volume 停启后持久）
  - tailcat ✅（`tailcat --serve=2222` 出 token，guest 侧 `ssh -o ProxyCommand="tailcat <token> 2222"` 穿隧道进空间）
  - **端到端 ✅**：cospace-base 镜像（debian + Node 22 + claude + codex + git/tmux/python/rg/jq + bubblewrap，见 `image/`）内 `claude -p` 带每空间假 token → Mac 网关换真 token → 正常应答；真凭证未进空间。Codex 使用镜像内系统 `bwrap`，不依赖 bundled fallback。
- **M1 单机版（2026-08-29 完成 ✅）**：Go 实现 `cospaced`（`cmd/cospaced` + `internal/{container,spaces,gateway}`），TDD：四包单测全绿 + `scripts/smoke.sh` 真机冒烟全过（建空间 → 加成员 → 客侧 ssh → 空间内 claude 经网关换头 200 → 撤销即失效 → 停启后文件保留 IP 变化正确处理 → 删除即回收）。凭证网关含按空间用量记录钩子。镜像不再烘任何凭证/公钥，全部由 daemon 运行时注入
- **M2 连接（2026-08-29 完成 ✅）**：tailcat 嵌入 daemon（`internal/transport`，每空间稳定地址 + 端口 22/80 路由 + 活动跟踪 + 连接唤醒）；一次性配对码（`internal/pairing`，磁盘只存 hash、常量时间比较、单次使用）；guest 工具 `cmd/cospace`（connect ProxyCommand + pair 自动写 ssh config，并把空间 host key 固定到专用 managed known_hosts）；`cospaced invite` 子命令。BYO VPS 哑中转也已实现（`internal/relay` yamux 多路复用 + `cmd/cospace-relay` + `deploy/relay-install.sh`，端口跨重连稳定）。**真机 dogfood 全过**（`scripts/dogfood-tailcat.sh`）：建空间→发码→cospace 配对（单次码验证）→guest 经真实 DERP 隧道 ssh 进空间→空间内 claude 经网关换头。
- **M3 Web 控制台（2026-08-29 完成 ✅）**：`internal/api` REST + `internal/api/web` 单文件 UI（Daylight Gold 落地，go:embed 进 daemon），空间管理 / 邀请（终端窗 + 一次性码）/ 额度显示（`internal/usage` JSONL 持久化 + SSE token 解析）/ 资源账 / 电源警告 / **两层 AI 开关**（首页 **AI for all spaces** + 每空间 provider）；console 双栈 loopback 监听。所有端点真机 curl 验证通过。
- **M4 打磨（部分完成）**：空闲休眠 + 连接唤醒 + Host 人工休眠锁 ✅（`StopIdle` / `StartOnConnect` 与持久化 `manual_sleep` 分流）、caffeinate 电源守护 ✅（`internal/power`）、launchd plist + Deploy.md ✅、**额度软管控 ✅**（等价美元计量 + 每空间累计花费/并发上限，见 §6；`internal/gateway/{pricing,quota}.go` + 控制台 Budget 卡 + `PUT /limits`，含单测）。待办：重启/睡醒后容器网络自愈、brew formula。
- **M6 全面 space 化（2026-08-31 完成 ✅）**：对外与机器层全部 room→space（UI/CLI/API `/api/spaces`/磁盘 `spaces/`+`space.json`/usage 字段/Go 标识符/镜像内 Linux 用户 `space`）。daemon 启动自动迁移旧数据；改名前建的容器由 `syncRuntime` 首次同步时就地把 Linux 用户 `room` 改名为 `space`（同 uid、home 迁移、`/home/room` 符号链接），老 guest 的 `User room` 配置因 sshd `AllowUsers space room` 继续可登录。guest 工具的 pair 响应字段同步改名（破坏性，旧 `cospace`/`gr` 二进制不能再配对，需重装；已重新发版）。真机：一个在用空间迁移后新配对 `space@<空间名>` 通过；新镜像空间端到端 `FRESH-SPACE-OK`。
- **M5 CoSpace 改名 + full-auto + 模板 + 凭证保活（2026-08-31 完成 ✅）**：品牌 Guestroom → CoSpace（二进制 `cospaced`/`cospace`+`co` 别名、模块路径、launchd label、数据目录带旧目录自动迁移、known_hosts 与 ssh config 受管块标记、新空间 token 前缀 `cs_`，旧空间 token 不变）；full-auto 预设与空间模板（见 §3/§13）；凭证保活（见 §14），新增单测全绿。

## 12. 控制台视觉方向（已定稿）

「Daylight Gold」：暖白渐变底 + 金色光球 + 淡金 64px 网格的分层背景，白色玻璃卡片 + 彩色投影，金色渐变按钮与钥匙 logo，深色终端元素作对比焦点（邀请弹窗的三步终端窗、Access 卡的单行 ssh 命令条），破坏性操作（删空间）走居中确认弹窗 + 红色确认按钮，赞助区使用带官方 provider 标志的独立卡片，交错入场动画。可点原型与画布源在 `design/`（`Main.dc.html` 为唯一真源），体系参考 Sub2API 前端。

## 13. 空间内的 AI 工具（per-space）

- **每空间选装**（`Space.Providers`，控制台 "AI in this space" 卡片可开关，CLI/API `SetProviders`）：`anthropic`(Claude Code)/`openai`(Codex)/`xai`(Grok)独立开关，允许三项全关；只有改名前的旧 space.json 完全缺少 `providers` 字段时才兼容解释为全开。A 空间可只 Codex、B 空间可 Codex+Claude Code+Grok。关闭 provider 会移除对应 env/config，并由网关按 space/provider 返回 403，不能用空间 token 绕过。三家全部用 host 订阅、经网关换头；首页 **AI for all spaces** 默认开，关闭时三家统一暂停。
- **Full-auto 预设（`Space.FullAuto`，新空间默认开）**：空间的安全边界是环境级（VM 隔离 + 空间内零真凭证 + 美元/并发闸），不是动作级审批，所以 agent 出厂即全速：syncRuntime 托管写入 claude `~/.claude/settings.json` 的 `defaultMode: bypassPermissions` 与 codex `config.toml` 的 `approval_policy="never"` + `sandbox_mode="danger-full-access"`（后两者是 TOML 顶层键，必须在 `[model_providers.*]` 表头之前）。关闭 full-auto 即移除自动审批字段，回到工具默认审批流；Codex 配置由 sync 整体重写，Claude 仅合并托管的 permissions 字段并保留用户其他配置。grok CLI 暂无已知等价配置，不含在内。改名前的旧空间 `full_auto` 缺省为关。控制台 "AI in this space" 卡片有开关，API `PUT /api/spaces/{name}/full_auto`，CLI `space create -full-auto`。
- **claude**：网关注入假 token + `ANTHROPIC_BASE_URL`,边缘换真订阅 token。空间内显示 API 登录、实际计订阅额度,正常。托管 `settings.json` 常驻注入：statusline（空间名行 + `claude-hud`，镜像已预装；旧镜像回退 jq 单行"模型 · 目录"）与可选默认模型（`-claude-model`，默认不设、跟随 claude 自身默认）。注意 API-token 认证模式下 `/model` 选择器不列订阅档位模型（如 fable），但网关照常放行——显式指定或默认配置即可用（真机 `FABLE-VIA-GATEWAY-OK`）。空间内 `COSPACE_SPACE` 环境变量恒有，脚本可用。sync 时还向 `~/.claude.json` **合并**（不覆盖）预置：`hasCompletedOnboarding`、`bypassPermissionsModeAccepted` 与 `/home/space`、`/workspace` 的 `hasTrustDialogAccepted`——空间本身就是边界，逐目录信任弹窗是纯摩擦。**statusline 架构（真机踩坑后定稿）**：渲染器 = npm 包 `claude-hud` 的**安装器**落到 `~/.claude/statusline-command.sh` 的脚本（镜像 build 时以 space 用户预跑生成，settings.json 由安装器写的那份要删掉让 sync 播种带前缀版）；托管命令 = `[空间名]` 前缀经 awk 拼进渲染器第一行（claude 只渲染两三行 statusline，独立前缀行会被挤掉）。三个死坑：`claude-hud` 命令本身是安装器、每次执行都重装并改写配置，绝不能放进 statusline 命令；statusline 命令执行失败时 claude 会**自愈**重写 settings.json（生成默认脚本），所以命令必须先裸测通过；ccusage 的 native 二进制被 npm 装完没有执行位，需 `chmod -R a+rx`（否则 💰/⏱ 永远是 —）。
- **codex**：ChatGPT 订阅 token 只被 `https://chatgpt.com/backend-api/codex` 认，裸转 api.openai.com 被拒(缺 scope)。网关 openai 上游**默认**即该后端，bearer 换头直连即可(真机验证:真 token→400 参数错、假 token→401；空间内 `codex exec` 端到端通过)，**零外部依赖**。端点路径形状由上游 URL 自带(chatgpt 后端无 `/v1`；API-key 赞助传 `-openai-upstream https://api.openai.com/v1`)，空间侧 base_url 一律 `<gateway>/openai`。syncRuntime 自动为空间写 `~/.codex/config.toml`(model_provider 指网关、`supports_websockets=false`、model 用 `-codex-model` 默认 `gpt-5.6-sol`，并镜像 host 的显示/行为偏好：`model_reasoning_effort/plan_mode_reasoning_effort=xhigh`、`model_reasoning_summary=auto`、`personality=pragmatic`、`review_model=同主模型`、`project_doc_fallback_filenames=[AGENTS.md, CLAUDE.md]`；另写 `model_catalog_json`（go:embed 的官方目录快照，sol/terra/luna 三模型均真机验证过网关）——codex 对自定义 provider 不枚举模型，没有目录则 /model 选择器为空；以及 `/home/space` 与 `/workspace` 的 trust_level=trusted，防止每次 sync 整写 config 把 codex 自己追加的信任条目抹掉后反复弹信任框)+ 设 `OPENAI_API_KEY`=假 token。
- **grok**：**已打通、走 host 订阅**(真机 `grok -p` 端到端 `GROK-SPACE-OK`)。xAI Grok CLI 预装镜像(`grok 1.0.13`;安装器的 symlink 指向 `/root/.grok` 空间用户读不了,Dockerfile 已改成拷真身+重建 symlink)。网关新增 `xai` provider,上游默认 `https://api.x.ai/v1`(订阅 OIDC JWT 直接被接受:真 token→200、假→401)。凭证读 `~/.grok/auth.json`(顶层键是动态 `issuer::uuid`,取其 `.key`)。空间侧:`GROK_MODELS_BASE_URL=<gateway>/xai` + `XAI_API_KEY`=假 token 切到 BYOK 模式,但**还须** `~/.grok/config.toml` 写 `[auth] preferred_method="api_key"`,否则仍强制 `grok login`(这是关键坑,syncRuntime 自动写)。
- **cursor**：**仍不纳入当前三家 Host 赞助**（2026-08-30 用 Cursor Agent `2026.08.11-e8db854` 复核）。guest 使用自己的 Cursor Remote SSH 一直可用；不能把它误写成 Host Cursor 订阅已接入。
  - 当前 CLI 已正式支持 `--endpoint` / `CURSOR_API_ENDPOINT`，因此“完全没有自定义端点”不再是障碍；但现有 Host 浏览器登录 token 仍不能当 User API Key 使用。实测假 `CURSOR_API_KEY` 经网关换成 Host 登录 token 后，`GetMe` 可 200，但 `/auth/exchange_user_api_key` 仍 401，聊天不会开始。
  - 官方另有 Dashboard 生成的 User API Key，可能形成新的独立 spike；当前 Host 没有这类 key，而且 exchange 响应是否会把可复用真凭证发回客户端仍需证明。未通过“空间只持假 token”的端到端验证前，不算可接入。
  - 换头方案(claude/codex/grok 那套:空间发假 token、网关换真 token)对 cursor **不成立**。真机验证过:所有控制面请求(DashboardService/AiService 十余个)都能经网关换头成功(200),但 cursor-agent 会拿 `GetMe` 返回的真实身份和它本地持有的 token 做校验,发实际聊天**前**就报 "Authentication error"。把本地 token 伪造成"真 payload(真 userId)+ 假签名"的 JWT 也不行(仍卡在同一处;签名是 HS256 对称,客户端本可不验签,但实测仍拒——推测真实 access token 还被放进了某个请求 body 由服务端校验)。
  - 唯一能跑通的是**真 cursor 凭证直接进空间**(`CURSOR_AUTH_TOKEN`=真 token 直连官方,真机 `CURSOR-DIRECT-OK`/`CURSOR-PROXY-OK` 成功)。但真凭证落进空间文件/env,ssh 进空间的 guest 能读走、拿去别处登录 host 的 cursor 账号,破坏"凭证不出 Mac"红线。
  - 结论:cursor 只适合"真凭证透传给完全信任的人"的极窄场景,不符合产品的通用赞助模型,不做。guest 仍可用**自己的** Cursor 经 Remote SSH 连空间(那是 guest 自己的凭证,一直可用,与此无关)。
  - 若将来非要做,方向是把网关认空间从 token-based 改成 IP-based(每空间容器 IP 唯一),让 token 位置能放真 userId 过身份校验——但那是动网关核心的一笔大活,且仍需先解决"真 token 是否被放进 body"这个未定项。

## 14. 待验证 / 开放问题 / Roadmap

已决定要做、尚未实现：

- **网络白名单与日志（待做）**：已有 `Open` / `Gateway-only` 开关；自定义域名白名单和出站审计日志尚未实现。
- **审计时间线**：空间详情页把用量 JSONL 与未来的出站日志拼成一条"agent 干了什么"时间线——事后审计代替事前弹窗的具象化。

待验证 / 开放问题：

- **凭证保活（已实现 ✅，`internal/keepalive` + `-cred-keepalive`）**：真机实测（2026-08-31）access token 寿命 claude ~8h、grok ~8h、codex ~6 天，且只有 vendor CLI 实际运行时才续期——host 一夜不用，空间内该家 AI 全 401。解法是 daemon 每 5 分钟检查各凭证的精确过期时间（claude 取文件与 Keychain 两份中最晚的 `expiresAt`；codex/grok 解 JWT 的 `exp`），距过期 <30 分钟（`-cred-keepalive`，0 关闭）就 spawn 一次最便宜的 CLI 调用让官方 CLI 自己续期回写：claude `--model haiku -p`、codex `exec -m gpt-5.6-luna -c model_reasoning_effort="low"`（后端拒绝 "minimal"；luna 实测比默认模型省约 1/3）、grok 默认模型 `-p --disable-web-search`（订阅只有 4.6/4.5，无更便宜模型，不 pin slug 以免下线后保活变哑）。只 ping「Host 总闸开 && 至少一个空间启用」的 provider。ping 子进程在空临时目录、最小环境（必须带 `USER`，否则 `claude -p` 直接鉴权失败）、独立进程组中运行，超时杀整组且 `WaitDelay` 10 秒，防止 hook/MCP 子进程持有管道挂死保活。实测 grok CLI 对未过期 token 不主动续期，所以过期后按 tick 级重试，最坏断档 ≈ 一个 tick（5 分钟）而非 8 小时。**不在网关里自实现 OAuth 刷新**：refresh token 可能轮换，与本地 CLI 互踩会把 host 登出。真机教训（2026-10-02）：此前网关与保活只看 `.credentials.json`，而 Claude Code 实际续的是 Keychain 那份，日志里 103 次 "ping ran but expiry did not advance" 全是这个原因；现在两处共用 `gateway.ClaudeOAuth`。

- cospace 域名（cospace.dev / cospace.sh 等）可用性实查；对外物料统一写法 CamelCase "CoSpace"，README 首句带定义句压 SEO（存在 CoSpaces Edu 与联合办公品牌同名碰撞）
- 邀请网页的最终形态细节（VPS 托管的实现面）
- **分发全部在 GitHub，项目不运营下载服务器**：安装脚本在 GitHub Pages（仓库 `docs/`：guest 的 `install.sh` / `install.ps1`，host 的 `host.sh`），二进制与 `VERSION` 在 GitHub Releases（`scripts/release.sh` 调 goreleaser），guest 的 Homebrew cask 在 `JingxuanKang/homebrew-tap`，空间镜像在 ghcr.io。下载地址与安装命令的单一源是 `internal/dist`：控制台邀请弹窗、邀请页和 CLI 都从它取，前端不写死。**安装命令即更新命令、可重复执行**：仓库根 `VERSION` 是版本单一源，发布时经 ldflags 注入并作为 `releases/latest/download/VERSION` 发布；guest 安装器先看 `command -v cospace`，版本一致就退出，否则原地覆盖 PATH 上那份（Homebrew 装的交给 `brew upgrade`），所以邀请永远只有一套三步命令。host 的 `host.sh` 装 Apple container（有 Homebrew 用 brew，否则用 Apple 签名 pkg）、装 `cospaced`、执行 `cospaced setup`（启动 container 服务并自动装内核、写 launchd、打开控制台），重复执行即升级；`cospaced uninstall` 只移除服务、保留空间与数据。Windows 产物已交叉编译，尚未做 Windows 真机端到端验证。零安装（纯 ssh）需要 BYO VPS 中转——`internal/relay` + `cmd/cospace-relay` + `deploy/relay-install.sh` 已实现并测试，但**尚未接进 `cospaced serve`**。
- **launchd 下 guest ssh 修复（已解决）**：长期运行的 daemon 进程启动早于 Apple container 的 vmnet 网桥，对容器 IP 得 `no route to host`（新起的进程则正常）。DialSSH 改为每次连接 spawn `nc <ip> 22` 子进程桥接（`ncDial`，socketpair 包成 net.Conn），新进程有路由、且天然扛容器/vmnet 重启。真机全链路（launchd daemon + brew cospace + tailcat + 空间内 claude）已验证通过。
- `cospace` 未签名/未公证，cask 用 postflight `xattr -dr com.apple.quarantine` 绕过 Gatekeeper；正式对外前应换 Apple Developer ID 签名+公证。大陆网络访问 GitHub / ghcr.io 可能很慢，待定镜像加速方案。
- relay 控制通道目前明文 TCP：guest 流量本身是 SSH 加密不受影响，但 daemon↔relay 的认证 token 可被路径上的中间人截获并冒用 daemon 注册空间。待加 TLS + 证书固定

Spike 实测记下的工程事实（daemon 实现要处理）：

- 容器重启后 IP 会变（192.168.64.4 → .5），daemon 必须每次动态解析，不可缓存
- 容器内可经 192.168.64.1 访问 Mac 宿主（凭证网关监听于此）
- spike 镜像把 sshd host key 烘进了镜像——产品必须在建空间时为每空间生成独立 host key
- codex 的 ChatGPT token 会刷新（auth.json 含 refresh_token），网关需同步拿到刷新后的 token
- 空间环境变量经 /etc/profile.d 注入，交互登录生效；非登录 ssh 命令需显式 source
