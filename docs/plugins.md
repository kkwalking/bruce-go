# Bruce Go JavaScript 插件

Bruce Go 可以用 JavaScript 插件扩展。一个插件就是「一个 manifest + 一个 JavaScript
ES module」组成的目录，它可以向 Bruce 贡献 **Tool**、**Hook** 与 **Slash Command**，
并使用一小组由用户显式授予的宿主能力。

本文是插件作者参考手册。设计取舍、模块划分与安全模型推导见
[plugin-architecture.md](plugin-architecture.md)。

> 英文版本见 [plugins.en.md](plugins.en.md)。

## 插件是什么

一个插件是：

- 插件搜索根下的一个**目录**；
- 目录里有一个 **`plugin.json` manifest**，声明插件提供什么、需要什么；
- 以及一个 **JavaScript 入口模块**，导出 manifest 中指名引用的 handler 函数。

插件**不是**第二套 Agent Runtime。它定义的 Tool 会注册进 Bruce 原有的 Tool Registry，
然后与内建工具经历**完全相同**的调度、并发控制、沙箱、审批、网络策略、取消与事件记录。
Agent 无法分辨一个 Tool 来自哪里，也不需要分辨。

```text
<workspace>/.bruce/plugins/<name>/plugin.json     workspace 级插件
<workspace>/.bruce/plugins/<name>/index.js
<home>/.bruce/plugins/<name>/plugin.json          user 级插件
<home>/.bruce/plugins/<name>/index.js
```

也支持 `<name>.json` 这种与模块同级的单文件 manifest 布局。

### 插件目录

| 级别 | 路径 | 生效范围 |
|---|---|---|
| Workspace | `<workspace>/.bruce/plugins/` | 仅当前项目 |
| User | `~/.bruce/plugins/` | 所有项目 |

**优先级：同名时 workspace 插件覆盖 user 插件**，覆盖关系会列在 `/plugin` 里。
同一个根目录内按插件名顺序加载，因此结果不依赖目录遍历顺序。

两个插件不得声明同名 Tool 或同名 Command。按插件名排序，先声明者保留，
后来者的该条声明被丢弃并报为诊断——不会有任何静默覆盖。

## Manifest 格式

`plugin.json` 是 JSON，字段名用 camelCase。**未知字段名会被拒绝**：拼错的字段
不能静默失效，否则你会以为配置生效了。

```json
{
  "apiVersion": "bruce.plugin/v1",
  "name": "todo-tracker",
  "version": "1.0.0",
  "description": "统计 workspace 中的 TODO",
  "entry": "index.js",
  "permissions": ["fs.read"],
  "concurrency": { "maxRuntimes": 2, "parallelSafe": true },
  "tools": [
    {
      "name": "todo_scan",
      "description": "扫描 workspace 中的 TODO 注释",
      "handler": "scan",
      "promptSnippet": "查找 workspace 中的 TODO 注释",
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
    { "name": "todo-report", "description": "打印 TODO 报告", "handler": "report", "usage": "/todo-report [path]" }
  ],
  "metadata": { "homepage": "https://example.com" }
}
```

### 字段说明

