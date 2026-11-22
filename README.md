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

The local gateway uses PostgreSQL for Buddy identities and encrypted provider credentials. Create the database and encryption Secrets once, then deploy the API:

```bash
make docker-build-codex IMG=ghcr.io/hoangndst/buddy-sandboxes-codex:0.171.0
kind load docker-image ghcr.io/hoangndst/buddy-sandboxes-codex:0.171.0 --name dell-cluster
install -d -m 0700 "$HOME/.buddy"
(umask 077; openssl rand -hex 32 > "$HOME/.buddy/postgres-password")
(umask 077; printf '{"1":"%s"}\n' "$(openssl rand -base64 32)" > "$HOME/.buddy/provider-encryption-keys.json")
kubectl create secret generic buddy-postgres -n buddy-sandboxes-system --from-file=password="$HOME/.buddy/postgres-password" --dry-run=client -o yaml | kubectl apply -f -
DB_PASSWORD="$(cat "$HOME/.buddy/postgres-password")"
printf 'postgres://buddy:%s@buddy-postgres.buddy-sandboxes-system.svc:5432/buddy?sslmode=disable' "$DB_PASSWORD" > "$HOME/.buddy/database-url"
chmod 0600 "$HOME/.buddy/database-url"
kubectl create secret generic buddy-api-database -n buddy-sandboxes-system --from-file=url="$HOME/.buddy/database-url" --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic buddy-api-provider-encryption -n buddy-sandboxes-system --from-file=keys.json="$HOME/.buddy/provider-encryption-keys.json" --dry-run=client -o yaml | kubectl apply -f -
make gateway-deploy
```

The egress gateway and operator must already be deployed with `buddy-sandboxes-egress-gateway-secret`. The API uses that secret to authorize provider requests through the egress gateway. `make gateway-deploy` applies a pinned single-replica PostgreSQL StatefulSet, runs the schema migration Job, enables the `default` namespace, and deploys the API. The local gateway package includes a Cilium policy for Kubernetes API access and requires the CiliumNetworkPolicy CRD. Production deployments should use an externally managed PostgreSQL service and back up the database together with its encryption keys.

Forward the local API and database services in separate terminals:

```bash
kubectl port-forward -n buddy-sandboxes-system svc/buddy-api 8080:8080
kubectl port-forward -n buddy-sandboxes-system svc/buddy-postgres 15432:5432
```

Enable the tenant namespace using the operator's kubeconfig and database access:

```bash
DB_URL="$(sed 's/@buddy-postgres.buddy-sandboxes-system.svc:5432/@127.0.0.1:15432/' "$HOME/.buddy/database-url")"
BUDDY_DATABASE_URL="$DB_URL" go run ./cmd/buddy-api namespace enable --namespace default
```

Initialize the first administrator once. Store a one-time bootstrap token hash, then consume the token. Save the administrator login token, which is printed only once:

```bash
(umask 077; openssl rand -base64 32 > "$HOME/.buddy/bootstrap-token")
DB_URL="$(cat "$HOME/.buddy/database-url" | sed 's/@buddy-postgres.buddy-sandboxes-system.svc:5432/@127.0.0.1:15432/')"
BUDDY_DATABASE_URL="$DB_URL" go run ./cmd/buddy-api migrate --bootstrap-token-file "$HOME/.buddy/bootstrap-token"
BUDDY_DATABASE_URL="$DB_URL" go run ./cmd/buddy-api bootstrap-admin --namespace default --bootstrap-token-file "$HOME/.buddy/bootstrap-token" > "$HOME/.buddy/admin-token"
chmod 0600 "$HOME/.buddy/admin-token"
buddy-sbx login --token-file "$HOME/.buddy/admin-token" http://127.0.0.1:8080
buddy-sbx secret set openai
buddy-sbx secret set anthropic
buddy-sbx run codex . -- "Fix the failing tests"
```

`buddy-sbx secret set` reads the key without echoing it and sends it to Buddy over loopback or HTTPS. Buddy encrypts keys before storing them in PostgreSQL. Agent Pods receive a short-lived Sandbox capability in the provider-specific environment variable; the API key remains outside the Pod. Use `--sandbox <name>` for a Sandbox-specific key. `buddy-sbx secret ls` displays metadata only. The authenticated user owns provider keys and sandboxes in the namespace assigned by an administrator; namespace selection is not a CLI option.

Provider usage can incur charges from the provider. Set a usage limit with the provider if available.

Project Sandboxes are retained after synchronization by default. Pass `--rm` to delete one after a conflict-free sync. On conflicts or transfer errors, the Sandbox stays available for recovery. `--preview` lists included and excluded paths without creating a Sandbox. Windows synchronizes writable files as `0644`, read-only files as `0444`, and directories as `0755`; POSIX group, other, and executable permission bits are not preserved on Windows.

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
