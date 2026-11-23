#!/usr/bin/env bash
set -euo pipefail

# 1. Seed home directory dotfiles if masked by emptyDir mount
if [ ! -f /home/sandbox/.bashrc ] && [ -d /etc/skel ]; then
    cp -rn /etc/skel/. /home/sandbox/ 2>/dev/null || true
fi

# 2. Strict umask
umask 0022

# 3. Chain execution
exec "$@"
