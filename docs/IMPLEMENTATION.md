# Go Harness 实现说明

> 状态：**M0-M4 已跑通**（内核 / 会话 / LLM / 工具 / Agent 循环 / CLI），能真实调用模型读写本地文件。
> 设计文档：[ARCHITECTURE.md](ARCHITECTURE.md) ｜ 实测时间：2026-10-30

## 1. 现在能做什么

~~~bash
# 构建（Go 1.27.3，装在 ~/sdk/go，未进系统 PATH）
export PATH=$HOME/sdk/go/bin:$PATH
cd ~/go-harness && make build

# 检查配置（不发请求）
./bin/gh run "任意任务" --dry-run

# 真跑
export DEEPSEEK_API_KEY=sk-...
./bin/gh run "在当前目录创建 hello.txt 并读回确认" --workdir /tmp/demo

# 看工具与 schema
./bin/gh tools

# 回放事件日志（事件溯源的最小可用形态）
./bin/gh replay /tmp/ghsess1
~~~

## 2. 实测证据（不是「应该能跑」）

真实调用 DeepSeek（deepseek-chat），任务「创建 hello.txt → read 读回 → 一句话总结」：

~~~
→ deepseek-chat · 会话 /tmp/ghsess1
  4 assistant/message step=1 finish=tool-calls calls=[write]
  5 tool/result write(...) err=false "已写入 hello.txt（19 字节）"
  7 assistant/message step=2 finish=tool-calls calls=[read]
  8 tool/result read(...)      err=false "1 hi from gh harness"
 10 assistant/message step=3 finish=stop "我在当前目录创建了 hello.txt ..."
 11 turn/end reason=completed steps=3 2.455s

← completed · 3 步 · 3565 tokens(prompt 3437 / completion 128) · 2.455s
~~~

文件确实落地（`cat /tmp/ghtest/hello.txt` → `hi from gh harness`），事件 11 条，`gh replay` 能把它折回 6 条消息的对话视图。

代码规模：**2880 行生产代码 + 1373 行测试（50 个测试函数）**，零第三方依赖（纯标准库），全量 `go test ./...` 通过。

### 2.1 多轮会话延续（同一个会话目录，两次独立进程）

~~~bash
./bin/gh run "用 bash 数一下当前目录有几个 .txt 文件" --workdir /tmp/ghtest2 --session /tmp/ghsess2
./bin/gh run "刚才你数出来是几个文件？只回答数字。"        --workdir /tmp/ghtest2 --session /tmp/ghsess2
~~~

结果：第二轮不需要重新数文件就答对了，因为它从事件日志恢复了历史。

~~~
  8 turn/end  reason=completed steps=2 1.543s     ← 第 1 轮
  9 turn/start                                    ← 第 2 轮（turn 编号自动延续）
 10 user/message
 11 request/header  msgs=5                        ← 历史被折叠成 5 条消息喂回模型
 12 assistant/message step=1 finish=stop text="2"
 13 turn/end reason=completed steps=1 915ms
~~~

这一条是「事件溯源真的能用」的证据：进程重启、事件追加、视图折叠、turn 计数恢复，全部走的是同一份 JSONL。

## 3. 已实现清单（对应设计文档的层）

| 层 | 内容 | 代码 | 行数 |
|---|---|---|---|
| L0 内核 | 服务注册表、`Get[T]`/`Must[T]`、插件 `Provides/Requires` 稳定拓扑排序、成环/缺依赖启动期报错、LIFO 关闭 | `pkg/kernel` | 227 |
| L1 会话 | JSONL append-only、单写者 + `Sync`、崩溃残行自愈截断、`surfaceOp replace` 折叠 | `pkg/session` | 337 |
| L2 领域 | `Block` 接口 + text/reasoning/image/tool-call/tool-result/未知块透传、`FinishReason`、`TurnEndReason` 闭集 | `pkg/domain` | 137 |
| L3 LLM | `Adapter` 接口、`iter.Seq2` 流、错误闭集（AUTH/QUOTA/RATE_LIMIT/INVALID_REQUEST/CONTEXT_OVERFLOW/TIMEOUT/UNAVAILABLE）、重试白名单 + Retry-After、SSE 解析器、tool-call 增量装配 | `pkg/llm` | 308 |
| L3 适配器 | OpenAI Chat Completions 兼容（DeepSeek/OpenRouter/vLLM），含 `reasoning_content`、usage、错误映射、`HealthCheck` | `pkg/llm/adapters/openai` | 362 |
| L5 工具 | 注册表、守卫链、JSON 校验、超时、UTF-8 安全截断、按反射生成 JSON Schema（`Define[T]`） | `pkg/tools` | 323 |
| L5 内置工具 | read/write/edit/glob/grep/bash + 工作区路径约束（含符号链接逃逸拒绝） | `pkg/tools/builtin` | 499 |
| L4 Agent | Turn/Step 循环、四维预算里的步数预算、重试、只读工具并发、串行工具独占、终态恰好一条 `turn/end`、取消写 aborted | `pkg/agent` | 317 |
| L8 CLI | `run`/`replay`/`tools`/`version`、流式输出、flag 与位置参数任意顺序、SIGINT 取消 | `cmd/gh` | 370 |

