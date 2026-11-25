---
title: "buddy-sbx resume"
linkTitle: "resume"
description: "resume a PVC-backed sandbox"
slug: "buddy-sbx_resume"
---

## buddy-sbx resume

resume a PVC-backed sandbox

```
buddy-sbx resume <sandbox-id> [flags]
```

### Examples

```
  # resume a sandbox by ID
  buddy-sbx resume my-box
```

### Options

```
  -h, --help               help for resume
      --timeout duration   Maximum time to wait for the sandbox transition (default 3m0s)
```

### Options inherited from parent commands

```
      --config string     Path to config file (default: ~/.buddy/config.yaml)
      --endpoint string   Buddy Gateway endpoint URL (overrides context)
      --token string      Authentication token (overrides context)
```

### SEE ALSO

* [buddy-sbx](../)	 - buddy-sbx is a developer CLI for managing isolated AI coding-agent sandboxes

