# Buddy Sandboxes

Kubernetes-native operator providing isolated, declarative execution environments for automated AI coding agents.

## Features

- Declarative API: Define sandboxes using the `Sandbox` custom resource (`sandboxes.buddy.io/v1alpha1`).
- Workload Isolation: Multi-layer containment combining non-root security contexts, dropped capabilities, read-only root filesystems, and `RuntimeClass` integration (gVisor / Kata / runc).
- Scoped Network Boundaries: Automatic NetworkPolicy synthesis per sandbox blocking cloud metadata (`169.254.169.254/32`) and private subnets (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`), with selective egress for package registries and git.
- Workspace Storage: Configurable workspace volume mounts using emptyDir (ephemeral mode) or PersistentVolumeClaims (persistent mode).
- Lifecycle Control: Active deadline enforcement (`maxDurationSeconds`) mapped to native pod deadlines and condition-based status reporting.
- Autonomous Loop: Includes a persistent automation runner managing the specification, testing, and review stages.

## Quickstart

### Prerequisites

- Go 1.25+
- Docker 24+
- Kubernetes cluster (Kind, Minikube, or cloud provider)
- kubectl and kustomize

### Installation

Clone the repository and install CRDs:

```bash
git clone https://github.com/hoangndst/buddy-sandboxes.git
cd buddy-sandboxes
make install
```

Run the operator locally:

```bash
make run
```

### Creating a Sandbox

Apply a sample sandbox custom resource:

```yaml
apiVersion: sandboxes.buddy.io/v1alpha1
kind: Sandbox
metadata:
  name: agent-task-01
  namespace: default
spec:
  image: "golang:1.25"
  command: ["/bin/sh", "-c"]
  args: ["sleep 3600"]
  workingDir: "/workspace"
  maxDurationSeconds: 3600
  workspace:
    ephemeral: true
    mountPath: "/workspace"
  network:
    allowInternetWeb: true
    allowDNS: true
    blockClusterInternal: true
    blockMetadataService: true
```

Check sandbox state:

```bash
kubectl get sandboxes
kubectl describe sandbox agent-task-01
```

## Running Tests

Run unit and integration tests with envtest:

```bash
make test
```

Run linter and formatting checks:

```bash
make fmt vet
```

## License

Apache License 2.0.
