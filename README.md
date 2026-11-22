# Buddy Sandboxes

Kubernetes-native operator providing isolated, declarative execution environments for automated AI coding agents.

## Features

- Declarative API: Define sandboxes using the `Sandbox` custom resource (`sandboxes.buddy.io/v1alpha1`).
- Workload Isolation: Multi-layer containment combining non-root security contexts, dropped capabilities, read-only root filesystems, and `RuntimeClass` integration (gVisor / Kata / runc).
- Scoped Network Boundaries: Automatic NetworkPolicy synthesis per sandbox blocking cloud metadata (`169.254.169.254/32`) and private subnets (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`), with selective egress for package registries and git.
- Workspace Storage: Configurable workspace volume mounts using emptyDir (ephemeral mode) or PersistentVolumeClaims (persistent mode).
- Lifecycle Control: Active deadline enforcement (`maxDurationSeconds`), explicit suspend/resume for writable PVC-backed workspaces, and condition-based status reporting.
- Process Reaping and Introspection: Optional shared process namespace for orphan reaping, plus nonsecret Sandbox, Pod, resource, and initial deadline environment values.
- Observable Execution: Detailed lifecycle phases, status conditions, and container exit code tracking.
- Local Project Runs: Bounded project snapshots, streamed terminal sessions, conflict-aware result sync, and a brokered Codex provider path.

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

Process namespace sharing is opt-in with `spec.shareProcessNamespace: true` or `buddy-sbx run --share-process-namespace`. It changes PID 1 behavior and process visibility within the Pod, so it remains disabled by default. Sandbox deadlines can be set or tightened, but not increased or removed after they are configured.

### Suspending a persistent workspace

Suspension deletes the execution Pod and stops its processes. The workspace data is retained on a writable PVC. Resuming creates a new Pod and starts the configured command again, so commands may repeat external side effects. Sandboxes using `emptyDir` or a read-only claim cannot be suspended.

```bash
buddy-sbx suspend agent-task-01 --timeout=3m
buddy-sbx resume agent-task-01 --timeout=3m
```

The timeout only limits how long the CLI waits. It does not cancel the requested state change. A Sandbox's `maxDurationSeconds` deadline continues to elapse while suspended.

### Running an agent against a local project

`buddy-sbx run codex . -- "Fix the failing tests"` uploads a filtered snapshot of the current directory, starts Codex with its native terminal interface, and merges the result when the process exits. The local directory is not mounted into the Pod. `.git` history and excluded secret or build paths are not transferred. Changes are synchronized after the attached process exits; this is not continuous directory synchronization.

Build and load the local API and Codex images, create a private API token, and deploy the local gateway:

```bash
make docker-build-buddy-api IMG=buddy-api:local
make docker-build-codex IMG=ghcr.io/hoangndst/buddy-sandboxes-codex:0.171.0
kind load docker-image buddy-api:local --name dell-cluster
kind load docker-image ghcr.io/hoangndst/buddy-sandboxes-codex:0.171.0 --name dell-cluster
install -d -m 0700 "$HOME/.buddy"
(umask 077; openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n' > "$HOME/.buddy/api-token")
chmod 0600 "$HOME/.buddy/api-token"
kubectl create secret generic buddy-api-token -n buddy-sandboxes-system --from-file=token="$HOME/.buddy/api-token" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -k config/local-gateway
```

The egress gateway and operator must already be deployed with the generated `buddy-sandboxes-egress-gateway-secret`. The API uses that same secret to authorize requests through the egress gateway. The local API maps its token to the `default` namespace. The local gateway Kustomize package includes a Cilium policy that permits the API Pod to reach the Kubernetes API server; this package requires the CiliumNetworkPolicy CRD.

In one terminal, forward the local API service and log in from another:

```bash
kubectl port-forward -n buddy-sandboxes-system svc/buddy-api 8080:8080
buddy-sbx login --token-file "$HOME/.buddy/api-token" http://127.0.0.1:8080
```

Set `OPENAI_API_KEY` in the CLI process environment and start a project run:

```bash
read -rsp "OpenAI API key: " OPENAI_API_KEY; echo
export OPENAI_API_KEY
buddy-sbx run codex . --allow-api-spend -- "Fix the failing tests"
unset OPENAI_API_KEY
```

Codex project runs require explicit `--allow-api-spend`, default to a two-hour Sandbox deadline, and keep the provider key in the local CLI and API process. The Pod receives a short-lived random session token. Provider credentials are not forwarded into project Sandboxes. Legacy runs without a local path require the explicit `--unsafe-forward-provider-key` option before forwarding a provider key.

The broker enforces a cumulative token ceiling, not a dollar-denominated spending cap. `--allow-api-spend` permits billable requests but does not set a maximum charge.

Project Sandboxes are retained after synchronization by default. Pass `--rm` to delete one after a conflict-free sync. On conflicts or transfer errors, the Sandbox stays available for recovery. `--preview` lists included and excluded paths without creating a Sandbox or credential session. Windows synchronizes writable files as `0644`, read-only files as `0444`, and directories as `0755`; POSIX group, other, and executable permission bits are not preserved on Windows.

Detached mode stages the project and leaves an idle container running. It does not keep Codex running in the background. `buddy-sbx exec` starts a new process, and `buddy-sbx cp` retrieves files explicitly. Reattaching to the same Codex conversation and named-Sandbox reuse are not supported.

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
