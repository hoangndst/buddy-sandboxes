# Autonomous Development Loop Specification

## 1. Overview and Purpose

This document defines the automated engineering process used by autonomous agents and developers working on Buddy Sandboxes. The process enforces reproducible quality gates, bounded feature delivery, automated PR reviews, and deterministic checkpoint persistence across process restarts.

The state machine follows nine discrete stages:

```
RESEARCH -> SPECIFY -> DESIGN REVIEW -> IMPLEMENT -> TEST -> OPEN PR -> CODEX REVIEW -> FIX -> COMPLETE
```

## 2. Location of Components and Rules

| Component | Location | Description |
| :--- | :--- | :--- |
| Runner Script | [scripts/autonomous-runner.py](file:///home/hoangndst/Server/buddy-sandboxes/scripts/autonomous-runner.py) | Python automation runner managing stage transitions and tool execution. |
| Configuration | [.buddy/runner-config.json](file:///home/hoangndst/Server/buddy-sandboxes/.buddy/runner-config.json) | Task definitions, branch mappings, quality gate commands, and polling limits. |
| Checkpoint State | [.buddy/checkpoint.json](file:///home/hoangndst/Server/buddy-sandboxes/.buddy/checkpoint.json) | Persistent JSON state recording active task, current stage, PR info, and history. |
| CI Definition | [.github/workflows/ci.yml](file:///home/hoangndst/Server/buddy-sandboxes/.github/workflows/ci.yml) | GitHub Actions workflow executing manifests check, linter, tests, and build. |

## 3. Stage Transition Rules

### Stage 1: RESEARCH
- Objective: Investigate requirements, upstream code patterns, and constraints.
- Exit Criteria: Concrete problem statement and component boundaries documented.

### Stage 2: SPECIFY
- Objective: Draft or update architecture specifications in `docs/architecture.md`.
- Exit Criteria: CRD schemas, security profiles, storage modes, and failure modes explicitly written.

### Stage 3: DESIGN REVIEW
- Objective: Conduct multi-perspective reviews (via EGM reviewer personas).
- Exit Criteria: All critical and blocker review findings resolved and incorporated into the specification.

### Stage 4: IMPLEMENT
- Objective: Create a bounded feature branch (e.g. `feat/<feature-name>`) and implement changes.
- Invariant: Scope must remain small and focused on a single milestone objective.

### Stage 5: TEST
- Objective: Execute all local quality gates before opening a pull request.
- Required Gates:
  1. `make manifests generate` (verifying git tree remains clean).
  2. `make fmt vet` (formatting and go vet analysis).
  3. `make test` (controller envtest suite with coverage).
  4. `go test -race ./...` (concurrency race condition detection).
- Failure Rule: On gate failure, increment retry counter. If retries exceed limit (default 3), mark task as `BLOCKED`.

### Stage 6: OPEN PR
- Objective: Push feature branch to origin and open a GitHub pull request.
- Template Requirements: Overview, list of changes, link to architecture doc, verification steps.
- Invariant: Never push directly to `main`.

### Stage 7: CODEX REVIEW
- Objective: Request automated review on the PR by posting `@codex review`.
- Polling Rule: Poll PR review comments every 30 seconds (up to 600s timeout).
- Transition Rules:
  - If review completes with zero actionable findings: Transition to `COMPLETE`.
  - If review reports actionable findings: Transition to `FIX`.
  - If review times out or errors: Mark task as `BLOCKED` for human triage.

### Stage 8: FIX
- Objective: Investigate each actionable finding, implement targeted remediation, and add regression tests.
- Re-Test Rule: Transition back to `TEST` to verify all quality gates before pushing updates.

### Stage 9: COMPLETE
- Objective: Mark task complete in `.buddy/checkpoint.json`.
- Human Merge Gate: Autonomous agents are strictly forbidden from auto-merging PRs into `main`. The repository maintainer provides final merge approval.

## 4. CLI Execution Reference

Check the current state of the development runner:

```bash
./scripts/autonomous-runner.py --status
```

Advance one step in the state machine:

```bash
./scripts/autonomous-runner.py --step
```

Reset the local checkpoint state:

```bash
./scripts/autonomous-runner.py --reset
```