| 字段 | 必填 | 含义 |
|---|---|---|
| `apiVersion` | 是 | 必须是 `bruce.plugin/v1`。 |
| `name` | 是 | `^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`，最长 64 字符。 |
| `version` | 是 | `1.2.3`，可带 `-prerelease` 或 `+build`。 |
| `description` | 是 | 会显示在 `/plugin` 中。 |
| `entry` | 是 | JavaScript 模块路径，相对插件目录，且必须留在该目录内。 |
| `permissions` | 否 | 插件级权限请求，见[权限模型](#权限模型)。 |
| `concurrency` | 否 | `maxRuntimes`（0–64，默认 4）与 `parallelSafe`（默认 false）。 |
| `tools` | 否 | Tool 声明。 |
| `hooks` | 否 | Hook 声明。 |
| `commands` | 否 | Slash Command 声明。 |
| `metadata` | 否 | 自由字符串映射。key 由你决定，value 必须是字符串。 |

Tool 声明必须给出 `name`、`description`、`handler`。`schema` 缺省为空对象 schema。
Tool 上的 `permissions` 会在该 Tool 范围内收窄插件级声明；不写 `permissions` 的
Tool 继承插件级声明。`parallelSafe` 与 `timeoutMs` 可以逐 Tool 设置。

命令名不得与内建命令（如 `/sandbox`、`/hitl`、`/plugin`）冲突；工具名不得与内建工具
（如 `read_file`、`execute_command`）冲突。两者都在**加载期**被拒绝。

## JavaScript 入口

入口是 ES module。Handler 是**具名导出**，manifest 用名字指向它们：

```js
// index.js
import { get, set } from "bruce:storage";
import { info } from "bruce:api";

export function scan(input) {
  // input 就是模型的工具参数对象，原样传入。
  const { path, options = {}, extensions = [] } = input;
  const recursive = options.recursive === true;
  const depth = options.depth ?? 3;
  set({ scope: "plugin", key: "lastPath", value: path });
  return { path, recursive, depth, extensions, host: info().version };
}

export function guard(input) {
  // 返回 { block: true, reason } 可拒绝一次工具调用。
  if (input.tool === "execute_command" && /rm -rf/.test(input.args.command ?? "")) {
    return { block: true, reason: "危险命令" };
  }
  return { args: input.args };
}

export function report() {
  return { output: "TODO 报告：" + (get({ scope: "plugin", key: "lastPath" }).value ?? "无") };
}
```

Handler 可以是嵌套路径：`"handler": "tools.read"` 会解析导出对象 `tools` 的
`read` 属性。

### 为什么用具名导出而不是 `registerTool()`

Handler 是在**任何 JavaScript 执行之前**就由静态元数据解析出来的。这正是让插件形态
在加载期即可知的关键：handler 写错是带文件名和行号的启动期错误，而不是第一次调用时
的意外。它同时让多个 Runtime 服务同一个插件时不必共享闭包，也让 reload 变成"重新
编译一个模块"，而不是"拆掉活着的状态"。

### Tool 参数与返回值

参数以单个对象传入。标准 JSON 数据模型被**无损**保留：嵌套对象、数组、布尔、
数字、`null` 与字符串都不会被字符串化或扁平化。

```js
export function echo(input) {
  return input;                 // 结构与类型原样返回
}
export function text(input) {
  return "纯字符串会原样交给 Agent";
}
```

返回字符串会成为该 Tool 的文本输出；其它值会以美化 JSON 返回。抛异常会产生结构化
失败，其中带插件名、handler 与失败阶段。

### 超时

每次调用都有上界。Tool / Hook / Command 声明上的 `timeoutMs` 设置各自的上界；
不设则用默认值（30 秒）。Agent 的 context 始终是最外层上界，所以即使插件自己的
超时还没到，Ctrl-C 也能终止它。

## 权限模型

Manifest 里的 `permissions` 是**请求**，永远不是授予。插件实际拿到什么由 Bruce 的
Host Policy 决定，插件无法给自己授予任何东西。

| 权限 | 授予的能力 |
|---|---|
| `fs.read` | 通过宿主 API 读取 workspace 内的文件。 |
| `fs.write` | 通过宿主 API 写入 workspace 内的文件。 |
| `net` | 通过宿主 API 发起网络请求。 |
| `shell` | 通过宿主 API 执行 shell 命令。 |
| `storage` | 使用 `bruce:storage`。 |
| `events` | 发出观测事件。 |

判定顺序：

1. **拒绝永远优先。** 出现在 `plugins.deny` 中的权限永不授予。
2. **插件级覆盖全局。** `plugins.perPlugin.<name>.allow` 会**整体替换**该插件的
   `plugins.allow`。
3. **插件只拿到自己声明过的。** 策略允许但 manifest 没请求的权限不会授予，只会被
   记为 "undeclared"，让你看到这处不一致。
4. **沙箱必须能强制执行。** 沙箱无法强制执行的能力会在加载期被拒绝。宿主不会在
   其实拦不住插件读任意文件的情况下，告诉插件"你可以读文件系统"。

默认**不授予任何权限**。安装一个插件本身并不会给它文件系统、网络或 shell 访问。

### 配置策略

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

`plugins.enabled: false` 会完全关闭插件发现：此时 Bruce 的行为与引入插件系统之前
完全一致。

### 插件做不到的事

插件的运行时里**不存在** `require`、`process`、`os`、`fetch`、文件系统、网络 socket
和 shell。这些名字在它的运行时中未定义。唯一的出口是宿主能力，而每个宿主能力在
执行前都会对照授权检查。

## Host API

宿主只安装一个全局对象 `bruce`。它里面有什么取决于授权：没有 `storage` 权限的插件
没有 `bruce.storage`。

> **当前已实现的模块是 `bruce:api`、`bruce:storage`、`bruce:events`。**
> `fs.read`、`fs.write`、`net`、`shell` 四种权限在 manifest 与策略层已完整可用
> （会被正确判定、拒绝、并要求审批），但**尚无对应的 `bruce:fs` / `bruce:net`
> 模块**供插件实际调用。也就是说插件目前可以"被允许拥有"这些能力，但还没有
> "使用"这些能力的 API。

### `bruce:api`

```js
import { apiVersion, name, version, source, info } from "bruce:api";
// apiVersion: "bruce.plugin/v1"
// name, version, source: 该插件自身的身份
// info(): 与上面同样的值，以对象形式返回
```

### `bruce:storage`

长期状态放在这里，**不要**放在模块全局变量里。一个插件由多个 Runtime 服务，
某个 Runtime 里的模块全局与下一个 Runtime 里的同名变量并不是同一个；存在那里的值
会表现得"自己会变"。

```js
import { get, set, remove, keys } from "bruce:storage";

set({ scope: "plugin", key: "lastRun", value: { at: Date.now() } });
const found = get({ scope: "plugin", key: "lastRun" });
// found.found === true, found.value === { at: ... }
remove({ scope: "plugin", key: "lastRun" });
keys({ scope: "plugin" });   // { keys: ["lastRun"] }
```

作用域：

| 作用域 | 生命周期 |
|---|---|
| `invocation` | 一次工具调用。调用结束时清理。 |
| `session` | 当前 session。 |
| `plugin` | 直到插件被卸载。 |
| `workspace` | 当前 workspace。 |
| `global` | 所有 workspace。 |

存储按插件命名空间隔离。插件 A 无法读取或覆盖插件 B 的值，即使 key 与作用域相同。

### `bruce:events`

```js
import { emit } from "bruce:events";
emit("todo.scan.finished", { count: 12 });
```

发出事件需要 `events` 权限。事件按插件命名，会出现在 Bruce 的活动流中。

## Import 限制

只有两类 import 能解析成功：

1. **插件自身目录内的相对模块** —— `./lib/util.js`、`../shared.js`。相对 import 以
   导入方所在目录为基准解析，因此 `lib/` 里的模块 `import "./sibling.js"` 拿到的是
   它自己的同级模块。
2. **宿主 virtual module** —— `bruce:api`、`bruce:storage`、`bruce:events`，
   每个仅在该插件被授予对应权限时才可用。

其余一切都被拒绝：

- 裸标识符（`left-pad`、`lodash`）—— 不做 npm 解析；
- `node_modules`，即使该目录真实存在；
- Node 内建模块（`fs`、`path`、`child_process`、`node:fs`）；
- 绝对路径与 `file://` URL；
- 网络 URL。

路径穿越在**解析符号链接之后**判定，因此指向插件目录之外的软链接不会成为一条出路：

```js
import "../../../../etc/passwd";   // 拒绝
import "./escape-link.js";         // 当它解析到目录之外时拒绝
```

模块只编译一次并被该插件的所有 Runtime 共享，因此热调用不会重复付出解析成本。

## 动态代码

`eval`、`Function` 构造器及任何等价能力**默认关闭**。插件没有理由生成代码，而生成
代码是逃出沙箱的经典手法。尝试使用会抛 `EvalError`：

```js
eval("1+1");                    // EvalError
new Function("return 1")();     // EvalError
```

`plugins.allowDynamicCode: true` 可以重新打开，仅适用于可信部署。

内建原型被冻结并共享，因此插件无法通过改写 `Array.prototype` 影响另一个插件的代码
行为。

## Hook

Hook 在 manifest 中声明，分两类。

> **接线状态（重要）**：目前只有 `tool.before` 与 `tool.after` 被真正接入执行链
> （经由 `integrated.Runtime` 注册的 `ToolInterceptor`）。下表标注了每个事件的实际
> 状态。未被接线的 5 个 observer 事件与 `chat.before`，其校验、执行、失败策略与
> 单元测试都已实现，但运行时目前不会调用它们——**注册成功不等于会触发**。

### Observer Hook（观察）

只能观察，不能改变或阻止任何流程。

| 事件 | 触发时机 | 当前状态 |
|---|---|---|
| `session.started` | 会话开始。 | ⚠️ 未接线，不会触发 |
| `session.ended` | 会话结束。 | ⚠️ 未接线，不会触发 |
| `tool.started` | 工具调用开始。 | ⚠️ 未接线，不会触发 |
| `tool.completed` | 工具调用结束。 | ⚠️ 未接线，不会触发 |
| `message.created` | 产生一条消息。 | ⚠️ 未接线，不会触发 |

```js
export function onToolStarted(input) {
  emit("audit.tool", { tool: input.tool });
}
```

> 替代方案：Bruce 自身的 `event.Bus` 已经会发出 `tool_call_started`、
> `tool_call_completed`、`message_completed`、`run_started` 等事件。但这些是
> **插件主动上报**，不是宿主回调插件，语义不同。

### Interceptor Hook（拦截）

可以改变或阻止流程。它们**同步、按确定顺序**执行：先按插件名，再按声明顺序。
后一个 interceptor 能看到前一个产生的数据。

| 事件 | 能做什么 | 当前状态 |
|---|---|---|
| `tool.before` | 改写参数，或拒绝调用。 | ✅ 已接线 |
| `tool.after` | 改写结果文本。 | ✅ 已接线 |
| `chat.before` | 改写消息列表，或拦截整个回合。 | ⚠️ 未接线，不会触发 |

```js
export function guard(input) {
  // input: { tool, args, runId, mode }
  return { args: { ...input.args, path: normalize(input.args.path) } };
}
```

返回结构：

- `{ "args": {...} }` —— 替换参数。该 Tool 声明的 schema、以及所有策略、沙箱、
  审批检查随后都针对**这些**值执行，而不是原始值。
- `{ "block": true, "reason": "..." }` —— 拒绝调用，reason 会展示给模型。
- `{ "result": { "output": "...", "status": "success" } }` —— 由 `tool.after` 返回。

Interceptor 无法授予权限、禁用沙箱、跳过审批或改变安全模式。Hook 返回结果中的额外
字段会被忽略，且所有安全检查都会针对**最终数据**重新执行。如果你的 hook 把
`{"path": "src/a.go"}` 改成 `{"path": "/etc/passwd"}`，这次调用会被拒绝——原始参数
曾经通过检查这件事无关紧要。

### Hook 失败策略

| Hook | 失败时的行为 |
|---|---|
| Observer | 记录并跳过。观察永远不能弄坏会话。 |
| `tool.before`、`chat.before` | **fail closed**：拒绝该次调用。跑不起来的守卫绝不能被跳过。 |
| `tool.after` | 保留已经产生的结果，并记录失败。操作已经发生了。 |

一个插件失败不会让另一个插件失效：每个 hook 在自己的调用里执行。

## Slash Command

```js
export function report(input) {
  // input: { command, args: [...], raw, joined }
  return { output: "report" };     // 也可以直接返回字符串
}
```

插件命令与内建命令一起出现在 `/help` 与 Tab 补全中。插件命令永远不能覆盖内建命令；
冲突会被报出，内建命令胜出。

## 取消

插件与其它工具共用同一套取消机制。Ctrl-C、工具超时、session 取消、agent 取消或进程
退出都会到达正在运行的 JavaScript 并终止它，包括 `while (true)` 死循环：

```js
export function spin() { let i = 0; while (true) { i++; } }   // 可被终止
```

取消之后 Runtime 会被干净地归还到池中，因此下一次调用可以正常工作。被取消的调用
不会让池死锁，也不会让会话卡住。

## Reload

```text
/plugin reload                # 重新加载全部插件
/plugin reload todo-tracker   # 重新加载单个插件
/plugin unload todo-tracker   # 卸载
```

Reload 会重新读取 manifest、重新编译 JavaScript，并替换该插件的 Tool、Hook、Command
与 Runtime。不会有任何累积：reload 十次之后仍然只有一个 Tool、一个 Hook、一个
Command，也不会泄漏 Runtime。

策略是确定的，而且是**被强制执行**的，不只是写在文档里：

- **正在运行**的调用用它开始时的代码跑完，绝不会被 reload 打断。
- 仍在**等待 Runtime** 的调用会被重新指向新 generation 并执行新代码，**不会失败**。
- 之后的所有调用都使用新代码。

因此 reload 永远不会把一次工具调用变成失败，也不会让调用方拿到新旧混合的结果。

改完插件后执行 `/plugin reload`，这就是日常开发循环。

## 调试

```text
/plugin              # 已加载的插件、已授予的权限、诊断信息
/plugin info <name>  # 单个插件详情
/plugin hooks        # 已注册的 hook 及其执行顺序
/status              # 插件数、工具数、hook 数
```

加载问题——manifest 错误、语法错误、handler 缺失、权限拒绝、命令冲突——会作为诊断
出现在 `/plugin` 下，并作为活动事件出现。Runtime 丢弃也会被上报：如果插件把 Runtime
弄坏到池必须扔掉它，你会看到 `plugin.runtime_discarded`，带插件名与原因，而不是一次
静默替换。**坏插件永远不会阻止 Bruce 启动**，也不会影响其它插件。设置
`plugins.failFast: true` 可以让坏 manifest 直接阻止启动。

抛出的错误带插件名、插件路径、handler 与失败阶段，因此一条消息就能说清"哪里、什么
阶段、失败了什么"。错误文本中的凭据会被脱敏。

常见错误：

| 现象 | 原因 |
|---|---|
| `field timeoutMS` | Manifest 字段名大小写敏感，正确的是 `timeoutMs`。 |
| `has no exported function` | `handler` 指向的导出在模块中不存在。 |
| `does not exist in plugin` | `entry` 路径写错，或文件不存在。 |
| `is reserved by a built-in` | 工具名或命令名与内建冲突。 |
| `cannot be granted` | 请求了该权限，但策略不允许，或沙箱无法强制执行。 |
| 某个值"自己会变" | 它存在模块全局变量里。请改用 `bruce:storage`。 |

## 安全模型

> **JavaScript VM 沙箱不是操作系统级安全边界。**
>
> **Bruce 的 Host Policy 才是最终权限边界。**

引擎的冻结内建、关闭动态代码、Runtime 隔离，让插件**行为规矩**，而不是让它**被关住**。
它们能阻止插件改写共享原型或靠编译代码逃出沙箱；它们阻止不了一个铁了心的插件消耗
CPU，也不能替代操作系统自身的隔离。

真正约束插件的是这条链路：

```text
JavaScript 插件
   │  只能使用授权范围内的宿主对象；没有 os、没有 exec、没有 net、没有 Go 值
   ▼
Bruce Host Capability   (bruce:storage、bruce:events ...)
   │  每次调用都对照授权检查
   ▼
Permission / Policy     (tool.Policy 能力元数据 + host policy)
   │  在任何 hook 改写之后，针对【最终数据】重新检查
   ▼
Sandbox / HITL          (sandbox.Manager、approval.Handler)
   ▼
真实操作
```

值得记住的推论：

- **插件与 Bruce 同进程运行**，共享地址空间。引擎或宿主桥接里的 bug 就是 Bruce 的 bug。
- 沙箱约束的是**shell 命令与文件写入**，不是 JavaScript。没被授予能力的插件根本
  没有通往文件系统的路径，因为它的运行时里压根没有文件系统 API。
- 审批由宿主强制执行，不由插件决定。声明了写、shell 或网络能力的插件工具，会通过
  与内建工具**同一个**审批提示征求用户同意，插件无法抑制它。
- 插件无法改变沙箱模式、无法给自己授予权限、无法把自己标记为已批准。

## 已知限制

- **没有 TypeScript。** 入口必须是 JavaScript。若要用 TypeScript，应当在发布前
  **预编译**成 JavaScript，而不是在 VM 里塞一个 TypeScript 运行时。
- **没有 npm、没有 `node_modules`、没有 `package.json`。** 插件必须自包含：
  要么单文件，要么带上自己用到的相对模块。
- **没有 Node 内建模块。** 没有 `fs`、`path`、`process`、`child_process`、`http`。
- **不兼容 Pi 或 OpenCode 插件。** API 是受它们启发的，不是它们的兼容层。
- **Storage 目前是内存实现。** `session`、`plugin`、`workspace`、`global` 作用域
  在进程重启后不保留。API 是按持久化设计的，后端还不是。
- **5 个 observer hook 事件与 `chat.before` 尚未接线**：契约、校验、失败策略与测试
  都已实现，但运行时目前不会调用它们。见 [Hook](#hook) 一节的状态表。
- **`fs.read` / `fs.write` / `net` / `shell` 尚无对应的 Host API 模块。** 权限判定
  已完整可用，但插件还没有使用这些能力的 API。
- **`chat.before` 只拿到 JSON 形式的消息列表。** hook 可以改写消息，但还无法检查
  模型或 token 预算。
- **Hook 顺序按插件名，再按声明顺序。** 没有数值优先级。若你必须排在某个插件之后，
  你的插件名必须排在它后面。
- **插件无法从两个搜索根之外加载。** 还没有基于路径的安装方式。
- **没有资源计量。** 插件最多可以消耗到超时为止的 CPU，内存未按插件设限。
- **Windows 未验证。** 路径处理覆盖了 Windows 形式，但没有在 Windows 环境实测过。

## 完整示例

```text
.bruce/plugins/todo-tracker/
├── plugin.json
└── index.js
```

`plugin.json`：

```json
{
  "apiVersion": "bruce.plugin/v1",
  "name": "todo-tracker",
  "version": "1.0.0",
  "description": "统计 TODO 注释并生成报告",
  "entry": "index.js",
  "permissions": ["fs.read", "storage"],
  "concurrency": { "maxRuntimes": 2 },
  "tools": [
    {
      "name": "todo_scan",
      "description": "扫描指定路径下的 TODO 注释",
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
    { "name": "todo-report", "description": "显示上一次扫描结果", "handler": "report" }
  ]
}
```

`index.js`：

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
  if (!found.found) return { output: "还没有执行过扫描。" };
  return { output: "上次扫描：" + JSON.stringify(found.value) };
}
```

然后：

```text
/plugin                        # 确认已加载、以及拿到了哪些权限
/plugin reload todo-tracker    # 改完 index.js 之后
```

## 另见

- [plugin-architecture.md](plugin-architecture.md) —— 架构、模块划分、实现计划与
  安全边界推导，以及实现期间发现并修复的缺陷记录。
- [sandbox-design.md](sandbox-design.md) —— host policy 所依赖的沙箱设计。
- [plugins.en.md](plugins.en.md) —— 本文的英文版本。
