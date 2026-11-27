#!/usr/bin/env bash
set -euo pipefail

TASK_FILE="${BUDDY_TASK_FILE:-/run/buddy/task.json}"
if [ ! -f "$TASK_FILE" ]; then
    echo "Task specification file not found: $TASK_FILE" >&2
    exit 1
fi

PROMPT=$(jq -r '.prompt // empty' "$TASK_FILE")
if [ -z "$PROMPT" ]; then
    echo "Empty prompt in task specification: $TASK_FILE" >&2
    exit 1
fi

exec /bin/sh -c "$PROMPT"
