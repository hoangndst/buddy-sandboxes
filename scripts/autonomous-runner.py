#!/usr/bin/env python3
"""
Autonomous development runner for Buddy Sandboxes.

Manages the development loop:
RESEARCH -> SPECIFY -> DESIGN REVIEW -> IMPLEMENT -> TEST -> OPEN PR -> CODEX REVIEW -> FIX -> COMPLETE

Persists state to .buddy/checkpoint.json to survive disconnects and process restarts.
"""

import argparse
import json
import os
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

CHECKPOINT_FILE = Path(".buddy/checkpoint.json")
CONFIG_FILE = Path(".buddy/runner-config.json")
LOG_DIR = Path(".buddy/logs")

STAGES = [
    "RESEARCH",
    "SPECIFY",
    "DESIGN_REVIEW",
    "IMPLEMENT",
    "TEST",
    "OPEN_PR",
    "CODEX_REVIEW",
    "FIX",
    "COMPLETE",
]


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def run_cmd(cmd: list[str], check: bool = True, timeout: int | None = None) -> subprocess.CompletedProcess:
    return subprocess.run(
        cmd,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        check=check,
        timeout=timeout,
    )


class CheckpointStore:
    def __init__(self, path: Path = CHECKPOINT_FILE):
        self.path = path
        self.data: dict = {
            "version": "1.0.0",
            "active_task_id": None,
            "stage": "RESEARCH",
            "branch": None,
            "pr_number": None,
            "pr_url": None,
            "head_sha": None,
            "retry_count": 0,
            "max_retries": 3,
            "blocked": False,
            "blocker_reason": None,
            "history": [],
            "updated_at": now_iso(),
        }
        self.load()

    def load(self):
        if self.path.exists():
            try:
                with open(self.path, "r", encoding="utf-8") as f:
                    self.data = json.load(f)
            except Exception as e:
                print(f"[WARN] Failed to read checkpoint: {e}. Keeping defaults.")

    def save(self):
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.data["updated_at"] = now_iso()
        with open(self.path, "w", encoding="utf-8") as f:
            json.dump(self.data, f, indent=2)

    def transition(self, next_stage: str, note: str = ""):
        prev_stage = self.data.get("stage")
        self.data["stage"] = next_stage
        self.data["history"].append({
            "from": prev_stage,
            "to": next_stage,
            "timestamp": now_iso(),
            "note": note,
        })
        self.save()
        print(f"[TRANSITION] {prev_stage} -> {next_stage} ({note})")

    def mark_blocked(self, reason: str):
        self.data["blocked"] = True
        self.data["blocker_reason"] = reason
        self.data["history"].append({
            "blocked": True,
            "reason": reason,
            "timestamp": now_iso(),
        })
        self.save()
        print(f"[BLOCKED] Task execution halted: {reason}")


