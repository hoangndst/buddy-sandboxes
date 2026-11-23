# Remote Git Workspaces

## Problem

Developers need to start an agent against a remote Git repository without first downloading it to the CLI host or uploading local project files. Buddy already supports repository seeding through Sandbox workspace configuration, but the CLI exposes only low-level `--git-*` flags and does not provide a clear remote-workspace workflow.

## User experience

Run an agent directly against a remote HTTPS repository:

```sh
buddy-sbx run codex --clone https://github.com/example/service.git --branch feature/cache
```

Select a shallow clone or a same-namespace Kubernetes Secret for a private repository:

```sh
buddy-sbx run claude --clone https://git.example.net/team/service.git \
  --branch release/2.x --git-depth 1 --git-secret-ref service-git
```

`--clone` accepts an HTTPS URL without embedded credentials. `--branch` selects a branch, tag, or commit. If omitted, Git checks out the repository's default branch. A tag or commit may leave the worktree in detached HEAD. `--git-depth 0` or omission requests full history; positive values request a shallow clone. `--git-subpath` places the repository under a relative directory inside `/workspace`.

The CLI waits for the Sandbox to reach Running. Kubernetes runs the `git-init` container before the agent container, so the selected repository is ready when the agent starts. The CLI does not download, upload, or synchronize a local project in this mode. Changes remain in the Sandbox workspace. They are not pushed to the remote automatically; users can inspect or retrieve them with the existing `exec` and `cp` commands.

New attached clone runs are retained by default, including after agent errors and clone failures, so the workspace and failure state remain available for inspection. Use `--rm` to request deletion after the attached run. A detached clone is rejected because detached mode creates an idle Sandbox and does not start the selected agent.

## CLI contract

```text
buddy-sbx run <preset|image> [local-path] [flags] [-- task args...]
```

| Flag | Meaning |
| --- | --- |
| `--clone URL` | Clone a remote HTTPS repository into the Sandbox workspace. |
| `--branch REF` | Branch, tag, or commit to check out. Defaults to the remote default branch. |
| `--git-depth N` | Clone history depth. Zero or omission means full history. |
| `--git-subpath PATH` | Relative workspace subdirectory for the clone. |
| `--git-secret-ref NAME` | Same-namespace Secret with HTTPS Git username and password/token values. |

The existing `--git-url` and `--git-branch` flags remain accepted for compatibility and are deprecated in favor of `--clone` and `--branch`. Supplying both spellings for the same value is an error. Credential-free legacy `--git-url` workflows retain their existing supported URL schemes and lifecycle behavior. Legacy runs that reference a Secret must migrate to the repository-bound HTTPS Secret format described below.

`--clone` is mutually exclusive with a local project path, `--git-url`, `--pvc`, `--preview`, and `--detach`. Branch, depth, subpath, and Secret-reference options require `--clone` or legacy `--git-url`. Invalid combinations fail before creating a Sandbox. The existing project snapshot, PVC, and legacy Git-seeding modes are otherwise unchanged.

The credential Secret is resolved in the caller's assigned Kubernetes namespace; CLI users cannot choose another namespace. Each user identity is assigned one namespace, which is the Kubernetes isolation boundary for that user's Sandbox and Git Secrets. An administrator or cluster operator creates the Secret in that assigned namespace with `GIT_CREDENTIAL_HOST`, `GIT_CREDENTIAL_PATH`, `GIT_USERNAME`, and `GIT_TOKEN` data keys. The token value may be a password when the Git server accepts HTTPS basic authentication. The namespace is assigned when the user account is provisioned. For example:

```sh
kubectl create secret generic service-git -n <assigned-user-namespace> \
  --from-literal=GIT_CREDENTIAL_HOST=git.example.net \
  --from-literal=GIT_CREDENTIAL_PATH=team/service.git \
  --from-literal=GIT_USERNAME=git \
  --from-file=GIT_TOKEN=/secure/path/to/repository-token
```

For private repositories, the Secret must also bind the credential to one exact repository using `GIT_CREDENTIAL_HOST` (lowercase DNS host, no port) and `GIT_CREDENTIAL_PATH` (canonical repository path without a leading slash). It contains `GIT_USERNAME` and `GIT_TOKEN`. Existing unbound Secrets fail closed. The init-container credential helper returns credentials only when Git requests the exact HTTPS host and path, with `credential.useHttpPath=true`; it does not persist credentials for `store` or `erase` requests. Create the Secret in the user's assigned namespace with these four keys. Kubernetes namespace authorization determines who can reference it.

For `--clone`, the URL must use canonical HTTPS syntax with no userinfo, query, fragment, encoded or ambiguous path forms, or explicit port. The API rejects HTTP(S) URL userinfo for all Git workspace requests, and credentialed URLs must use canonical HTTPS syntax at both the API and CRD boundaries. URL host and repository path must exactly match the Secret binding. Credentialed Git requests do not follow HTTP redirects, preventing credentials from being reused for another path or host. The Secret is mounted read-only only in `git-init`.

When `--git-subpath` is set with `--clone`, the agent starts with `/workspace/<subpath>` as its working directory. Without a subpath, it starts in `/workspace`. Credential-free legacy `--git-url` behavior is unchanged; legacy Secret references must use the repository-bound format.

