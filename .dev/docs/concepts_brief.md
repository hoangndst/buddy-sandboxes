# Buddy Sandboxes API Gateway and Client CLI (`buddy-api` & `buddy-sbx`)

* **Author:** hoangndst
* **Date:** 2026-11-20

## Overview

Buddy Sandboxes operates as a full-service platform providing isolated coding environments for developers and AI agents. To eliminate the requirement for developers to configure `kubectl`, manage kubeconfigs, or write raw Kubernetes YAML, the system introduces a dedicated two-tier client and gateway architecture:

1. `buddy-sbx`: A standalone developer and agent CLI tool.
2. `buddy-api`: A stateless in-cluster API gateway service.

The resulting request path is:

```
Developer / AI Agent -> buddy-sbx CLI -> buddy-api Service -> Kubernetes API Server (Sandbox CR) -> buddy-sandboxes Operator
```

## Architecture and Mechanisms

### 1. Control Plane (`buddy-api`)
The `buddy-api` service exposes a standard REST and WebSocket API:
- `POST /v1/sandboxes`: Accepts sandbox execution parameters (image, duration, memory/cpu, workspace mode) and synthesizes a `Sandbox` custom resource in the appropriate tenant namespace.
- `GET /v1/sandboxes`: Lists active sandboxes filtered by tenant or owner.
- `GET /v1/sandboxes/{id}`: Returns status, conditions, and connection endpoints.
- `DELETE /v1/sandboxes/{id}`: Initiates cascading teardown.

### 2. Data Plane (Terminal Streaming and Exec)
- `GET /v1/sandboxes/{id}/exec` (WebSocket): Establishes a bidirectional streaming connection. The `buddy-api` service terminates client WebSocket frames and bridges stdin, stdout, stderr, and terminal resize events (SIGWINCH) to the Kubernetes API `pods/exec` subresource.

### 3. Workspace Ingestion
- `POST /v1/sandboxes/{id}/workspace` (HTTP Multipart): Uploads a compressed tar archive of the developer's local working directory. The API gateway extracts the archive into the sandbox pod's `/workspace` volume before initiating tool execution.
- `GET /v1/sandboxes/{id}/workspace/diff`: Streams modified files back to the host machine upon session termination.

### 4. Client CLI (`buddy-sbx`)
The `buddy-sbx` binary provides developer commands:
- `buddy-sbx run <agent> [flags]`: Ingests the current working directory, creates a remote sandbox, attaches an interactive terminal session, and cleans up on exit (`--rm`).
- `buddy-sbx exec <id> -- <cmd>`: Executes one-off commands inside an existing sandbox.
- `buddy-sbx ls`: Displays running sandboxes and health status.
- `buddy-sbx rm <id>`: Deletes a sandbox.

## Analysis

### Contrarian and YAGNI Lens
Building a custom API gateway adds operational complexity compared to running `client-go` locally on the developer machine. However, for a managed platform:
1. Distributing kubeconfig files and granting direct Kubernetes API access to hundreds of developers or automated agent bots violates least-privilege security boundaries.
2. The `buddy-api` service isolates the cluster control plane behind API tokens, request rate limiters, and tenant quotas.
3. Keeping `buddy-api` stateless by storing state directly in `Sandbox` CRDs avoids external database operational overhead.

### SRE and Scalability Lens
Bidirectional terminal streaming over WebSockets holds long-lived connections.
- The `buddy-api` gateway must implement connection draining on graceful shutdown.
- Horizontal Pod Autoscaling (HPA) must scale based on active WebSocket connection counts and CPU usage.

## Goals vs. Non-Goals

### In-Scope Goals
- Provide a clean CLI (`buddy-sbx`) requiring zero `kubectl` installation or cluster knowledge.
- Deliver an in-cluster stateless gateway (`buddy-api`) providing REST and WebSocket endpoints.
- Support bidirectional PTY terminal streaming for interactive agents.
- Support workspace tarball ingestion and diff extraction.

### Out-of-Scope Non-Goals
- Proprietary database persistence in Milestone 1 (state is stored entirely in Kubernetes CRDs).
- Custom virtual machine hypervisors (workload isolation continues to rely on Kubernetes RuntimeClass with gVisor and Kata).

## Alternatives Weighed

- **Alternative 1: Direct Client-go Binary without API Server**:
  Rejected by author. While direct client-go avoids running an in-cluster API server, it forces every developer and agent worker to possess a valid kubeconfig and direct network access to the Kubernetes control plane, violating enterprise security policies.

- **Alternative 2: In-Pod SSH Daemon**:
  Rejected by author. Running an OpenSSH server inside each sandbox pod requires opening ingress ports, managing host keys, and violating the default-deny ingress NetworkPolicy. WebSocket-to-exec proxying preserves ingress isolation.

## Key Decisions & Staging for Next Steps

1. Decision: Keep `buddy-api` completely stateless in Milestone 1, deriving all state directly from `sandboxes.buddy.io/v1alpha1` custom resources.
2. Next Step: Initiate EGM multi-lens review on this concepts brief.
