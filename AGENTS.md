# bruce-go — 开发约定

本文件对人和 agent 同等生效。凡写「必须」「拒绝」的，都已经或应当由机器强制
（见「机器强制」一节），不是建议。

## 项目背景

本项目是 Java 版 bruce coding agent 的 Go 版本移植，尚未完成。

- 原 Java 版本：`/Users/zhouzekun/code/bruce-cli`

## 分支与提交

单人维护，`main` 是唯一长期分支。

### 必须在分支上开发

**需求开发和 bug 修复一律先开分支。** 不允许在 `main` 上直接 `commit`。

```sh
git checkout -b feat/<name>      # 或 fix/<name>、chore/<name>、refactor/<name>
```

`main` 只接受两种进入方式：

1. 本地合并特性分支后推送（`git merge feat/<name>`，再 `git push origin main`）；
2. 在 GitHub 上合并 PR。

> 由 `.githooks/pre-push` 强制：在 `main` 上直接创建的提交会被拒绝推送。
> 来自分支的提交、以及 merge 提交，正常放行。

### 必须带验收报告

**每次提交的正文都是一份验收报告，不允许空正文。** 只有一句话的 subject 无法在
半年后解释当时的判断，这是本项目最容易累积的债。

格式：

```text
<scope>: <一句话说清改了什么>

— <改动前的行为，错在哪；涉及具体位置时给 file.go:line>
<改成了什么，逐条对应上面每一处>

<验证：实际跑过的命令与结果。新增或修改的测试点名，
 并说明它在改动前会失败（fails without the change）>

<未验证的部分写明「未验证」及原因，不许省略>
```

`<scope>` 用 `feat` / `fix` / `refactor` / `perf` / `test` / `docs` / `chore`，
可带括号子域，例如 `fix(sandbox):`、`feat(tui):`。

一份完整的例子：

```text
fix(sandbox): .git 写保护改为大小写不敏感

— 原来的写保护用大小写敏感的前缀比较，在 macOS 的默认文件系统上可以用
  .GIT/config 绕过，符号链接也没有二次校验（internal/sandbox/seatbelt.go:212）。

现在在解析符号链接之后再比一次，并统一按大小写不敏感比较；
deny 规则同时覆盖 .git 与其解析后的真实路径。

新增 TestGitProtectionCaseInsensitive（.GIT/config 与指向 .git 的软链接
都被拒绝）——没有这个改动它会失败。
go vet ./...、go test ./...、go test -race ./... 全部通过。

未验证：Windows 的 ACL 路径，本机没有环境。
```

> 由 `.githooks/commit-msg` 强制：去掉注释行后正文为空即拒绝提交。
> `Merge` / `Revert` / `fixup!` / `squash!` 提交豁免。

### 未验证就说未验证

**不许把「没测」写成「应该没问题」。** 沙箱后端、真实 API、跨平台路径这类本机不具备
条件的，照下面写清楚即可，这比含糊其辞有用得多：

```text
未验证：Linux Bubblewrap 路径，本机是 macOS。
```

## 版本与发布

**版本号的唯一来源是 git tag，不要再手工修改源码里的版本常量。**

发布一个版本 = 推一个 `v*` tag。这一步会触发 `release.yml`，自动产出
GitHub Release 与可下载产物：

```sh
make tag VERSION=v0.9.0     # 打一个带注释的 tag
git push origin v0.9.0      # 触发 release.yml，产物挂到 GitHub Release
```

`make build` 用 `git describe` 派生版本号并注入二进制：

- 正好在 tag 上 → `0.9.0`
- tag 之后 3 个提交 → `0.9.0-3-gabc1234`
- 工作区有未提交改动 → 带 `-dirty` 后缀

裸 `go build ./cmd/bruce` 报告 `dev`。

- 重大功能 → minor 递增（`v0.9.0` → `v0.10.0`）
- bug 修复 → patch 递增（`v0.9.0` → `v0.9.1`）

### 发布产物

只发两个平台，定义在 `Makefile` 的 `PLATFORMS`（加平台改这里）：

| 平台 | 归档 |
|---|---|
| macOS（Apple Silicon） | `bruce_0.9.0_darwin_arm64.tar.gz` |
| Linux（x86-64） | `bruce_0.9.0_linux_amd64.tar.gz` |

另有 `checksums.txt`（sha256）。归档解开就是二进制本身，没有外层目录。
产物用 `CGO_ENABLED=0` 构建，因此 linux 包静态链接、不依赖目标机 glibc。

发布前可以在本地预演整个打包过程：

```sh
make release-artifacts VERSION=v0.9.0              # 产出到 dist/（已 gitignore）
scripts/verify-release-artifacts.sh dist 0.9.0     # 跑发版用的那套自检
```

`release.yml` 会在建 release **之前**先跑测试（含 `-race` 与沙箱严格模式）
并自检产物：产物集合、checksums、归档结构、内嵌的 GOOS/GOARCH 与版本号，
能执行的产物还会实跑 `--version` 并与 tag 比对。任一环失败就不发布。

