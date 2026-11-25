---
title: "buddy-sbx config"
linkTitle: "config"
description: "Manage local connection contexts"
slug: "buddy-sbx_config"
---

## buddy-sbx config

Manage local connection contexts

### Synopsis

Config manages gateway endpoints and authentication tokens stored in ~/.buddy/config.yaml.

### Examples

```
  # View all configured connection contexts
  buddy-sbx config get-contexts

  # Show the currently active context
  buddy-sbx config current-context

  # Switch active context
  buddy-sbx config use-context staging

  # Define or update a context endpoint
  buddy-sbx config set-context prod --endpoint https://gateway.buddy.example.com
```

### Options

```
  -h, --help   help for config
```

### Options inherited from parent commands

```
      --config string     Path to config file (default: ~/.buddy/config.yaml)
      --endpoint string   Buddy Gateway endpoint URL (overrides context)
      --token string      Authentication token (overrides context)
```

### SEE ALSO

* [buddy-sbx](../)	 - buddy-sbx is a developer CLI for managing isolated AI coding-agent sandboxes
* [buddy-sbx config current-context](../buddy-sbx_config_current-context/)	 - Display the active context name
* [buddy-sbx config get-contexts](../buddy-sbx_config_get-contexts/)	 - List all configured contexts
* [buddy-sbx config set-context](../buddy-sbx_config_set-context/)	 - Add or update a context
* [buddy-sbx config use-context](../buddy-sbx_config_use-context/)	 - Switch the active context