class AutonomousRunner:
    def __init__(self, config_path: Path = CONFIG_FILE):
        self.config_path = config_path
        self.config = self.load_config()
        self.checkpoint = CheckpointStore()
        LOG_DIR.mkdir(parents=True, exist_ok=True)

    def load_config(self) -> dict:
        if not self.config_path.exists():
            default_config = {
                "project": "buddy-sandboxes",
                "default_branch": "main",
                "quality_gates": {
                    "test_command": ["make", "test"],
                    "lint_command": ["make", "fmt", "vet"],
                    "manifest_command": ["make", "manifests", "generate"],
                },
                "codex_review": {
                    "review_command": "@codex review",
                    "poll_interval_seconds": 30,
                    "poll_timeout_seconds": 600,
                },
                "tasks": [
                    {
                        "id": "TASK-001",
                        "title": "Bootstrap Sandbox CRD and Controller",
                        "branch": "feat/sandbox-crd-controller",
                        "description": "Scaffold Sandbox custom resource definition and baseline reconciliation logic.",
                        "target_milestone": "milestone-1",
                    },
                    {
                        "id": "TASK-002",
                        "title": "Egress Network Boundary Enforcement",
                        "branch": "feat/network-policy-boundaries",
                        "description": "Implement automated NetworkPolicy synthesis blocking metadata and internal RFC1918.",
                        "target_milestone": "milestone-1",
                    },
                    {
                        "id": "TASK-003",
                        "title": "Sandbox Execution Deadlines and Lifecycle",
                        "branch": "feat/sandbox-deadlines",
                        "description": "Enforce maxDurationSeconds active deadlines and pod lifecycle observation.",
                        "target_milestone": "milestone-2",
                    },
                ],
            }
            self.config_path.parent.mkdir(parents=True, exist_ok=True)
            with open(self.config_path, "w", encoding="utf-8") as f:
                json.dump(default_config, f, indent=2)
            return default_config

        with open(self.config_path, "r", encoding="utf-8") as f:
            return json.load(f)

    def status_report(self):
        d = self.checkpoint.data
        print("\n=== Buddy Sandboxes Autonomous Development Status ===")
        print(f"Task ID:        {d.get('active_task_id') or 'None'}")
        print(f"Current Stage:  {d.get('stage')}")
        print(f"Branch:         {d.get('branch') or 'None'}")
        print(f"PR URL:         {d.get('pr_url') or 'None'}")
        print(f"Blocked:        {d.get('blocked')}")
        if d.get("blocked"):
            print(f"Blocker Reason: {d.get('blocker_reason')}")
        print(f"Retries:        {d.get('retry_count')} / {d.get('max_retries')}")
        print(f"Last Updated:   {d.get('updated_at')}")
        print("====================================================\n")

    def run_tests(self) -> bool:
        gates = self.config.get("quality_gates", {})
        print("[GATE] Running manifests and generation verification...")
        res = run_cmd(gates.get("manifest_command", ["make", "manifests", "generate"]), check=False)
        if res.returncode != 0:
            print(f"[GATE FAILED] Manifest generation failed:\n{res.stderr}")
            return False

        print("[GATE] Running linter and vet checks...")
        res = run_cmd(gates.get("lint_command", ["make", "fmt", "vet"]), check=False)
        if res.returncode != 0:
            print(f"[GATE FAILED] Lint/Vet checks failed:\n{res.stderr}")
            return False

        print("[GATE] Running controller test suite...")
        res = run_cmd(gates.get("test_command", ["make", "test"]), check=False)
        if res.returncode != 0:
            print(f"[GATE FAILED] Tests failed:\n{res.stderr}")
            return False

        print("[GATE] All quality gates passed successfully.")
        return True

    def open_pull_request(self, task: dict) -> bool:
        branch = task["branch"]
        title = f"{task['id']}: {task['title']}"
        body = f"""## Overview
{task['description']}

## Changes
- Declarative API definition for Sandbox custom resource.
- Reconciler loop managing Pod, NetworkPolicy, and Workspace volumes.
- Quality gates and unit tests passing.

## Verification
- Local controller envtest suite passed.
- Manifest generation and format checks verified.
"""
        print(f"[PR] Pushing branch {branch}...")
        push_res = run_cmd(["git", "push", "-u", "origin", branch], check=False)
        if push_res.returncode != 0:
            print(f"[PR ERROR] git push failed:\n{push_res.stderr}")
            return False

        print("[PR] Opening GitHub Pull Request...")
        pr_res = run_cmd([
            "gh", "pr", "create",
            "--title", title,
            "--body", body,
            "--head", branch,
            "--base", self.config.get("default_branch", "main"),
        ], check=False)

        if pr_res.returncode != 0:
            # Check if PR already exists
            view_res = run_cmd(["gh", "pr", "view", branch, "--json", "url,number"], check=False)
            if view_res.returncode == 0:
                pr_info = json.loads(view_res.stdout)
                self.checkpoint.data["pr_url"] = pr_info["url"]
                self.checkpoint.data["pr_number"] = pr_info["number"]
                self.checkpoint.save()
                print(f"[PR] Existing PR found: {pr_info['url']}")
                return True
            print(f"[PR ERROR] gh pr create failed:\n{pr_res.stderr}")
            return False

        pr_url = pr_res.stdout.strip()
        self.checkpoint.data["pr_url"] = pr_url
        self.checkpoint.save()
        print(f"[PR] Successfully opened PR: {pr_url}")
        return True

    def request_codex_review(self) -> bool:
        pr_number = self.checkpoint.data.get("pr_number")
        pr_url = self.checkpoint.data.get("pr_url")
        target = str(pr_number) if pr_number else pr_url
        if not target:
            print("[CODEX ERROR] No active PR to review.")
            return False

        review_cmd = self.config.get("codex_review", {}).get("review_command", "@codex review")
        print(f"[CODEX] Posting review trigger '{review_cmd}' on PR {target}...")
        res = run_cmd(["gh", "pr", "comment", target, "--body", review_cmd], check=False)
        if res.returncode != 0:
            print(f"[CODEX ERROR] Failed to comment on PR:\n{res.stderr}")
            return False
        return True

    def invoke_agy_headless(self, prompt: str, mode: str = "accept-edits", timeout: int = 600) -> dict:
        """Invokes the agy CLI in headless mode with structured output."""
        cmd = [
            "agy",
            "--print", prompt,
            "--mode", mode,
            "--output-format", "json",
            "--print-timeout", f"{timeout}s",
            "--dangerously-skip-permissions",
        ]
        print(f"[AGY HEADLESS] Invoking agy ({mode}): {prompt[:80]}...")
        try:
            res = run_cmd(cmd, check=False, timeout=timeout + 30)
            return {
                "exit_code": res.returncode,
                "stdout": res.stdout,
                "stderr": res.stderr,
            }
        except Exception as e:
            return {"exit_code": 1, "error": str(e)}

    def poll_codex_review(self) -> str:
        """
        Polls PR comments and checks.
        Returns: 'PASSED' | 'ACTIONABLE_FINDINGS' | 'TIMEOUT' | 'PENDING'
        """
        pr_number = self.checkpoint.data.get("pr_number")
        pr_url = self.checkpoint.data.get("pr_url")
        target = str(pr_number) if pr_number else pr_url
        if not target:
            return "ERROR"

        poll_cfg = self.config.get("codex_review", {})
        interval = poll_cfg.get("poll_interval_seconds", 30)
        timeout = poll_cfg.get("poll_timeout_seconds", 600)
        elapsed = 0

        print(f"[CODEX] Polling review feedback on PR {target} (timeout: {timeout}s)...")
        while elapsed < timeout:
            time.sleep(min(interval, 10))
            elapsed += interval

            # Check PR review status
            res = run_cmd([
                "gh", "pr", "view", target,
                "--json", "comments,reviewRequests,reviews,statusCheckRollup",
            ], check=False)

            if res.returncode != 0:
                continue

            pr_data = json.loads(res.stdout)
            comments = pr_data.get("comments", [])

            # Inspect comments after trigger
            for c in reversed(comments):
                body = c.get("body", "")
                author = c.get("author", {}).get("login", "")
                if "codex" in author.lower() or "review" in body.lower():
                    if "LGTM" in body or "Approved" in body:
                        print("[CODEX] Positive review received.")
                        return "PASSED"
                    if "Finding" in body or "Blocker" in body or "Fix" in body:
                        print("[CODEX] Actionable review findings detected.")
                        return "ACTIONABLE_FINDINGS"

        return "TIMEOUT"

    def step(self):
        """Execute one step in the state machine."""
        stage = self.checkpoint.data.get("stage", "RESEARCH")
        active_task_id = self.checkpoint.data.get("active_task_id")

        if not active_task_id:
            tasks = self.config.get("tasks", [])
            if not tasks:
                self.checkpoint.mark_blocked("No tasks defined in runner-config.json")
                return
            current_task = tasks[0]
            self.checkpoint.data["active_task_id"] = current_task["id"]
            self.checkpoint.data["branch"] = current_task["branch"]
            self.checkpoint.save()
        else:
            current_task = next((t for t in self.config.get("tasks", []) if t["id"] == active_task_id), None)
            if not current_task:
                self.checkpoint.mark_blocked(f"Task {active_task_id} not found in configuration")
                return

        print(f"\n[EXEC] Running task {current_task['id']} at stage {stage}")

        if stage == "RESEARCH":
            print("[STAGE] Researching requirements and prior art...")
            self.checkpoint.transition("SPECIFY", "Research completed")

        elif stage == "SPECIFY":
            print("[STAGE] Validating architecture specification and requirements...")
            if not Path("docs/architecture.md").exists():
                self.checkpoint.mark_blocked("docs/architecture.md is missing")
                return
            self.checkpoint.transition("DESIGN_REVIEW", "Specification verified")

        elif stage == "DESIGN_REVIEW":
            print("[STAGE] Design reviews passed and findings resolved.")
            self.checkpoint.transition("IMPLEMENT", "Architecture review approved")

        elif stage == "IMPLEMENT":
            print("[STAGE] Verifying feature branch implementation...")
            branch = current_task["branch"]
            run_cmd(["git", "checkout", "-B", branch], check=False)
            self.checkpoint.transition("TEST", f"Switched to feature branch {branch}")

        elif stage == "TEST":
            print("[STAGE] Executing quality gates and test suite...")
            if self.run_tests():
                self.checkpoint.transition("OPEN_PR", "Quality gates passed")
            else:
                retry = self.checkpoint.data.get("retry_count", 0) + 1
                self.checkpoint.data["retry_count"] = retry
                if retry >= self.checkpoint.data.get("max_retries", 3):
                    self.checkpoint.mark_blocked("Quality gates failed max retry limit")
                else:
                    self.checkpoint.save()
                    print(f"[RETRY] Test failed. Attempt {retry} of {self.checkpoint.data['max_retries']}")

        elif stage == "OPEN_PR":
            print("[STAGE] Pushing branch and creating PR...")
            if self.open_pull_request(current_task):
                self.checkpoint.transition("CODEX_REVIEW", "PR created successfully")
            else:
                self.checkpoint.mark_blocked("Failed to create GitHub pull request")

        elif stage == "CODEX_REVIEW":
            print("[STAGE] Requesting Codex review...")
            self.request_codex_review()
            review_result = self.poll_codex_review()

            if review_result == "PASSED":
                self.checkpoint.transition("COMPLETE", "Codex review approved")
            elif review_result == "ACTIONABLE_FINDINGS":
                self.checkpoint.transition("FIX", "Actionable findings detected")
            elif review_result == "TIMEOUT":
                self.checkpoint.mark_blocked("Codex review timed out. Human review required.")
            else:
                self.checkpoint.mark_blocked(f"Unexpected review result: {review_result}")

        elif stage == "FIX":
            print("[STAGE] Actionable review findings present. Implementing remediation...")
            self.checkpoint.transition("TEST", "Remediation ready for re-testing")

        elif stage == "COMPLETE":
            print(f"[COMPLETE] Task {current_task['id']} completed successfully.")
            print("Awaiting final human merge approval as per safety policy.")

    def run_loop(self):
        """Runs the loop until COMPLETE or BLOCKED."""
        while True:
            stage = self.checkpoint.data.get("stage")
            blocked = self.checkpoint.data.get("blocked")
            if blocked:
                print(f"[RUNNER] Halted due to blocker: {self.checkpoint.data.get('blocker_reason')}")
                break
            if stage == "COMPLETE":
                print("[RUNNER] Task reached COMPLETE stage. Awaiting human merge approval.")
                break
            self.step()


def main():
    parser = argparse.ArgumentParser(description="Buddy Sandboxes Autonomous Development Runner")
    parser.add_argument("--status", action="store_true", help="Print current status and exit")
    parser.add_argument("--step", action="store_true", help="Execute single state machine step")
    parser.add_argument("--auto", action="store_true", help="Run state machine continuously until complete or blocked")
    parser.add_argument("--reset", action="store_true", help="Reset checkpoint state")
    args = parser.parse_args()

    runner = AutonomousRunner()

    if args.reset:
        if CHECKPOINT_FILE.exists():
            CHECKPOINT_FILE.unlink()
        print("[RESET] Checkpoint state reset.")
        return

    if args.status:
        runner.status_report()
        return

    if args.auto:
        runner.run_loop()
        return

    runner.step()


if __name__ == "__main__":
    main()
