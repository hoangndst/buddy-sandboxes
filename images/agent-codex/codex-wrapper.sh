#!/usr/bin/env bash
set -euo pipefail

# 1. Seed home directory dotfiles if masked by emptyDir mount
if [ ! -f /home/sandbox/.bashrc ] && [ -d /etc/skel ]; then
    cp -rn /etc/skel/. /home/sandbox/ 2>/dev/null || true
fi

# 2. Configure Codex directory and broker config
mkdir -p "$HOME/.codex"
if [ "${BUDDY_PROVIDER_MODE:-}" = "broker" ]; then
    config_path="${BUDDY_CODEX_CONFIG_PATH:-/etc/buddy/codex/config.toml}"
    if [ -f "$config_path" ]; then
        if [ -n "${OPENAI_MODEL:-}" ]; then
            safe_model=$(printf '%s\n' "$OPENAI_MODEL" | sed 's/[\/&]/\\&/g')
            sed "s/^model = .*/model = \"${safe_model}\"/" "$config_path" > "$HOME/.codex/config.toml"
        else
            cp "$config_path" "$HOME/.codex/config.toml"
        fi
    fi
fi

# 3. Chain execution under Tini for process group forwarding and reaping
if [ -n "${BUDDY_CODEX_SKIP_EXEC:-}" ]; then
    exit 0
fi
if [ "$#" -gt 0 ] && [ "$1" = "sleep" ]; then
    exec /usr/bin/tini -g -s -- "$@"
fi
exec /usr/bin/tini -g -s -- /opt/codex/node_modules/.bin/codex "$@"
