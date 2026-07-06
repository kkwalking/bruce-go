## 1. Plan State And Persistence

- [x] 1.1 Add plan data models for plan ID, path, revision, sha256, action, content snapshot, and pending/approved/rejected/canceled state.
- [x] 1.2 Add a plan store rooted at `~/.bruce/plans/` with safe path resolution, symlink escape prevention, atomic writes, revision increments, and sha256 calculation.
- [x] 1.3 Add session support for `plan_event` entries, including append helpers, active-path replay, tree labels, and JSONL round-trip tests.
- [x] 1.4 Extend runtime/session context with derived active plan state without making `plan_event` the source of current mode.

## 2. Plan Tools And Permission Guard

- [x] 2.1 Implement plan-only tools such as `read_plan`, `replace_plan`, and `edit_plan` that operate only on the current active plan.
- [x] 2.2 Make successful plan tool writes append `plan_event(updated)` with plan ID, action, revision, sha256, path, summary, and snapshot policy.
- [x] 2.3 Add a plan-mode tool registry view that exposes only read-only exploration tools and plan tools.
- [x] 2.4 Add a plan-mode command guard that allows conservative read-only commands and rejects mutating or ambiguous commands with clear feedback.
- [x] 2.5 Add tests proving project writes, mutating commands, and write-capable MCP tools are unavailable or rejected in plan mode.

## 3. Planning Agent Runtime

- [x] 3.1 Add a Planning Agent prompt and runtime path that researches tasks, maintains markdown plans, and presents plans without executing project changes.
- [x] 3.2 Change `Runtime.RunTask` in `PLAN` mode to use Planning Agent instead of the old DAG Plan-and-Execute agent.
- [x] 3.3 Support `/plan <description>` by switching to plan mode and immediately running planning for the provided description.
- [x] 3.4 Ensure presented plans append `plan_event(presented)` with enough content to recover from session JSONL alone.

## 4. Plan Review Commands And Handoff

- [x] 4.1 Implement `/plan approve`, `/plan reject [reason]`, `/plan cancel`, and `/plan continue [feedback]`.
- [x] 4.2 On approval, append `plan_event(approved)`, switch mode to `REACT`, and inject the approved markdown plan into the ReAct execution context.
- [x] 4.3 On reject or cancel, append `plan_event(rejected)` or `plan_event(canceled)` and do not execute project changes.
- [x] 4.4 On continue, keep `PLAN` mode and route feedback back to Planning Agent for plan updates.
- [x] 4.5 Preserve existing HITL behavior for concrete execution after plan approval.

## 5. Resume And User-Visible Rendering

- [x] 5.1 Update `/resume` reconstruction to restore both mode from `mode_change` and active plan state from `plan_event`.
- [x] 5.2 Detect missing or hash-mismatched plan files and recover from session snapshots when possible.
- [x] 5.3 Update `/status` to show pending plan ID, revision, and path.
- [x] 5.4 Update `/session` and `/tree` rendering to show plan lifecycle events with concise labels.
- [x] 5.5 Update TUI activity and completion hints for plan review commands.

## 6. Legacy Plan-And-Execute Cleanup

- [x] 6.1 Remove user-facing references to Plan-and-Execute from CLI help, README, docs, and tests.
- [x] 6.2 Decide whether to delete `internal/plan` or keep it internal-only with no `/plan` references.
- [x] 6.3 Update migration notes to document that Go `/plan` now follows Claude Code-style planning semantics.

## 7. Verification

- [x] 7.1 Add integration tests for `/plan`, `/plan <description>`, plan creation, plan update, plan presentation, approval, rejection, cancellation, and continue planning.
- [x] 7.2 Add resume tests for pending, approved, rejected, canceled, missing-file, and hash-mismatch plan states.
- [x] 7.3 Add session JSONL tests proving `mode_change` determines mode independently from `plan_event`.
- [x] 7.4 Run `go test ./...` and `openspec validate align-plan-mode-with-claude-code --strict`.
