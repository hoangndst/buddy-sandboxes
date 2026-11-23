#!/usr/bin/env bash
set -euo pipefail

# 1. Seed home directory dotfiles if masked by emptyDir mount
if [ ! -f /home/sandbox/.bashrc ] && [ -d /etc/skel ]; then
    cp -rn /etc/skel/. /home/sandbox/ 2>/dev/null || true
fi

# 2. Configure Codex directory and broker config
mkdir -p "$HOME/.codex"
if [ "${BUDDY_PROVIDER_MODE:-}" = "broker" ]; then
    if [ -f /etc/buddy/codex/config.toml ]; then
        if [ -n "${OPENAI_MODEL:-}" ]; then
            sed "s/^model = .*/model = \"${OPENAI_MODEL}\"/" /etc/buddy/codex/config.toml > "$HOME/.codex/config.toml"
        else
            cp /etc/buddy/codex/config.toml "$HOME/.codex/config.toml"
        fi
    fi
fi

# 3. Chain execution under Tini for process group forwarding and reaping
exec /usr/bin/tini -g -s -- /opt/codex/node_modules/.bin/codex "$@"
