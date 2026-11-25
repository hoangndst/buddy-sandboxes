---
title: "buddy-sbx get"
linkTitle: "get"
description: "Display one or many resources"
slug: "buddy-sbx_get"
---

## buddy-sbx get

Display one or many resources

### Synopsis

Get displays resources in a tabular or JSON format.

Supported resource types:
  - sandboxes (aliases: sandbox, sbx)
  - sessions  (aliases: session, sess)
  - secrets   (aliases: secret)
  - users     (aliases: user)

```
buddy-sbx get <resource> [name] [flags]
```

### Examples

```
  # List all sandboxes
  buddy-sbx get sandboxes

  # List sandboxes in short form
  buddy-sbx get sbx

  # List sandboxes with wide details
  buddy-sbx get sbx -o wide

  # Get a specific sandbox
  buddy-sbx get sbx my-box

  # List sandboxes belonging to a specific user
  buddy-sbx get sbx --user usr_alice

  # List sessions inside a sandbox
  buddy-sbx get sessions -s my-box

  # List credential secrets
  buddy-sbx get secrets
```

### Options

```
  -h, --help             help for get
  -o, --output string    Output format (json or wide)
  -s, --sandbox string   Target sandbox for child resources (sessions)
  -u, --user string      Filter sandboxes by user ID (admin only)
```

### Options inherited from parent commands

```
      --config string     Path to config file (default: ~/.buddy/config.yaml)
      --endpoint string   Buddy Gateway endpoint URL (overrides context)
      --token string      Authentication token (overrides context)
```

### SEE ALSO

* [buddy-sbx](../)	 - buddy-sbx is a developer CLI for managing isolated AI coding-agent sandboxes

