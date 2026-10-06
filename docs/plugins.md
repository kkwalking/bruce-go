# Bruce Go JavaScript Plugins

Bruce Go can be extended with JavaScript plugins. A plugin is a directory with a
manifest and a JavaScript ES module; it can contribute tools, hooks and slash
commands, and it can use a small set of host capabilities the user explicitly
grants.

This document is the plugin author's reference. The design rationale, the
module layout and the security model are in
[plugin-architecture.md](plugin-architecture.md).

## What a plugin is

A plugin is:

- a **directory** under a plugin search root,
- containing a **`plugin.json` manifest** that declares what the plugin offers
  and what it needs,
- and a **JavaScript entry module** that exports the handler functions the
  manifest names.

A plugin is not a separate agent runtime. The tools it defines are registered
into Bruce's existing tool registry and then go through exactly the same
scheduling, concurrency control, sandbox, approval, network policy,
cancellation and event logging as a built-in tool. The agent cannot tell where
a tool came from, and it does not need to.

```text
<workspace>/.bruce/plugins/<name>/plugin.json     workspace plugin
<workspace>/.bruce/plugins/<name>/index.js
<home>/.bruce/plugins/<name>/plugin.json          user plugin
<home>/.bruce/plugins/<name>/index.js
```

`<name>.json` is also accepted as a single-file manifest next to its module.

### Plugin directories

| Root | Path | Scope |
|---|---|---|
| Workspace | `<workspace>/.bruce/plugins/` | This project only |
| User | `~/.bruce/plugins/` | Every project |

**Precedence: a workspace plugin overrides a user plugin with the same name.**
The override is listed by `/plugin`. Within one root, plugins load in name
order, so the result never depends on directory iteration order.

Two plugins may not declare the same tool name or the same command name. The
first declarer (by plugin name) keeps it; the loser's declaration is dropped and
reported as a diagnostic. Nothing is silently overwritten.

## Manifest format

`plugin.json` is JSON with camelCase field names. Unknown field names are
rejected — a typo must not silently disable a setting you thought you had
configured.

```json
{
  "apiVersion": "bruce.plugin/v1",
  "name": "todo-tracker",
  "version": "1.0.0",
  "description": "Tracks TODOs found in the workspace",
  "entry": "index.js",
  "permissions": ["fs.read"],
  "concurrency": { "maxRuntimes": 2, "parallelSafe": true },
  "tools": [
    {
      "name": "todo_scan",
      "description": "Scan the workspace for TODO comments",
      "handler": "scan",
      "promptSnippet": "Find TODO comments in the workspace",
      "schema": {
        "type": "object",
        "properties": {
          "path": { "type": "string" },
          "options": {
            "type": "object",
            "properties": {
              "recursive": { "type": "boolean" },
              "depth": { "type": "integer" }
            }
          },
          "extensions": { "type": "array", "items": { "type": "string" } }
        },
        "required": ["path"]
      }
    }
  ],
  "hooks": [
    { "event": "tool.before", "handler": "guard", "timeoutMs": 2000 }
  ],
  "commands": [
    { "name": "todo-report", "description": "Print the TODO report", "handler": "report", "usage": "/todo-report [path]" }
  ],
  "metadata": { "homepage": "https://example.com" }
}
```

### Fields

