# CoSpace base space image.
# No credentials, keys, or env are baked in — cospaced injects the per-space
# gateway env (/etc/profile.d/cospace.sh) and authorized_keys at runtime.
FROM debian:bookworm-slim
# Every RUN below pipes through tail/dd; without pipefail a failed install
# would still produce a "successful" image with no claude/codex in it.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
LABEL org.opencontainers.image.source="https://github.com/JingxuanKang/cospace" \
      org.opencontainers.image.description="CoSpace space image: Linux dev VM with Claude Code, Codex and Grok" \
      org.opencontainers.image.licenses="MIT"
# Core dev toolchain. openssh-client (not just -server) is required for git
# over ssh / deploy keys; build-essential for npm node-gyp & pip C extensions.
RUN apt-get update && apt-get install -y --no-install-recommends \
      openssh-server openssh-client git tmux curl wget ca-certificates \
      python3 python3-venv build-essential \
      ripgrep fd-find jq tree less vim nano unzip zip htop \
      dnsutils netcat-openbsd procps bubblewrap \
    && ln -sf "$(command -v fdfind)" /usr/local/bin/fd \
    && rm -rf /var/lib/apt/lists/*
# GitHub CLI from the official apt repo (kept updatable).
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*
# uv (modern Python package manager). Install into a public path so the space
# user can run it — the installer defaults to ~/.local/bin (root's, unreadable).
RUN curl -LsSf https://astral.sh/uv/install.sh \
      | env UV_INSTALL_DIR=/usr/local/bin INSTALLER_NO_MODIFY_PATH=1 sh
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs && rm -rf /var/lib/apt/lists/*
# claude-hud is an INSTALLER (drops a statusline script into ~/.claude);
# ccusage powers that script's cost/reset line and ships a native binary
# that npm leaves without exec permission.
# Clean npm's download cache in the same layer: it is ~230 MB that every host
# would otherwise pull for nothing.
RUN npm install -g @anthropic-ai/claude-code @openai/codex claude-hud ccusage 2>&1 | tail -1 \
 && chmod -R a+rx /usr/lib/node_modules/ccusage \
 && npm cache clean --force && rm -rf /root/.npm /root/.cache
# xAI Grok CLI. The installer symlinks into /root/.grok, unreadable by the
# space user, so replace the links with the real binary. Spaces authenticate
# through the gateway (cospaced injects XAI_API_KEY + BYOK config).
RUN GROK_BIN_DIR=/usr/local/bin bash -c 'curl -fsSL https://x.ai/cli/install.sh | bash' 2>&1 | tail -2 \
 && cp "$(readlink -f /usr/local/bin/grok)" /usr/local/bin/grok-bin \
 && rm /usr/local/bin/grok /usr/local/bin/agent \
 && mv /usr/local/bin/grok-bin /usr/local/bin/grok \
 && ln -s grok /usr/local/bin/agent && chmod 755 /usr/local/bin/grok \
 && rm -rf /root/.grok
RUN mkdir /run/sshd && useradd -m -s /bin/bash space \
 && mkdir -p /home/space/.ssh && touch /home/space/.ssh/authorized_keys \
 && chmod 700 /home/space/.ssh && chmod 600 /home/space/.ssh/authorized_keys && chown -R space:space /home/space/.ssh
# Keys only, no root logins: stated explicitly so a package an agent installs
# later cannot quietly reintroduce password auth through a changed default.
RUN printf '%s\n' \
      'PasswordAuthentication no' \
      'KbdInteractiveAuthentication no' \
      'PermitRootLogin no' \
      'PubkeyAuthentication yes' \
      'PermitUserEnvironment no' \
      'AllowUsers space room' \
    > /etc/ssh/sshd_config.d/cospace.conf
# Pre-generate the statusline renderer script for the space user; drop the
# settings.json the installer writes so cospaced seeds its own (with the
# space-name prefix) at first sync.
RUN su space -c "claude-hud" 2>&1 | tail -1 \
 && rm -f /home/space/.claude/settings.json
RUN apt-get update && apt-get install -y --no-install-recommends nftables && rm -rf /var/lib/apt/lists/*
EXPOSE 22
COPY entrypoint.sh /usr/local/sbin/cospace-entrypoint
RUN chmod 755 /usr/local/sbin/cospace-entrypoint
CMD ["/usr/local/sbin/cospace-entrypoint"]