## Architecture

The CLI maps clone options into the existing `Workspace.Git` request. The authenticated API creates the Sandbox using its existing namespace assignment and authorization rules. The operator places the `git-init` init container ahead of the agent container and mounts the workspace at `/workspace`. Git clone runs inside the Sandbox Pod, not on the CLI host.

The init container follows the Sandbox's network policy and configured egress proxy. It runs as a non-root UID, drops all Linux capabilities, and uses the runtime's default seccomp profile. The existing workspace mount and Git init script are reused. No separate clone service or host-side repository transfer is introduced.

The repository destination and agent working directory are separate fields. `--git-subpath` sets both the clone destination and, for canonical `--clone` mode, the agent working directory. The default directory remains `/workspace`. Legacy `--git-url --git-subpath` does not change its working-directory behavior.

## Security boundaries

- Canonical `--clone` and all credentialed Git URLs require HTTPS. Credential-free legacy Git seeding retains its existing URL schemes. Reject HTTP(S) URL userinfo for all Git workspace requests.
- The CLI and API never accept a credential value for Git authentication. The API request contains only the Secret name, not Secret data.
- The referenced Secret is mounted read-only into `git-init` only. It is not mounted into the agent container or workspace.
- A Git credential helper reads the Secret files and responds only to matching HTTPS host and repository path requests. Secret values must not be placed in process arguments, environment variables, Git configuration, status fields, logs, or error messages.
- The clone script must not print the repository URL. Credentialed clones disable HTTP redirects. Failure output must redact URL userinfo defensively even though userinfo is rejected at validation.
- Validate `--git-subpath` as a clean, relative path. Reject absolute paths, `..` components, and values that resolve outside `/workspace`.
- Repository contents are untrusted input to the selected agent. Existing Sandbox isolation, runtime class, resource limits, and egress policy apply.

## Failure behavior

If the repository URL or subpath is invalid, reject the invocation before Sandbox creation. The API and operator validate these values; CRD validation also rejects unsafe credentialed URLs for direct Kubernetes clients. If a referenced Secret is missing, Kubernetes cannot mount it and the Pod remains Pending. Creating the Secret lets kubelet retry the volume mount and start the Pod; if the Secret is never created, the Sandbox remains Pending until it is removed. No `git-init` log is available until the Pod starts. If the Secret lacks required keys, the init container fails safely without exposing Secret contents. If DNS, proxy access, authentication, or revision selection fails in the init container, the Sandbox enters its failed state and the CLI reports the Ready condition reason and message. The failed init Pod is retained with the Sandbox for diagnostics. Users can fetch init-container output with `buddy-sbx logs <sandbox-id> --container git-init`. If that log stream is unavailable, the CLI reports the status failure and Sandbox ID. Retain the new clone Sandbox unless the caller explicitly requested `--rm`; with `--rm`, delete it after reporting a startup or execution failure. An interrupted CLI retains the Sandbox unless `--rm` was explicitly requested.

If an existing PVC contains a Git worktree, the current init script may treat it as already initialized. Clone mode rejects `--pvc` to avoid silently using an existing repository or stale revision. Existing `--git-url` plus PVC behavior is not changed by this interface.

## Alternatives considered

- **Clone on the CLI host and upload an archive:** rejected because it transfers repository contents through the CLI host and defeats the remote-workspace workflow.
- **Add a dedicated clone service:** rejected because the existing API, Workspace Git field, init container, egress controls, and status lifecycle already provide the required path.
- **Accept credentials in URL userinfo or flags:** rejected because values can appear in shell history, process arguments, API metadata, and logs.
- **Remove existing `--git-url` and `--git-branch` flags:** rejected to preserve existing scripts and API use. They remain deprecated compatibility flags.

## Acceptance criteria

1. `buddy-sbx run <preset> --clone URL [--branch REF]` selects the existing Sandbox workspace Git configuration and runs the agent only after clone init succeeds.
2. Clone mode never snapshots or synchronizes a local directory.
3. Branch, tag, commit, shallow-depth, default-branch, and subpath behavior are covered by tests.
   When a requested commit is unavailable at the selected shallow depth, initialization fails with the normal Git error and does not silently fetch full history.
4. CLI validation rejects incompatible options before making an API request.
5. Existing Git flags remain functional and preserve their current cleanup behavior.
6. Private HTTPS credentials are available only through a same-namespace Secret bound to the exact host and repository path and mounted read-only in `git-init`; tests verify they are withheld on mismatches and absent from environment values, command arguments, logs, agent containers, API responses, and workspace files.
7. Clone startup failures expose the Sandbox Ready condition reason/message and can be inspected with `buddy-sbx logs --container git-init`. Missing logs do not prevent status reporting. Retain the Sandbox unless `--rm` was explicitly requested; explicit cleanup removes it after failure. An interrupted CLI does not delete the Sandbox unless explicit cleanup was requested.
8. Public CLI help, README, and workspace documentation explain remote clone mode, credential setup, retention, and the fact that changes are not pushed automatically.
9. Quality gates and KinD live checks pass for successful clone, private credential mount, invalid revision, and clone failure cleanup/retention.
