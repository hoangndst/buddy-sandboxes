#!/usr/bin/env bash
set -euo pipefail

TASK_FILE="${BUDDY_TASK_FILE:-/run/buddy/task.json}"
if [ ! -f "$TASK_FILE" ]; then
    echo "Task specification file not found: $TASK_FILE" >&2
    exit 1
fi

PROMPT_FILE="${BUDDY_PROMPT_FILE:-/run/buddy/prompt.txt}"
if [ -f "$PROMPT_FILE" ]; then
    PROMPT=$(cat "$PROMPT_FILE")
else
    PROMPT=$(jq -r '.prompt // empty' "$TASK_FILE")
fi

SYS_PROMPT_FILE="${BUDDY_SYSTEM_PROMPT_FILE:-/run/buddy/system-prompt.txt}"
if [ -f "$SYS_PROMPT_FILE" ]; then
    SYSTEM_PROMPT=$(cat "$SYS_PROMPT_FILE")
else
    SYSTEM_PROMPT=$(jq -r '.system_prompt // empty' "$TASK_FILE")
fi

MODEL=$(jq -r '.model // empty' "$TASK_FILE")

CODEX_ARGS=(--no-daemon --dangerously-bypass-approvals-and-sandbox)
if [ -n "$MODEL" ]; then
    CODEX_ARGS+=(--model "$MODEL")
fi
CODEX_ARGS+=(exec --skip-git-repo-check -)

if [ -n "$SYSTEM_PROMPT" ]; then
    printf "%s\n\n%s" "$SYSTEM_PROMPT" "$PROMPT" | exec /usr/local/bin/codex "${CODEX_ARGS[@]}"
elif [ -f "$PROMPT_FILE" ]; then
    exec /usr/local/bin/codex "${CODEX_ARGS[@]}" < "$PROMPT_FILE"
else
    printf '%s' "$PROMPT" | exec /usr/local/bin/codex "${CODEX_ARGS[@]}"
fi
