---
title: "Concepts"
linkTitle: "Concepts"
description: "Core architectural concepts, security boundaries, and runtime execution"
weight: 20
---

# Concepts

Buddy Sandboxes introduces Kubernetes-native abstractions designed specifically for running autonomous agents securely.

## Core Resources

- **Sandbox (`sandboxes.buddy.io/v1alpha1`)**: The primary execution unit. Manages container lifecycles, resource quotas, compute runtime classes, and execution deadlines.
- **NetworkProfile (`sandboxes.buddy.io/v1alpha1`)**: Defines egress boundaries, DNS domain filtering, private subnet filtering, and metadata service blocking.
- **SandboxPool (`sandboxes.buddy.io/v1alpha1`)**: Maintains a standby pool of warm, pre-warmed sandbox pods for sub-second agent scheduling.
- **SandboxLease (`sandboxes.buddy.io/v1alpha1`)**: Atomic runner reservation from a SandboxPool with automatic expiration.
- **Workspace (`sandboxes.buddy.io/v1alpha1`)**: Manages persistent volume claims and Git repository checkout across sandbox restarts and suspensions.

## Architecture Highlights

1. **Defense-in-Depth Isolation**: Workloads run as non-root users with dropped Linux capabilities, optional gVisor or Kata container runtimes, and strict read-only root filesystems.
2. **Egress Firewall**: Network policies prevent data exfiltration, access to cloud instance metadata (`169.254.169.254`), and unauthorized cluster lateral movement.
3. **Out-of-Band Secrets**: Model API keys and Git credentials remain on the control plane or gateway broker; tokens are never mounted or exposed directly in container environment variables.
