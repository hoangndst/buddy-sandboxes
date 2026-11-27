#!/usr/bin/env bash
set -euo pipefail

TASK_FILE="${BUDDY_TASK_FILE:-/run/buddy/task.json}"
if [ ! -f "$TASK_FILE" ]; then
    echo "Task specification file not found: $TASK_FILE" >&2
    exit 1
fi

PROMPT=$(jq -r '.prompt // empty' "$TASK_FILE")
SYSTEM_PROMPT=$(jq -r '.system_prompt // empty' "$TASK_FILE")
MODEL=$(jq -r '.model // empty' "$TASK_FILE")

CLAUDE_ARGS=(--dangerously-skip-permissions)
if [ -n "$MODEL" ]; then
    CLAUDE_ARGS+=(--model "$MODEL")
fi
if [ -n "$SYSTEM_PROMPT" ]; then
    CLAUDE_ARGS+=(--append-system-prompt "$SYSTEM_PROMPT")
fi
CLAUDE_ARGS+=(-p "$PROMPT")

exec /usr/local/bin/claude "${CLAUDE_ARGS[@]}"
