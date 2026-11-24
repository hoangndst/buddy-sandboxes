# Technical Specifications & System Designs

This directory contains technical specifications and system designs for Buddy Sandboxes platform subsystems.

## Documentation Index

| Specification | Domain | Description |
| :--- | :--- | :--- |
| [Architecture Overview](../architecture.md) | Platform Core | High-level system architecture, threat model, and CRD design. |
| [Product Roadmap](../roadmap.md) | Platform Lifecycle | Current capabilities overview and upcoming product roadmap. |
| [Agent Preset Hardening & Task Dispatch](agent_preset_hardening_and_task_dispatch.md) | Developer Ergonomics | Hardened agent presets, one-shot task dispatch, safe execution modes, and sync diff reporting. |
| [User Identity & Brokered Credentials](user_identity_and_provider_credentials.md) | Security & Identity | User identity, access tokens, AES-256-GCM credential storage, and provider broker. |
| [Custom Provider Endpoints & Models](custom_provider_endpoints_and_models.md) | Security & Identity | Custom provider base URLs, dynamic egress gateway tokens, and model overrides. |
| [Service Credentials & Private Git Access](service_credential_broker_and_git_authentication.md) | Security & Identity | User-managed provider credentials and repository-bound GitHub clone authentication. |
| [Administrator CLI & Tenant Operations](admin_cli_and_tenant_operations.md) | Interface & Tooling | Workstation administrative commands (`buddy-sbx admin`) for users, tokens, and tenant sandboxes. |
| [Agent Project Workflow](agent_project_workflow.md) | Developer Ergonomics | Local project snapshot staging, attached/detached runs, and bidirectional synchronization. |
| [Gateway & Developer CLI](gateway_and_cli.md) | Interface & Tooling | In-cluster API gateway (`buddy-api`), client SDK, and developer CLI (`buddy-sbx`). |
| [Workload Isolation & Runtimes](workload_isolation.md) | Security & Isolation | Multi-engine sandboxing (gVisor, Kata Containers), runtime admission, and Pod Security Admission. |
| [Workspaces & File Synchronization](workspaces_and_file_sync.md) | Storage & Data | Ephemeral/PVC workspace mounts, init-container Git seeding, and streaming tar transfers. |
| [Native MCP Sidecars](mcp_sidecars.md) | Tool Integration | Model Context Protocol sidecars, TCP startup probing, and lifecycle decoupling. |
| [Egress Gateway & Domain Whitelisting](egress_gateway_and_domain_whitelisting.md) | Network Security | Out-of-pod proxying, DNS rebinding/SSRF mitigation, and strict egress allowlisting. |
| [Workspace Cold Suspension & Resume](cold_workspace_suspension.md) | Cost Optimization | Zero-compute idle state and explicit suspension for PVC-backed sandboxes. |
| [Process Management & Introspection](process_namespace_and_introspection.md) | Container Runtime | Shared PID namespaces, Downward API metadata, and monotonic deadline tightening. |
| [Workspace Retention & Controller Events](workspace_retention_and_controller_events.md) | Storage & Observability | Declarative PVC reclaim policy, retention finalizers, Kubernetes event emission, and API rate limiting. |
