---
title: "buddy-sbx exec"
linkTitle: "exec"
description: "Execute a command inside an active sandbox"
slug: "buddy-sbx_exec"
---

## buddy-sbx exec

Execute a command inside an active sandbox

### Synopsis

Exec runs a command inside an active sandbox, supporting interactive TTY streaming and signal forwarding.

```
buddy-sbx exec <sandbox-id> [flags] -- <command...>
```

### Examples

```
  # Execute a single command in an active sandbox
  buddy-sbx exec my-box -- ls -la /workspace

  # Execute an interactive shell with TTY
  buddy-sbx exec -it my-box -- /bin/sh

  # Run a command detached in the background
  buddy-sbx exec -d my-box -- python worker.py
```

### Options

```
  -d, --detach        Run command in background supervisor session
  -h, --help          help for exec
  -i, --interactive   Keep STDIN open even if not attached
  -t, --tty           Allocate a pseudo-TTY
```

### Options inherited from parent commands

```
      --config string     Path to config file (default: ~/.buddy/config.yaml)
      --endpoint string   Buddy Gateway endpoint URL (overrides context)
      --token string      Authentication token (overrides context)
```

### SEE ALSO

* [buddy-sbx](../)	 - buddy-sbx is a developer CLI for managing isolated AI coding-agent sandboxes