自检逻辑在 `scripts/verify-release-artifacts.sh`，**不在 workflow 里内联** ——
`test.yml` 的 `release-dry-run` job 每次 push/PR 都用同一份脚本跑一遍完整发布
路径（含一组负向用例，确认坏产物真会被拒绝）。这样发布路径的回归不必等到
发版当天才发现：`release.yml` 只在推 tag 时运行，而推 tag 是不可逆的外部动作。

> 历史教训：自检脚本里曾有一处 `tar -tzf | grep -q` 配 `set -o pipefail`，
> 在 GNU tar 上因 SIGPIPE 稳定返回 141，BSD tar 上不复现 —— 本机 macOS
> 全绿，只有 Linux 会红。跨平台的行为差异要按 Linux 验证，别只信本机结果。

## 机器强制

这些已不是文档约定，改坏了会在提交时或 CI 里立刻失败：

| 约束 | 在哪强制 |
|---|---|
| 不在 `main` 上直接提交 | `.githooks/pre-push`（`make hooks` 安装） |
| 提交必须有正文 | `.githooks/commit-msg` |
| gofmt、vet、单测 | `make check`，以及 CI 的 `test` job |
| race 检测 | `go test -race ./...`，CI |
| 沙箱测试不许静默跳过 | CI 设 `BRUCE_REQUIRE_SANDBOX_TESTS=1` |
| 二进制能构建且能报版本 | CI 的 `build` job |
| 发布产物可信 | CI 的 `release` job：测试 + 产物自检不通过就不建 release |
| 发布路径不回归 | CI 的 `release-dry-run` job：每次 push/PR 跑一遍打包与自检 |

### 常用命令

```sh
make hooks          # 新 clone 后跑一次，启用 .githooks/
make check          # 提交前跑这个：gofmt -l + vet + test
make test           # 单测
make race           # race 检测
make sandbox-test   # 把沙箱测试的 skip 升级为失败（复现 CI 严格模式）
make build          # 带版本号构建
make release-artifacts VERSION=v0.9.0   # 本地预演发布产物，产出到 dist/
make tag VERSION=v0.9.0
```

**`make check` 是提交前的最低要求。**

### 沙箱测试为什么不能静默跳过

`internal/sandbox` 与 `internal/mcp` 的集成测试在本机后端不可用时默认 `t.Skip`，
所以 `go test ./...` 全绿并不代表沙箱是好的。CI 设
`BRUCE_REQUIRE_SANDBOX_TESTS=1` 把跳过变成失败；本地用 `make sandbox-test`
复现同样的严格性。

新增依赖沙箱后端的测试时，沿用这个环境变量判断，不要自己发明开关。

## 代码导航

### MCP

代码导航用 jCodeMunch-MCP。它在**本机全局配置**里（`~/.claude.json` 的项目条目），
不在仓库里；仓库不再提交 `.mcp.json`。

**开始任何任务前：**

1. `order { "action": "resolve_repo", "args": { "path": "." } }` 确认项目已索引。
   未索引则 `order { "action": "index_folder", "args": { "path": "." } }`。

**然后：**

- 知道要什么 → `order { "action": "<name>", "args": { ... } }`
- 只知道目标 → `route { "query": "用一句话描述任务" }` 选动作并整形参数
- 想看有什么 → `menu { "query": "你想做什么" }` 返回匹配的动作与示例参数
- 要完整目录与用法规则 → `jcodemunch_guide`

`menu` 与 `jcodemunch_guide` 会列出不在你工具清单里的动作，这是预期的：
front door 就是用来调用它们的。

**读结果时：**

- `verdict` 为 `no_implementation_found` 是「不存在」的证据。如实报告缺口，
  不要换个说法再搜一遍。
- `verdict` 为 `degraded` 表示某个通道不可用，此时**不能**据此断定不存在。
  先看 note 再下结论。
- `source: ""` 且带 `source_status` 表示正文没读到，不代表符号是空的。

**编辑文件后：**

- 装了 PostToolUse hook（Claude Code）时会自动重建索引。
- 否则编辑后调用 `order { "action": "register_edit", "args": { "paths": [...] } }`，
  批量改动用一次调用。

**每个会话宣告一次模型**，让服务端据此决定回答的详细程度：
`announce_model { "model": "<your-model-id>" }`。

**例外：** 即将编辑某个文件时用 `Read` —— harness 要求 `Edit`/`Write` 前先 `Read`。
用 jCodeMunch 去*找*和*理解*代码，只对要改的文件用 `Read`。

### 不再使用的工具

**不使用**代码知识图谱类工具（code-review-graph、GitNexus 等），**不使用**
openspec 规格流程。两者已从仓库中移除，历史 spec 一并删除。不要重新引入，
也不要引用它们。
