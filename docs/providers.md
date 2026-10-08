# LLM 供应商配置

Bruce 的 `llm.providers` 是一张**任意命名**的供应商表。每个条目声明自己的端点、
线协议、凭据与模型列表，没有名字上的限制——不存在「必须先叫 openai_compatiable
才能用」这种约定。三个内置供应商（`deepseek`、`glm`、`kimi`）只是预先写好的
条目：有编译进去的端点与默认模型表，可以用同名条目覆盖。

本文是配置参考；TUI 向导的操作流程见[配置向导](#配置向导)一节。

## 一个条目长什么样

```json
{
  "llm": {
    "defaultProvider": "my-gateway",
    "defaultModel": "some-model",
    "providers": {
      "my-gateway": {
        "protocol": "openai_chat",
        "apiKey": "sk-...",
        "baseUrl": "https://gateway.example.com/v1",
        "models": ["some-model", "another-model"],
        "modelCapabilities": {
          "some-model": { "contextWindow": 131072, "maxOutputTokens": 8192 }
        }
      }
    }
  }
}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `protocol` | 否 | 线协议。见下节。省略时按名字推断，推断通常够用 |
| `apiKey` | **是** | 没有 key 的条目会被跳过（不报错，只是不进候选列表） |
| `baseUrl` | 视情况 | 非内置名字必须有。内置名字省略时用编译进去的端点 |
| `models` | **是** | 该 key 能访问的模型列表。对内置供应商也同样优先于内置表 |
| `modelCapabilities` | 否 | 每个模型的上下文窗口与最大输出。键必须出现在 `models` 里 |

规范化在每次读取与写入时都会做一遍：供应商名转小写并去首尾空白，`models` 去空、
去重、去首尾空白，`protocol` 别名归一到规范值。手写的配置与 Bruce 写出的配置
最终会收敛到同一种形状。

供应商名会被用作 `/model` 选择器的一段，也是 `setting.json` 里的对象键，所以限制
为 `^[a-z0-9][a-z0-9_.-]*$`（小写字母、数字、点、短横线、下划线），保存时自动转
小写。非法名字、未知协议值都会在启动时报错并指出具体字段，而不是悄悄按默认值处理。

> `protocol` 之所以拒绝未知值而不是回退：**协议决定 API key 发往哪个端点**。
> 拼错的 `"antrhopic"` 如果被当成 `openai_chat`，key 就会连同完全不同的请求体
> 一起发给一个从未打算使用的接口。

## 三个协议

| `protocol` | 请求 | 鉴权 | 端点路径（相对 `baseUrl`） |
|---|---|---|---|
| `openai_chat` | Chat Completions | `Authorization: Bearer` | `{baseUrl}/chat/completions` |
| `openai_responses` | Responses API | `Authorization: Bearer` | `{baseUrl}/responses` |
| `anthropic` | Messages API | `x-api-key` + `anthropic-version` | `{baseUrl}/v1/messages` |

路径是**幂等拼接**的：`baseUrl` 已经带上该路径时原样使用。所以以下写法都正确：

```jsonc
// 都是 POST https://api.example.com/v1/chat/completions
"baseUrl": "https://api.example.com"
"baseUrl": "https://api.example.com/v1"
"baseUrl": "https://api.example.com/v1/chat/completions"
```

Anthropic 的 `baseUrl` 习惯上不带 `/v1`（官方文档的 `https://api.anthropic.com`），
但带上 `https://api.anthropic.com/v1` 也是对的。

### 供应商名的别名

条目名本身也有别名：`zai` / `zhipu` / `bigmodel` 归一为 `glm`，`moonshot` /
`moonshotai` 归一为 `kimi`，`openai` / `compatible` 等归一为
`openai_compatiable`。**同一张表里的两个名字不能同时出现**（见「校验与错误」）。

### 接受别名

`protocol` 也接受若干别名，写哪个都行：

| 别名 | 归一化为 |
|---|---|
| `openai`、`openai_compatible`（含历史拼写 `openai_compatiable`）、`chat_completions` | `openai_chat` |
| `responses` | `openai_responses` |
| `anthropic_messages`、`claude` | `anthropic` |

### 省略 protocol 时的推断

| 条件 | 推断结果 |
|---|---|
| 名字是 `glm` / `deepseek` / `kimi`（含别名） | `openai_chat` |
| 名字含 `anthropic` 或 `claude` | `anthropic` |
| 名字含 `responses` | `openai_responses` |
| 其余 | `openai_chat` |

推断是为兼容旧配置而存在的：引入 `protocol` 字段之前写的 `setting.json` 没有它，
需要继续按原来的方式工作。新配置建议显式写下 `protocol`，向导也会总是写显式值。

### 三个协议的行为差异

同一个模型经不同协议访问，Bruce 内部会做相应适配，这些差异对使用者透明：

- **工具调用**：Chat Completions 用 `tool_calls`；Responses 用扁平的 `function`
  工具 + `function_call` / `function_call_output` 条目，按 `call_id` 关联；
  Anthropic 用 `{name, description, input_schema}` 与 `tool_result` 内容块。
- **系统提示**：Anthropic 把 system 消息提升为顶层 `system` 字段，不留在消息列表里；
  Responses 用 `instructions`。
- **prompt 缓存**：Anthropic 会在 tools 与 system 末端放置 `cache_control` 断点
  （共 2 个，上限是 4 个），命中后 `cache_read_input_tokens` 计入 usage；
  Responses 从 input 中减去 `cached_tokens`。
- **`max_tokens`**：Anthropic 必填。取值顺序是调用方指定 → `modelCapabilities`
  的 `maxOutputTokens` → 内置兜底值 8192。
- **图片输入**：只接受 `data:` URL（内联 base64），不接受远程图片 URL。是否开启
  按模型名判断：Anthropic 客户端对所有模型开启；Chat Completions 客户端对内置
  供应商按表判断（`glm-5v*` 与 kimi 全系开启，deepseek 关闭），其余名字看模型名
  是否含 `vision` / `vl`；Responses 客户端看是否含 `vision` / `vl` 或以 `gpt-` /
  `o` 开头。

## 内置名字与显式字段的相互作用

`deepseek` / `glm` / `kimi` 三个名字在**完全不带 `protocol` 与 `baseUrl`** 时使用
编译进去的客户端，连同它们的请求怪癖一起保留（DeepSeek 关闭 HTTP/2、GLM 对
`glm-5v*` 换用非 coding 端点、Kimi 按模型决定是否发送 `reasoning_effort`）。

只要给内置名字写上 `protocol` **或** `baseUrl` 中的任意一个，该条目就改走通用
客户端，上面这些怪癖不再生效——端点完全由你写的 `baseUrl` 决定。这是刻意的：
显式声明的字段应当压过编译进去的默认值。

由此有一个实用后果：向导保存的条目**总是**带显式 `protocol`，也要求填 `baseUrl`，
所以用向导编辑过的内置供应商会走通用客户端。要保留内置行为，就让内置条目保持
完全没有这两个字段（例如只靠环境变量启用）。

## 环境变量

三个内置供应商可以只靠一个环境变量启用，不需要在 `setting.json` 里写任何条目：

| provider | 环境变量 | 默认模型 | 端点 |
|---|---|---|---|
| `deepseek` | `DEEPSEEK_API_KEY` | `deepseek-v4.1-flash` | `https://api.deepseek.com` |
| `glm` | `GLM_API_KEY` | `glm-5.1` | `https://open.bigmodel.cn/api/coding/paas/v4` |
| `kimi` | `MOONSHOT_API_KEY` | `kimi-k3` | `https://api.moonshot.cn/v1` |

环境变量与 `setting.json` 的分工是「补候选」而不是「覆盖」：环境变量只会注册那些
在 `setting.json` 里没有显式出现的 provider；显式配置的条目始终优先，即使它的
`apiKey` 是空字符串，也不会被环境变量顶掉。只有当 `setting.json` 没有指定
`defaultProvider` 时，环境变量注册的 provider 才会成为默认。

## `modelCapabilities`

```json
{
  "modelCapabilities": {
    "some-model": { "contextWindow": 131072, "maxOutputTokens": 8192 }
  }
}
```

`contextWindow` 驱动自动压缩的阈值计算；`maxOutputTokens` 是请求里的输出上限
（Anthropic 协议下必填）。两个值都为 0 的条目等同于没写。

- 键**必须**同时出现在同一条目的 `models` 里，否则启动报错。这一条是刻意的：
  能力项指向一个声明之外的名字，几乎总是 `models` 与 `modelCapabilities` 改到
  一半的残留，静默保留会让上下文窗口来自一个不会被用到的模型。
- 未声明 `contextWindow` 的模型不参与阈值自动压缩，但 API 返回的显式上下文溢出
  仍会被识别并触发压缩。
- 对内置模型（如 `glm-5.1`），声明的值会**覆盖**编译进去的默认能力。

## 配置向导

TUI 里运行：

```text
/provider add            # 新建，打开向导
/provider edit <name>    # 修改已有条目
/provider                # 或 /provider list，列出全部条目
/provider remove <name>  # 删除
```

`/provider add` 与 `/provider edit` 只有 TUI 里有意义（运行时会提示这一点），其余
子命令在非 TUI 路径下也能用。

### 向导流程

`/provider add` 依次询问：

1. **Name** — 供应商名，即 `setting.json` 里的键。
2. **Protocol** — ←/→ 在三个协议间选择。若该名字的推断结果与你选的不同，界面会
   提示「这个名字在不指定协议时会被推断为 X」。
3. **Base URL** — 端点根地址，协议对应的路径会自动追加。
4. **API Key** — 输入时掩码显示；保存时只把掩码后的形式渲染到界面，完整值只写入
   `setting.json`。
5. **连通性测试** — 自动发起，同时就是模型列表发现。这一步等价于
   `GET {baseUrl}/models`（Anthropic 走 `{baseUrl}/v1/models`），用相同的鉴权头。
6. **选择模型** — 空格键勾选/取消，`a` 全选/全不选，↑/↓ 滚动；下方还有一个
   「Also add」输入框，可以手动补逗号分隔的模型名。
7. **确认** — 显示协议、URL、掩码后的 key 与所选模型数，回车保存。

探测成功后，端点公布的 `context_window` / `max_output_tokens`（Anthropic 侧是
`max_input_tokens` / `max_tokens`）会随勾选**自动写入 `modelCapabilities`**，
所以自定义供应商不需要手工填窗口就能启用自动压缩。

探测失败不阻断流程：错误原样显示，回车进入手工输入模型名，向导照常保存。

> Esc 返回上一步，在第一步（或探测步骤）关闭向导；Ctrl+C 直接关闭。修改已有条目
> 时留空 API Key 表示保留原值——向导读不到已存的 key，要求重输会让改一个 Base URL
> 变成必须重新提供凭据。

### 首次运行

`setting.json` 里没有任何可用供应商时，Bruce **不再拒绝启动**，而是照常进入 TUI
并自动打开向导。此后发消息前必须先配好一个供应商；未配置时发消息会得到
「no LLM provider is configured; run /provider add to set one up」。

### 保存做了什么

向导与 `/provider remove` 走同一条路径，顺序是固定的：

```text
重新读盘 → 改动副本 → 解析新的默认模型 → 内存中构建候选客户端
        → 写盘 → 读回校验 → 一次性替换运行中的客户端 → 重建 agent
```

关键点是**先构建、后写盘**：配置有问题（比如无效的压缩窗口）时磁盘与内存都不动。
写盘后还会读回一次，只有能解析回来的文件才会被采用；读不回来时用改动前的配置回滚。

新建的供应商会自动成为当前选择的模型；编辑时若原选择仍然存在就保持不动，否则
确定性地回退到该供应商的默认模型。

## `/model` 与带斜杠的模型 ID

`/model` 接受三种选择器：

```text
/model                              # 列出全部可用模型，当前项在最前
/model <provider>                   # 切到该供应商的默认模型
/model <provider>/<model>           # 精确切换
/model <model>                      # 模型名唯一时可直接用
```

注意**模型 ID 里自带 `/` 的情况**（例如网关的 `group/flash`）：解析器用
`SplitN(selector, "/", 2)`，所以完整选择器 `my-gateway/group/flash` 能正确解析，
但只输入 `group/flash` 会被当成 `provider=group, model=flash`。用 Tab 补全补出
完整选择器即可避免这个歧义。

## 校验与错误

启动时下列情况会直接报错（不是静默跳过）：

- 供应商名不匹配 `^[a-z0-9][a-z0-9_.-]*$`
- `protocol` 是不认识的值（错误信息列出全部合法值）
- `modelCapabilities` 的键不在 `models` 里，或键为空
- **两个条目名解析到同一个供应商**，例如同时写了 `kimi` 与 `moonshot`（或
  `glm` 与 `zai`）。一个规范名只能有一个端点与一份凭据，两者并存时哪一份生效
  取决于 map 迭代顺序，可能把其中一个条目的 API key 发到另一个条目的端点。
  报错会点名冲突的两个条目，合并成一条即可。

静默跳过的只有「条目本身不可用」：没有 `apiKey`、没有 `baseUrl`（且名字非内置）、
拿不到任何模型。被跳过的条目不会出现在 `/model` 列表里，但仍留在 `setting.json`
中——补上缺的字段就能直接用。

## 凭据安全

API key 只应到达用户配置的那个端点。围绕这条规则的行为：

- **跨主机重定向被拒绝**。`Authorization` 在 Go 标准库里会在跨主机重定向时被
  剥离，但 `x-api-key`（Anthropic 头）不在标准库的敏感名单里，会被原样转发。
  因此所有客户端统一设置 `CheckRedirect`：重定向必须停在同一个 Host（含端口），
  否则报错而不是跟随。同一主机上的重定向（例如网关补尾斜杠）正常跟随。
- **远程主机的明文 `http://` 被拒绝**。key 会随每个请求发往该端点，明文连接上
  路径中任何一环都能读到。`localhost`、`127.0.0.1`、`::1` 等 loopback 地址豁免
  ——流量不出本机。这条只在向导里检查；直接写 `setting.json` 不受限制（自己写
  的配置自己负责），但客户端仍受上面的重定向规则约束。
- **`setting.json` 以 `0600` 写入**，每次保存后显式收紧权限（`WriteFile` 的
  mode 参数只在创建文件时生效）。目录仍是 `0755`。
- **上游错误文本会脱敏后再显示**。网关拒绝 key 时常在响应体里把它原样引回来
  （`invalid api key: sk-...`），这段文本会进 TUI 和 session 记录，因此在格式化
  为错误消息时按凭据形状脱敏。`APIError.Body` 字段本身保持原文，溢出检测读的是它。

## 另见

- [README](../README.md) —— 快速上手与完整的 `setting.json` 示例
