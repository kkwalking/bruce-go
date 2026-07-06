## 1. Runtime Gate

- [x] 1.1 Add a helper that detects `PLAN` mode with a pending/presented active plan and returns the fixed approval prompt for ordinary natural-language input.
- [x] 1.2 Ensure the gated path does not call Planning Agent, LLM, tools, `presentPlan`, or plan store write methods.
- [x] 1.3 Include current plan ID, revision, and path in the prompt if useful, but do not include the full markdown plan body.

## 2. Explicit Command Bypass

- [x] 2.1 Refactor `RunTask` into an internal helper that can optionally allow pending plan input.
- [x] 2.2 Route `/plan continue <feedback>` through the allow-pending path so feedback still reaches Planning Agent.
- [x] 2.3 Keep `/plan approve`, `/plan reject`, and `/plan cancel` behavior unchanged and outside the natural-language gate.

## 3. Tests

- [x] 3.1 Add an integration test where a pending plan exists and ordinary natural language returns the fixed prompt without consuming the next fake LLM response.
- [x] 3.2 Assert the gated response does not contain the full plan body and does not append extra `plan_event(presented)` or `plan_event(updated)` entries.
- [x] 3.3 Add an integration test proving `/plan continue <feedback>` still calls Planning Agent and can update the plan.
- [x] 3.4 Add an integration test proving `/plan approve` still bypasses the gate and hands off to ReAct.

## 4. Verification

- [x] 4.1 Run focused tests for `internal/integrated`.
- [x] 4.2 Run `go test ./...`.
- [x] 4.3 Run `openspec validate gate-pending-plan-natural-input --strict`.
