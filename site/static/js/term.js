(function () {
  var root = document.getElementById("term");
  if (!root) return;

  var code = root.querySelector("code");
  if (!code) return;

  var reduce =
    window.matchMedia &&
    window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  if (reduce) return;

  var steps = [
    {
      cmd: "buddy-sbx create secret openai --rpm 60 --max-concurrent 4",
      out: [
        "Enter OpenAI API key: ••••••••••••••••",
        "Stored openai API key for your account.",
      ],
    },
    {
      cmd: "buddy-sbx create secret anthropic",
      out: [
        "Enter Anthropic API key: ••••••••••••••••",
        "Stored anthropic API key for your account.",
      ],
    },
    {
      cmd: "buddy-sbx get secrets",
      out: [
        "SERVICE    SCOPE  SANDBOX  REPOSITORY  BASE_URL  MODEL  UPDATED",
        "openai     user   -        -           -         -      2026-11-25 17:30:00 UTC",
        "anthropic  user   -        -           -         -      2026-11-25 17:31:00 UTC",
      ],
    },
    {
      cmd: 'buddy-sbx run codex . -- "Fix unit tests in pkg/auth"',
      out: [
        "Project snapshot: 128 files, 140 entries, 84210 bytes",
        "Excluded paths: none",
        "✓ Workspace synchronized:",
        "  M pkg/auth/token.go",
        "  A pkg/auth/token_test.go",
      ],
    },
    {
      cmd: 'buddy-sbx run claude . -- "Summarize the open pull requests"',
      out: [
        "Project snapshot: 128 files, 140 entries, 84210 bytes",
        "Excluded paths: none",
        "✓ Workspace synchronized (no files changed).",
      ],
    },
    {
      cmd: "buddy-sbx run codex -d -n task-runner --pvc workspace",
      out: ["task-runner"],
    },
    {
      cmd: "buddy-sbx get sbx",
      out: [
        "NAME         IMAGE                                            PHASE    AGE  NETWORK  MCP  LABELS",
        "task-runner  ghcr.io/hoangndst/buddy-sandboxes-codex:0.171.0  Running  42s  Web      -    -",
      ],
    },
    {
      cmd: "buddy-sbx exec task-runner -- ps",
      out: ["  PID CMD", "    1 tini", "   12 /bin/bash"],
    },
    {
      cmd: "buddy-sbx suspend task-runner",
      out: ["task-runner Suspended"],
    },
    {
      cmd: "buddy-sbx resume task-runner",
      out: ["task-runner None"],
    },
    {
      cmd: "buddy-sbx delete sbx task-runner",
      out: ['sandbox "task-runner" deleted'],
    },
  ];

  var cursor = null;
  var paused = false;
  var waiters = [];
  var toggle = document.getElementById("term-toggle");

  function whenReady() {
    if (!paused) return Promise.resolve();
    return new Promise(function (resolve) {
      waiters.push(resolve);
    });
  }

  function resumeWaiters() {
    while (waiters.length) waiters.shift()();
  }

  if (toggle) {
    toggle.addEventListener("click", function () {
      paused = !paused;
      toggle.setAttribute("aria-pressed", paused ? "true" : "false");
      toggle.setAttribute("aria-label", paused ? "Play demo" : "Pause demo");
      toggle.textContent = paused ? "Play" : "Pause";
      if (!paused) resumeWaiters();
    });
  }

  function escapeHtml(text) {
    return text
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;");
  }

  function sleep(ms) {
    return whenReady().then(function () {
      return new Promise(function (resolve) {
        setTimeout(resolve, ms);
      });
    });
  }

  function ensureCursor() {
    if (!cursor) {
      cursor = document.createElement("span");
      cursor.className = "term-cursor";
      cursor.setAttribute("aria-hidden", "true");
    }
    code.appendChild(cursor);
  }

  function writeLine(cls, text) {
    var span = document.createElement("span");
    span.className = cls;
    span.innerHTML = escapeHtml(text);
    code.appendChild(span);
    code.appendChild(document.createTextNode("\n"));
    ensureCursor();
    root.scrollTop = root.scrollHeight;
  }

  function typeCommand(cmd) {
    var prompt = document.createElement("span");
    prompt.className = "term-prompt";
    prompt.textContent = "$";
    code.appendChild(prompt);
    code.appendChild(document.createTextNode(" "));
    var cmdSpan = document.createElement("span");
    cmdSpan.className = "term-cmd";
    code.appendChild(cmdSpan);
    ensureCursor();

    var i = 0;
    return new Promise(function (resolve) {
      function tick() {
        whenReady().then(function () {
          if (i >= cmd.length) {
            code.appendChild(document.createTextNode("\n"));
            ensureCursor();
            resolve();
            return;
          }
          cmdSpan.textContent += cmd.charAt(i);
          i += 1;
          root.scrollTop = root.scrollHeight;
          setTimeout(tick, 18 + Math.random() * 28);
        });
      }
      tick();
    });
  }

  function runStep(step) {
    return typeCommand(step.cmd).then(function () {
      var chain = Promise.resolve();
      (step.out || []).forEach(function (line) {
        chain = chain.then(function () {
          return sleep(40).then(function () {
            writeLine("term-out", line);
          });
        });
      });
      return chain.then(function () {
        return sleep(280);
      });
    });
  }

  function loop(i) {
    if (i >= steps.length) {
      return sleep(1200).then(function () {
        code.textContent = "";
        cursor = null;
        return loop(0);
      });
    }
    return runStep(steps[i]).then(function () {
      return loop(i + 1);
    });
  }

  code.textContent = "";
  loop(0);
})();
