## 项目背景
本项目是java版本的bruce coding agent的go版本移植，目前还未完成。
原Java版本项目位置: /User/zhouzekun/code/bruce-cli

## 开发guideline
需求开发或者bug修复时先新开分支，然后改动，开发完成之后不要提交，等我审阅
重大功能更新要更新minor version,修复bug则要更新patch version

## Code Exploration Policy

Always use jCodeMunch-MCP for code navigation. Never fall back to Read, Grep, Glob, or Bash for code exploration.
**Exception:** use `Read` when you are about to edit a file — the harness requires a `Read` before `Edit`/`Write`. Use jCodeMunch to *find and understand* code, then `Read` only the file you are changing.

This server runs the **front door** surface: three tools reach every jCodeMunch capability, so the tool list stays small and the catalogue is fetched only when you need it.

**Start any session:**
1. `order { "action": "resolve_repo", "args": { "path": "." } }` — confirm the project is indexed. If it is not: `order { "action": "index_folder", "args": { "path": "." } }`

**Then, for any task:**
- Know what you want → `order { "action": "<name>", "args": { ... } }`
- Know the goal, not the tool → `route { "query": "your task in a sentence" }` picks the action and shapes the arguments
- Want to see what exists → `menu { "query": "what you are trying to do" }` returns matching actions with example arguments
- Want the whole catalogue and the usage rules → `jcodemunch_guide`

`menu` and `jcodemunch_guide` list every action this server can run, including ones absent from your tool list. That is expected: the front door is the way to call them.

**Interpreting results:**
- A `verdict` of `no_implementation_found` is evidence of absence. Report the gap; do not re-search with different wording.
- A `verdict` of `degraded` means a channel was unavailable, so absence is NOT proven. Read the note before relying on the result.
- `source: ""` alongside `source_status` means the body could not be read, not that the symbol is empty.

**After editing files:**
- With PostToolUse hooks installed (Claude Code), edited files are reindexed automatically.
- Otherwise `order { "action": "register_edit", "args": { "paths": [...] } }` after an edit, batched for bulk changes.

**Announce your model once per session** so the server can size its answers: `announce_model { "model": "<your-model-id>" }`.

<!-- gitnexus:start -->
# GitNexus — Code Intelligence

This project is indexed by GitNexus as **bruce-go** (3511 symbols, 11245 relationships, 300 execution flows). Use the GitNexus MCP tools to understand code, assess impact, and navigate safely.

> Index stale? Run `node .gitnexus/run.cjs analyze` from the project root — it auto-selects an available runner. No `.gitnexus/run.cjs` yet? `npx gitnexus analyze` (npm 11 crash → `npm i -g gitnexus`; #1939).

## Always Do

- **MUST run impact analysis before editing any symbol.** Before modifying a function, class, or method, run `impact({target: "symbolName", direction: "upstream"})` and report the blast radius (direct callers, affected processes, risk level) to the user.
- **MUST run `detect_changes()` before committing** to verify your changes only affect expected symbols and execution flows. For regression review, compare against the default branch: `detect_changes({scope: "compare", base_ref: "main"})`.
- **MUST warn the user** if impact analysis returns HIGH or CRITICAL risk before proceeding with edits.
- When exploring unfamiliar code, use `query({query: "concept"})` to find execution flows instead of grepping. It returns process-grouped results ranked by relevance.
- When you need full context on a specific symbol — callers, callees, which execution flows it participates in — use `context({name: "symbolName"})`.

## Never Do

- NEVER edit a function, class, or method without first running `impact` on it.
- NEVER ignore HIGH or CRITICAL risk warnings from impact analysis.
- NEVER rename symbols with find-and-replace — use `rename` which understands the call graph.
- NEVER commit changes without running `detect_changes()` to check affected scope.

## Resources

| Resource | Use for |
|----------|---------|
| `gitnexus://repo/bruce-go/context` | Codebase overview, check index freshness |
| `gitnexus://repo/bruce-go/clusters` | All functional areas |
| `gitnexus://repo/bruce-go/processes` | All execution flows |
| `gitnexus://repo/bruce-go/process/{name}` | Step-by-step execution trace |

## CLI

| Task | Read this skill file |
|------|---------------------|
| Understand architecture / "How does X work?" | `.claude/skills/gitnexus/gitnexus-exploring/SKILL.md` |
| Blast radius / "What breaks if I change X?" | `.claude/skills/gitnexus/gitnexus-impact-analysis/SKILL.md` |
| Trace bugs / "Why is X failing?" | `.claude/skills/gitnexus/gitnexus-debugging/SKILL.md` |
| Rename / extract / split / refactor | `.claude/skills/gitnexus/gitnexus-refactoring/SKILL.md` |
| Tools, resources, schema reference | `.claude/skills/gitnexus/gitnexus-guide/SKILL.md` |
| Index, status, clean, wiki CLI commands | `.claude/skills/gitnexus/gitnexus-cli/SKILL.md` |

<!-- gitnexus:end -->
