## 1. Runtime Event Plumbing

- [x] 1.1 Add a typed plan lifecycle runtime event carrying `runtime.PlanEvent`.
- [x] 1.2 Emit the typed event only after `presentPlan()` successfully records `plan_event(presented)`.
- [x] 1.3 Ensure created/updated plan tool events remain durable session entries but do not trigger full-plan TUI rendering.

## 2. Session Replay Data

- [x] 2.1 Expose active path entries to UI replay without adding `plan_event` content to LLM history messages.
- [x] 2.2 Update `/resume` and session changed event payloads so TUI can replay messages and presented plan entries in timeline order.
- [x] 2.3 Preserve existing `/session`, `/sessions`, `/tree`, and `/status` behavior for plan state display.

## 3. TUI Plan Rendering

- [x] 3.1 Add a TUI message kind or equivalent render path for presented plan markdown content.
- [x] 3.2 Render `plan_event(presented).content` from the typed runtime event during live Plan Mode runs.
- [x] 3.3 Render `plan_event(presented).content` during session replay/resume using active path order.
- [x] 3.4 Keep assistant final messages as ordinary assistant output; do not add heuristic folding or deletion logic.

## 4. Planning Prompt

- [x] 4.1 Update Plan Mode prompt to remove the instruction to show plan key content in the final reply.
- [x] 4.2 Add prompt guidance that after `replace_plan` or `edit_plan`, the model should only return a short status such as “计划创建完成，请审阅。” or “计划已更新，请审阅。”
- [x] 4.3 Add prompt guidance that the model must not repeat, summarize, rewrite, or excerpt the markdown plan body because the system will display it from the plan event.

## 5. Tests

- [x] 5.1 Add runtime tests proving a presented plan event is emitted after successful `plan_event(presented)` recording.
- [x] 5.2 Add TUI tests proving live plan rendering uses `plan_event(presented).content` and preserves full markdown text.
- [x] 5.3 Add session resume/replay tests proving presented plan entries render in active path order without entering LLM history messages.
- [x] 5.4 Add prompt tests or assertions covering the concise final-response guidance.
- [x] 5.5 Run the relevant Go test packages for `internal/event`, `internal/session`, `internal/integrated`, `internal/tui`, and `internal/planning`.
