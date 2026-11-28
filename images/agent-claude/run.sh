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
MAX_STEPS="${BUDDY_MAX_STEPS:-$(jq -r '.max_steps // empty' "$TASK_FILE")}"

CLAUDE_ARGS=(--dangerously-skip-permissions)
if [ -n "$MODEL" ]; then
    CLAUDE_ARGS+=(--model "$MODEL")
fi
if [ -n "$MAX_STEPS" ] && [ "$MAX_STEPS" -gt 0 ] 2>/dev/null; then
    CLAUDE_ARGS+=(--max-turns "$MAX_STEPS")
fi
if [ -f "$SYS_PROMPT_FILE" ]; then
    CLAUDE_ARGS+=(--append-system-prompt-file "$SYS_PROMPT_FILE")
elif [ -n "$SYSTEM_PROMPT" ]; then
    CLAUDE_ARGS+=(--append-system-prompt "$SYSTEM_PROMPT")
fi
CLAUDE_ARGS+=(-p -)

if [ -f "$PROMPT_FILE" ]; then
    exec /usr/local/bin/claude "${CLAUDE_ARGS[@]}" < "$PROMPT_FILE"
else
    printf '%s' "$PROMPT" | exec /usr/local/bin/claude "${CLAUDE_ARGS[@]}"
fi