## 4. 与设计文档的偏差（如实记录）

| 设计里写的 | 实际做的 | 原因 |
|---|---|---|
| YAML profile + bundle + patch 配置 | 先用 CLI flags + 环境变量 | 保 M0-M4 不引入 YAML 依赖；配置合并是 M5 的活 |
| 上下文压缩三层 + 自动触发 | 只实现了 `surfaceOp replace` 的事件折叠与摘要占位 | 压缩是 M6；但存储与视图已经准备好 |
| 进程外插件 gRPC / WASM | 只做链接期注册 | 按你的决定：不要求运行期上下线 |
| subagent、PTC(run_code)、MCP | 未做 | M7-M11 |
| React UI | 未做（也是你的决定：UI 不用 Go） | UI 单独排期 |
| 沙箱 seam（OS 级） | 只有工作区路径约束；`bash` 未隔离 | 真实隔离需要 Seatbelt/容器，M8 一起做 |

## 5. 开发中真实踩到的两个坑（都补了回归测试）

1. **`iter.Seq2` 的 yield 签名**：`yield(chunk)` 编译不过，必须 `yield(chunk, nil)`；错误路径要保留 `yield(zero, err)`。
   一度用全局替换把错误分支也改坏了，靠 `go vet` 抓回来。
2. **tool_calls 的 wire 格式**：把「工具定义」（`parameters`）和「工具调用」（`arguments`）共用了同一个 struct，
   发给 DeepSeek 时 `messages[2].function.arguments` 缺失 → **HTTP 422**，turn 直接失败。
   修复：拆成 `wireToolDefFunc` / `wireCallFunc`，并加 `TestAssistantToolCallsSerializeArgumentsAsString` 钉住格式。

第 2 个坑说明了设计文档里那句「适配器要契约测试先行」不是空话：真供应商比任何 mock 都严格。

## 6. 测试矩阵（50 个）

| 包 | 覆盖点 | 数量 |
|---|---|---|
| `pkg/kernel` | 拓扑排序、缺依赖、成环、LIFO 关闭、重复服务 panic、类型不匹配 | 6 |
| `pkg/session` | 追加+折叠、重开恢复 seq、残行截断自愈、surfaceOp 替换 | 3 |
| `pkg/llm` | SSE（CRLF/注释/多行/无空行/逐字节分包）、tool-call 装配、错误归类、重试退避 | 11 |
| `pkg/llm/adapters/openai` | 文本+推理+usage、工具分片、429/400 错误、请求体翻译、`arguments` 回归 | 5 |
| `pkg/tools` | schema 生成、未知字段拒绝、重复注册、截断、守卫、超时、Specs 稳定 | 10 |
| `pkg/tools/builtin` | 读写往返、分页、二进制/目录拒绝、edit 唯一性、glob/`**`、grep include、bash 退出码/超时、沙箱逃逸（含符号链接） | 8 |
| `pkg/agent` | 纯文本、工具往返、最大步数 blocked、可重试 vs 不可重试、取消 aborted、system 提示、**端到端（假供应商 + 真工具 + 真事件）** | 9 |

其中 2 个是端到端：`TestEndToEndWriteThenAnswer`（SSE 假供应商 → 真适配器 → 真 write 工具 → 真事件存储 → 折叠回对话）与
`TestEndToEndToolErrorIsFedBack`（工具失败不炸 turn，而是作为结果回给模型）。

## 7. 下一步建议（按性价比排序）

1. **M5 配置**：`profiles/*.yaml` + bundle/patch 合并 + 启动期 schema 校验（当前 flags 足够自用，但不便于多人/多环境）。
2. **M6 压缩**：结果剪枝 + 滑窗 + 摘要；`CONTEXT_OVERFLOW` 时自动压缩并重试同一 step（现在会直接失败）。
3. **M8 沙箱 + PTC**：`bash` 接 macOS Seatbelt 或容器；`run_code` 用子进程 + JSON-RPC 工具桥。
4. **M7 subagent**：谱系头（`parentSession`/`delegationDepth`）已经能在 `SessionHeader` 里表达，事件层不用改。
5. **M9 UI**：只做 SSE + 一份 React，不要为了「全 Go」牺牲体验。

## 8. 安全与限制（重要）

- `bash` 能执行任意命令：**当前只有工作区路径约束，没有 OS 级隔离**。在不可信输入场景必须接沙箱。
- API key 只从环境变量读，不落盘、不进事件日志（`request/header` 只记 provider/model/哈希）。
- 工具输出默认 32 KB 截断，避免把上下文一次性打爆。
- 会话目录默认 `~/.go-harness/sessions/<时间戳>`，可随时 `gh replay` 审计。