| Field | Required | Meaning |
|---|---|---|
| `apiVersion` | yes | Must be `bruce.plugin/v1`. |
| `name` | yes | `^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`, at most 64 characters. |
| `version` | yes | `1.2.3`, optionally with `-prerelease` or `+build`. |
| `description` | yes | Shown by `/plugin`. |
| `entry` | yes | Path to the JavaScript module, relative to the plugin directory. Must stay inside it. |
| `permissions` | no | Plugin-wide permission requests. See [Permission model](#permission-model). |
| `concurrency` | no | `maxRuntimes` (0–64, default 4) and `parallelSafe` (default false). |
| `tools` | no | Tool declarations. |
| `hooks` | no | Hook declarations. |
| `commands` | no | Slash command declarations. |
| `metadata` | no | Free-form string map. Keys are yours; values must be strings. |

A tool declaration needs `name`, `description` and `handler`. `schema` defaults
to an empty object schema. `permissions` on a tool narrow the plugin-wide set
for that tool; a tool with no `permissions` inherits the plugin-wide ones.
`parallelSafe` and `timeoutMs` may be set per tool.

A command name may not collide with a built-in command such as `/sandbox`,
`/hitl` or `/plugin`. A tool name may not collide with a built-in tool such as
`read_file` or `execute_command`. Both are rejected at load time.

## JavaScript entry

The entry is an ES module. Handlers are **named exports**, and the manifest
points at them by name:

```js
// index.js
import { get, set } from "bruce:storage";
import { info } from "bruce:api";

export function scan(input) {
  // input is the tool arguments object, exactly as the model produced it.
  const { path, options = {}, extensions = [] } = input;
  const recursive = options.recursive === true;
  const depth = options.depth ?? 3;
  set({ scope: "plugin", key: "lastPath", value: path });
  return { path, recursive, depth, extensions, host: info().version };
}

export function guard(input) {
  // Return { block: true, reason } to refuse a tool call.
  if (input.tool === "execute_command" && /rm -rf/.test(input.args.command ?? "")) {
    return { block: true, reason: "destructive command" };
  }
  return { args: input.args };
}

export function report() {
  return { output: "TODO report: " + (get({ scope: "plugin", key: "lastPath" }).value ?? "none") };
}
```

Handlers may be nested: `"handler": "tools.read"` resolves the `read` property
of the exported `tools` object.

### Why named exports and not `registerTool()`

Handlers are resolved from static metadata **before any JavaScript runs**. That
is what makes the plugin's shape knowable at load time: a missing handler is a
startup error with a file and a line, not a surprise on the first call. It also
lets several runtimes serve one plugin without sharing closures, and makes
reload a matter of recompiling a module rather than tearing down live state.

### Tool arguments and results

Arguments arrive as one object. The standard JSON data model is preserved
exactly: nested objects, arrays, booleans, numbers, `null` and strings all
survive without stringification or flattening.

```js
export function echo(input) {
  return input;                 // structure and types come back unchanged
}
export function text(input) {
  return "a plain string is returned to the agent as-is";
}
```

A returned string becomes the tool's text output. Any other value is returned
as pretty-printed JSON. Throwing produces a structured failure that names the
plugin, the handler and the stage.

### Timeouts

Each invocation is bounded. `timeoutMs` on a tool, hook or command declaration
sets its own bound; otherwise the default (30s) applies. The agent's context is
always the outer bound, so a Ctrl-C stops a plugin even when its own timeout has
not elapsed.

## Permission model

A manifest's `permissions` are a **request**. They are never a grant. Bruce's
host policy decides what a plugin actually receives, and a plugin can never
grant itself anything.

| Permission | Grants |
|---|---|
| `fs.read` | Read files inside the workspace through host APIs. |
| `fs.write` | Write files inside the workspace through host APIs. |
| `net` | Make network requests through host APIs. |
| `shell` | Run shell commands through host APIs. |
| `storage` | Use `bruce:storage`. |
| `events` | Emit observation events. |

The rules, in order:

1. **Denied wins.** A permission in `plugins.deny` is never granted.
2. **Per-plugin overrides global.** `plugins.perPlugin.<name>.allow` replaces
   `plugins.allow` for that plugin entirely.
3. **A plugin only gets what it declared.** A permission the host allows but the
   manifest did not request is not granted. It is reported as "undeclared", so
   you can see the mismatch.
4. **The sandbox must be able to enforce it.** A capability the sandbox cannot
   enforce is refused at load time. A plugin is never told it may read the
   filesystem when the host cannot actually stop it from reading anything else.

By default **no permission is granted**. Installing a plugin does not by itself
give it filesystem, network or shell access.

### Configuring the policy

```json
{
  "plugins": {
    "enabled": true,
    "allow": ["fs.read", "storage"],
    "deny": ["shell"],
    "perPlugin": {
      "todo-tracker": { "allow": ["fs.read", "storage", "events"] },
      "sketchy-plugin": { "deny": ["fs.read", "fs.write", "net", "shell"] }
    },
    "failFast": false,
    "allowDynamicCode": false
  }
}
```

`plugins.enabled: false` turns discovery off entirely: Bruce then behaves exactly
as it did before the plugin system existed.

### What a plugin cannot do

A plugin has no `require`, no `process`, no `os`, no `fetch`, no filesystem, no
network socket and no shell. Those names are not defined in its runtime. The
only way out is a host capability, and every host capability is checked against
the grant before it runs.

## Host API

The host installs one global, `bruce`. What is present depends on the grant: a
plugin without the `storage` permission has no `bruce.storage`.

### `bruce:api`

```js
import { apiVersion, name, version, source, info } from "bruce:api";
// apiVersion: "bruce.plugin/v1"
// name, version, source: this plugin's own identity
// info(): the same values as an object
```

### `bruce:storage`

Long-lived state belongs here, **not** in a module global. Several runtimes
serve one plugin, so a module global in one runtime is not the same variable in
the next; a value stored there will appear to change at random.

```js
import { get, set, remove, keys } from "bruce:storage";

set({ scope: "plugin", key: "lastRun", value: { at: Date.now() } });
const found = get({ scope: "plugin", key: "lastRun" });
// found.found === true, found.value === { at: ... }
remove({ scope: "plugin", key: "lastRun" });
keys({ scope: "plugin" });   // { keys: ["lastRun"] }
```

Scopes:

| Scope | Lifetime |
|---|---|
| `invocation` | One tool call. Cleared when the call ends. |
| `session` | The current session. |
| `plugin` | Until the plugin is unloaded. |
| `workspace` | The current workspace. |
| `global` | Every workspace. |

Storage is namespaced per plugin. Plugin A cannot read or overwrite plugin B's
values, even with the same key and scope.

### `bruce:events`

```js
import { emit } from "bruce:events";
emit("todo.scan.finished", { count: 12 });
```

Emitting requires the `events` permission. Events are namespaced by plugin and
appear in Bruce's activity stream.

## Import restrictions

Only two kinds of import resolve:

1. **Relative modules inside the plugin's own directory** — `./lib/util.js`,
   `../shared.js`. A relative import is resolved against the importing module,
   so a module in `lib/` importing `./sibling.js` gets its own sibling.
2. **Host virtual modules** — `bruce:api`, `bruce:storage`, `bruce:events`,
   each available only when the plugin was granted the matching permission.

Everything else is refused:

- bare specifiers (`left-pad`, `lodash`) — no npm resolution,
- `node_modules`, even when the directory exists,
- Node builtins (`fs`, `path`, `child_process`, `node:fs`),
- absolute paths and `file://` URLs,
- network URLs.

Path traversal is blocked after symlink resolution, so a symlink pointing
outside the plugin directory does not become a way out:

```js
import "../../../../etc/passwd";   // refused
import "./escape-link.js";         // refused when it resolves outside
```

Modules are compiled once and shared by every runtime of that plugin, so a warm
invocation does not pay the parse cost again.

## Dynamic code

`eval`, the `Function` constructor and any equivalent are **disabled by
default**. A plugin has no reason to generate code, and generated code is the
classic way out of a sandbox. Attempting it throws an `EvalError`:

```js
eval("1+1");                    // EvalError
new Function("return 1")();     // EvalError
```

`plugins.allowDynamicCode: true` turns it back on. That is for a trusted
deployment only.

The built-in prototypes are frozen and shared, so a plugin cannot patch
`Array.prototype` to change how another plugin's code behaves.

## Hooks

Hooks are declared in the manifest and split into two kinds.

### Observer hooks

Observation only. They cannot change or stop anything.

| Event | Fires |
|---|---|
| `session.started` | A session begins. |
| `session.ended` | A session ends. |
| `tool.started` | A tool call begins. |
| `tool.completed` | A tool call finishes. |
| `message.created` | A message is produced. |

```js
export function onToolStarted(input) {
  emit("audit.tool", { tool: input.tool });
}
```

### Interceptor hooks

These can change or stop a flow. They run **synchronously, in a deterministic
order**: by plugin name, then by declaration order. The second interceptor sees
what the first one produced.

| Event | Can |
|---|---|
| `chat.before` | Rewrite the message list, or block the turn. |
| `tool.before` | Rewrite the arguments, or block the call. |
| `tool.after` | Rewrite the result text. |

```js
export function guard(input) {
  // input: { tool, args, runId, mode }
  return { args: { ...input.args, path: normalize(input.args.path) } };
}
```

Return shapes:

- `{ "args": {...} }` — replaces the arguments. A tool's declared schema and
  every policy, sandbox and approval check then run against **these** values,
  not the originals.
- `{ "block": true, "reason": "..." }` — refuses the call. The reason is shown
  to the model.
- `{ "result": { "output": "...", "status": "success" } }` — from `tool.after`.

An interceptor cannot grant a permission, disable the sandbox, skip approval or
change the security mode. Extra fields in a hook result are ignored, and every
security check runs again on the final data. If your hook rewrites
`{"path": "src/a.go"}` into `{"path": "/etc/passwd"}`, the call is refused — the
original argument having passed is irrelevant.

### Hook failure policy

| Hook | On failure |
|---|---|
| Observer | Logged and skipped. Observation never breaks a session. |
| `tool.before`, `chat.before` | **Fails closed.** The invocation is refused. A guard that cannot run must not be skipped. |
| `tool.after` | The produced result is kept and the failure is recorded. The operation already happened. |

A failing plugin never disables another plugin: each hook runs in its own
invocation.

## Slash commands

```js
export function report(input) {
  // input: { command, args: [...], raw, joined }
  return { output: "report" };     // or return a plain string
}
```

Plugin commands appear in `/help` and in Tab completion alongside built-ins.
A plugin command may never override a built-in command; the conflict is reported
and the built-in wins.

## Cancellation

Cancellation is the same for a plugin as for any other tool. A Ctrl-C, a tool
timeout, a session cancel, an agent cancellation or process shutdown all reach
the running JavaScript and stop it, including a `while (true)` loop:

```js
export function spin() { let i = 0; while (true) { i++; } }   // stoppable
```

After a cancellation the runtime is returned to the pool cleanly, so the next
call works. A cancelled call never leaves the pool deadlocked or the session
stuck.

## Reload

```text
/plugin reload                # every plugin
/plugin reload todo-tracker   # one plugin
/plugin unload todo-tracker   # remove it
```

A reload re-reads the manifest, recompiles the JavaScript, and replaces the
plugin's tools, hooks, commands and runtimes. Nothing accumulates: reloading ten
times leaves one tool, one hook and one command, and no leaked runtimes.

The policy is deterministic: an invocation already in flight finishes on the
code it started with, and the next invocation uses the new code. A reload never
interrupts work in progress.

Editing a plugin and running `/plugin reload` is the normal development loop.

## Debugging

```text
/plugin              # loaded plugins, granted permissions, diagnostics
/plugin info <name>  # one plugin in detail
/plugin hooks        # registered hooks, in execution order
/status              # plugin count, tool count, hook count
```

Load problems — a bad manifest, a syntax error, a missing handler, a permission
denial, a command conflict — appear as diagnostics under `/plugin` and as
activity events. A broken plugin never stops Bruce from starting, and never
affects another plugin. Set `plugins.failFast: true` to make a broken manifest
stop startup instead.

Thrown errors carry the plugin name, the plugin path, the handler and the
failure stage, so a message tells you exactly what failed where. Credentials are
redacted from error text.

Common mistakes:

| Symptom | Cause |
|---|---|
| "field timeoutMS" | Manifest field names are case-sensitive: it is `timeoutMs`. |
| "has no exported function" | The `handler` names an export the module does not have. |
| "does not exist in plugin" | The `entry` path is wrong, or the file is missing. |
| "is reserved by a built-in" | A tool or command name collides with a built-in. |
| "cannot be granted" | The permission was requested but the policy does not allow it, or the sandbox cannot enforce it. |
| A value "changes by itself" | It is in a module global. Use `bruce:storage`. |

## Security model

> **The JavaScript VM sandbox is not an OS security boundary.**
>
> Bruce's host policy is the final permission boundary.

The engine's frozen builtins, disabled dynamic code and runtime isolation make a
plugin *well-behaved*, not *contained*. They stop a plugin from patching shared
prototypes or compiling its way out of the sandbox; they do not stop a
determined plugin from consuming CPU, and they are not a substitute for the
operating system's own isolation.

What actually confines a plugin:

```text
JavaScript plugin
   │  only the host objects the grant allows; no os, no exec, no net, no Go values
   ▼
Bruce host capability   (bruce:storage, bruce:events, ...)
   │  every call checked against the grant
   ▼
Permission / policy     (tool.Policy capability metadata + host policy)
   │  re-checked on the FINAL data, after any hook rewrote it
   ▼
Sandbox / HITL          (sandbox.Manager, approval.Handler)
   ▼
The real operation
```

Consequences worth internalising:

- A plugin runs in-process. It shares the address space with Bruce. A bug in the
  engine or in the host bridge is a bug in Bruce.
- The sandbox constrains *shell commands and file writes*, not JavaScript. A
  plugin that is not granted a capability has no path to the filesystem at all,
  because there is no filesystem API in its runtime.
- Approval is enforced by the host, not by the plugin. A plugin tool that
  declares a write, shell or network capability asks the user for approval
  through the same prompt as a built-in tool, and the plugin cannot suppress it.
- A plugin cannot change the sandbox mode, grant itself a permission, or mark
  itself approved.

## Known limitations

- **No TypeScript.** The entry must be JavaScript. TypeScript should be
  precompiled to JavaScript before shipping, not run through a TypeScript
  runtime inside the VM.
- **No npm, no `node_modules`, no `package.json`.** A plugin must be
  self-contained: either one file or relative modules you ship with it.
- **No Node builtins.** No `fs`, `path`, `process`, `child_process`, `http`.
- **No Pi or OpenCode plugin compatibility.** The API is inspired by those
  tools, not a compatibility layer for them.
- **Storage is in-memory for now.** `session`, `plugin`, `workspace` and
  `global` scopes do not survive a restart. The API is the durable one; the
  backing store is not yet.
- **`chat.before` receives the message list as JSON.** A hook can rewrite
  messages, but it cannot yet inspect the model or the token budget.
- **Hook ordering is by plugin name, then declaration order.** There is no
  numeric priority. If you need to run after another plugin, its name must sort
  after yours.
- **A plugin cannot be loaded from outside the two search roots.** There is no
  path-based install yet.
- **No resource accounting.** A plugin can consume CPU up to its timeout, and
  memory is not capped per plugin.
- **Windows is untested.** The path handling covers Windows forms, but no
  Windows environment was used to verify it.

## Complete example

```text
.bruce/plugins/todo-tracker/
├── plugin.json
└── index.js
```

`plugin.json`:

```json
{
  "apiVersion": "bruce.plugin/v1",
  "name": "todo-tracker",
  "version": "1.0.0",
  "description": "Tracks TODO comments and reports on them",
  "entry": "index.js",
  "permissions": ["fs.read", "storage"],
  "concurrency": { "maxRuntimes": 2 },
  "tools": [
    {
      "name": "todo_scan",
      "description": "Scan a path for TODO comments",
      "handler": "scan",
      "schema": {
        "type": "object",
        "properties": {
          "path": { "type": "string" },
          "options": {
            "type": "object",
            "properties": {
              "recursive": { "type": "boolean" },
              "depth": { "type": "integer", "minimum": 1 }
            }
          },
          "extensions": { "type": "array", "items": { "type": "string" } }
        },
        "required": ["path"]
      }
    }
  ],
  "commands": [
    { "name": "todo-report", "description": "Show the last scan", "handler": "report" }
  ]
}
```

`index.js`:

```js
import { get, set } from "bruce:storage";

export function scan(input) {
  const { path, options = {}, extensions = [] } = input;
  const depth = options.depth ?? 3;
  const recursive = options.recursive === true;
  set({ scope: "plugin", key: "lastScan", value: { path, depth, recursive, extensions } });
  return { path, depth, recursive, extensions, scanned: true };
}

export function report() {
  const found = get({ scope: "plugin", key: "lastScan" });
  if (!found.found) return { output: "No scan has run yet." };
  return { output: "Last scan: " + JSON.stringify(found.value) };
}
```

Then:

```text
/plugin                        # confirm it loaded and what it was granted
/plugin reload todo-tracker    # after editing index.js
```

## See also

- [plugin-architecture.md](plugin-architecture.md) — architecture, module layout,
  implementation plan and the security boundary rationale.
- [sandbox-design.md](sandbox-design.md) — the sandbox the host policy defers to.
