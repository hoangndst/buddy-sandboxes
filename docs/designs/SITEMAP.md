# Buddy Sandboxes: Architecture and System Designs Sitemap

This directory contains published architectural specifications and system designs for Buddy Sandboxes.

## Design Documents

| Document | Milestone | Status | Description |
| :--- | :--- | :--- | :--- |
| [architecture.md](../architecture.md) | Platform Core | Active | Overall platform architecture, threat model, and CRD specification. |
| [roadmap.md](../roadmap.md) | Platform Lifecycle | Active | Complete multi-milestone platform roadmap (Milestones 1 through 8). |
| [gateway_and_cli.md](gateway_and_cli.md) | Milestone 2 | Merged (PR #3) | In-cluster gateway service (`buddy-api`), client SDK, and developer CLI (`buddy-sbx`). |
| [workload_isolation.md](workload_isolation.md) | Milestone 3 | Merged (PR #4) | Workload Isolation, RuntimeClasses, and Hardened Security Profiles specification. |
| [workspaces_and_file_sync.md](workspaces_and_file_sync.md) | Milestone 4 | Merged (PR #5) | Workspaces, Repository Seeding, and Streaming File Synchronization specification. |
| [process_namespace_and_introspection.md](process_namespace_and_introspection.md) | Milestone 5 | Accepted | Opt-in process namespace sharing, Downward API metadata, and monotonic deadline updates. |
