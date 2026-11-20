# AGENTS.md

EGM artifact directory: .dev/docs

## Workspace Rules
- All scratch concepts briefs, drafts, and Goldfish review reports are stored in `.dev/docs/`.
- Approved design specifications are committed to `docs/designs/`.
- Never use em dashes in markdown documents or code comments.
- Do not use promotional adjectives, buzzwords, or filler phrases.
- Never mention reference third-party projects by name in committed documentation.

## Autonomous Development Loop
The project follows the 9-stage engineering state machine:
RESEARCH -> SPECIFY -> DESIGN REVIEW -> IMPLEMENT -> TEST -> OPEN PR -> CODEX REVIEW -> FIX -> COMPLETE

- State is tracked in `.buddy/checkpoint.json`.
- Configuration is stored in `.buddy/runner-config.json`.
- Autonomous headless execution invokes `agy` CLI:
  `agy --print "<prompt>" --mode accept-edits --output-format json`
- Small, bounded pull requests only.
- Final merge approval requires human sign-off; never auto-merge to main.
