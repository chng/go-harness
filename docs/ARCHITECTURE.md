# Go Harness 技术方案（参考 DSH 架构，自顶向下拆解）

> 版本 v0.1 ｜ 编制日期 2026-10-23 ｜ 目标读者：架构/后端/平台工程
> 参考实现：@deepseek-ai/dsh v0.1.2-rc.1（本地实测测绘：222 个包，核心约 13.9 万行 JS，UI 约 7.4 万行 TS）

> **适用范围修订（v0.2）**：若这份框架要落在「后端业务系统」而不是本机编码助手，请先读
> [BACKEND.md](BACKEND.md)：那份文档重估了存储、执行模型、策略/审批/幂等、模型网关、
> 可观测与评测，并给出三档交付与迁移路线。本文的 L0-L10 分层依然成立，但「文件存储 +
> 阻塞式 Run + 无策略工具执行」这三处在后端场景必须替换。

---

## 目录

0. 结论摘要
1. 可行性评估（机制逐项对照）
2. 目标与非目标
3. 总体架构（L0-L9 分层 + 数据流 + 不变量）
4. L0 运行时内核
5. L1 会话存储（事件溯源）
6. L2 领域模型
7. L3 LLM 适配层
8. L4 Agent 循环
9. L5 工具系统（含 PTC）
10. L6 上下文装配与压缩
11. L7 扩展与配置
12. L8 传输与接口
13. L9 客户端与 UI
14. L10 可观测性与测试
15. 决策矩阵
16. 目录结构
17. 里程碑与工时
18. 风险登记与缓解
19. 与 DSH 的差异清单
20. 附录 A：DSH 222 包 → Go 模块映射
21. 附录 B：第一周起步清单
22. 结论

## 0. 结论摘要

| 问题 | 结论 |
|---|---|
| 技术上可行吗 | **可行**。事件溯源会话 + 工具管线 + 多模型适配 + 插件扩展点这四件事在 Go 里都有等价物，且 Go 在并发、单二进制分发、内存占用上优于 Node |
| 能 1:1 复刻吗 | **不能，也不建议**。三个机制必须换实现：①运行时动态插件 → 进程外插件/WASM；②AsyncIterable 流 → iter.Seq2 + context；③类型级 merge-extensible 映射 → 接口注册表 |
| 代价最大的部分 | ①插件生态不通用（现有 JS 插件不能直接跑）②UI 客户端插件机制要重做 ③热加载能力降级 |
| 工作量量级 | 核心 MVP（可跑通 agent 循环+工具+会话）≈ 10 周单人；对齐全功能 ≈ 9-12 人月；含 UI 完整对齐 ≈ 12-18 人月 |
| 建议路线 | 先做「内核 + 会话 + 工具 + LLM 适配 + CLI」的可扩展骨架（M0-M4）；UI 保留 TypeScript；进程外插件用 gRPC/WASM，不要用 Go plugin 包 |
| 最大风险 | 与 DSH 的行为兼容（会话格式、事件语义）与模型协议演化速度；用「读写兼容层 + 协议一致性测试」兜住 |

---

## 1. 可行性评估

### 1.1 机制逐项对照（可行性判断的核心）

| # | DSH 机制（JS/TS） | 作用 | Go 等价方案 | 可行性 | 主要损失 |
|---|---|---|---|---|---|
| 1 | Cordis 运行时：ctx 服务容器、inject 依赖声明、插件 apply(ctx)、effect 释放钩子 | 一切能力都是插件，按依赖拓扑加载/卸载 | 静态服务注册表 + 类型化 Context 访问器；插件声明 Requires()，注册表按依赖序初始化；Lifecycle 接口管 Close | 高 | 失去运行期任意增删服务；用进程外插件补齐 |
| 2 | 动态插件加载：require 任意 npm 包、profile 组合、patch 覆盖 | 生态化扩展 | 三选一：①链接期注册（init() 注册进 Registry）②进程外插件（gRPC over stdio）③WASM（wazero） | 中高 | Go plugin 包跨平台不可用（macOS 支持差、版本锁定），故不走；推荐①+② |
| 3 | 流式：AsyncIterable<StreamChunk> + AbortSignal | 模型增量输出、工具流式 | iter.Seq2[Chunk, error]（Go 1.23）+ context 取消 | 高 | 无（Go 更省内存） |
| 4 | 类型级扩展：ContentBlockMap / FinishReasonMap 用 TS 接口合并 | 插件扩展新内容块类型而不改核心 | 接口 + 注册表：type Block interface{ BlockType() string } + blockRegistry；解析处类型断言 + 兜底 | 中 | 编译期联合穷尽检查弱于 TS；靠「未知块透传」兜底 |
| 5 | 配置组合：profile → bundles → cordis.patch.yml → --patch 多层合并 | 一套代码多种部署形态 | 纯配置合并：YAML 多文档深度合并 + 优先级链 + 校验（与语言无关，可 1:1 复刻） | 高 | 无 |
| 6 | 热加载 HMR：cordis-plugin-hmr 热替换插件 | 改插件不重启 | 进程外插件可热重启；同进程插件不可热替换 → 接受重启（air/watchexec 补偿） | 中低 | 开发体验下降 |
| 7 | 会话事件溯源：append-only 日志 + seq 游标 + surfaceOp 压缩 | 可恢复、可分叉、可审计、上下文可压缩 | 完全可复刻，Go 实现更简单（jsonl+zstd 或 sqlite） | 高 | 无 |
| 8 | 工具管线：注册表 + JSON Schema + 执行/超时/输出上限/溢写 | 模型调用外部能力 | 反射生成 JSON Schema（invopop/jsonschema）+ 管线中间件 | 高 | 无 |
| 9 | PTC / run_code：模型写代码调用工具（worker_thread 隔离） | 强表达力的工具编排 | 子进程 worker（Python/Node）+ JSON-RPC 工具桥；或内嵌 goja | 中高 | 无 OS 级隔离则安全性差，必须配 sandbox |
| 10 | 类型化 RPC：typert 生成 host/client 双向类型 | 前后端强一致 | Connect-RPC（buf + protobuf）：HTTP/JSON + SSE + WS 一套 | 高 | 需引入 protoc/buf 工具链 |
| 11 | 凭据/审计/沙箱/审批等 seam（接口层） | 安全边界 | 同样做成 interface + 默认实现 + 可插拔后端 | 高 | 无 |
| 12 | 客户端插件：window.__ModuleLoader__ 槽位挂载 | UI 可扩展 | 保留 TS/React，做同样的「槽位注册表 + 动态 import」 | 中高 | UI 需重写一套（Go 侧无改动） |

### 1.2 Go 的收益与代价

**收益**

- 单二进制分发（CGO_ENABLED=0），部署即拷贝；Node 需 node_modules 与运行时
- 内存与启动：同等 agent 循环下常驻内存约为 Node 的 1/3-1/2，冷启动毫秒级
- 并发模型：工具并发、多会话、SSE 广播天然用 goroutine，不存在事件循环阻塞
- 静态类型 + 显式错误处理，长跑稳定性更好；context 取消贯穿全链路

**代价**

- 插件生态完全不能复用：DSH 的 222 个包、社区 JS 插件、客户端插件都要重写或走进程外桥
- 动态性下降：无法运行期加载任意模块；服务增删需重启或走外部进程
- 热更新只能进程级重启
- UI 仍是 TS/React。若要「全 Go」，WASM 前端（templ+HTMX 等）会牺牲交互体验，不建议

### 1.3 结论（分档）

| 目标 | 可行性 | 规模 | 建议 |
|---|---|---|---|
| A. 可扩展 harness 内核（会话/工具/LLM/循环/CLI） | 高 | ~1.5-2 万行 Go，单人 10 周 | 强烈推荐先做这档，能立刻产出可用的自有框架 |
| B. 对齐全功能（多传输/审批/沙箱/压缩/子代理/技能/凭据） | 中高 | ~5-7 万行 Go，3-6 人月 | 二阶段按业务增量做，不必全做 |
| C. 含 Web UI + 客户端插件 + 兼容现有 JS 插件 | 中低 | 再加 UI 7 万行 TS + JS 宿主桥，12-18 人月 | 不建议追平；如确需，用「进程外 JS 宿主（goja/node 子进程）」做适配层 |

---

## 2. 目标与非目标

### 2.1 目标

1. **事件溯源**：所有会话状态由 append-only 事件推导，可恢复、可分叉、可审计、可重放
2. **能力即插件**：LLM 适配器、工具、存储、沙箱、传输、上下文装配都是注册进来的扩展点
3. **多模型中立**：模型能力（上下文长度/图片/推理强度/工具调用）由适配器声明，上层不感知厂商差异
4. **可裁剪**：同一份核心代码，通过配置组合成 CLI（headless）、HTTP 服务、Web UI、定时任务四种形态
5. **可观测**：每次模型请求留快照（config/system/tools），支持事件级回放与指标
6. **安全边界显式化**：文件系统、进程执行、凭据、审批都是 seam，默认收紧、按需放开

### 2.2 非目标

- 不做 JS 插件兼容层（除非确有需求，走进程外桥）
- 不追求与 DSH 的 wire 协议字节级兼容（只做会话文件格式的读写兼容，便于迁移已有数据）
- 不做通用工作流编排器（DAG/调度留给 cron/K8s Job 等外部系统）
- 第一期不做多租户 SaaS（先单用户/单机，鉴权留 seam）

---

## 3. 总体架构

### 3.1 分层（自顶向下）

~~~
L9 客户端/UI      React + 客户端插件槽位（TS，独立仓）
L8 传输与接口     CLI(headless/stream) · HTTP/SSE · WS · Connect-RPC
L7 扩展与配置     profile/bundle/patch 合并 · settings · credentials
                  · hooks · 进程外插件(gRPC/WASM) 宿主
L6 上下文装配     System Prompt 分节 · 技能目录 · 上下文文件
                  · 会话压缩(prune/summarize) · 上下文窗口预算
L5 工具系统       Registry · JSON Schema · 执行管线(超时/取消/限额/审计)
                  · 结果溢写 · PTC(run_code) · 审批交互
L4 Agent 循环     Turn/Step 语义 · 工具调度 · 重试 · 预算 · 取消
L3 LLM 适配层     Adapter 接口 · 流式分块 · tool-call 增量拼装
                  · 重试/限流 · 模型目录与能力声明
L2 领域模型       ContentBlock/Message/SessionEvent/Turn/Step/Usage
L1 会话存储       事件日志(append-only) · seq 游标 · surface 视图
                  · 原子写 · 恢复/分叉 · jsonl+zstd（兼容）/sqlite
L0 运行时内核     Runtime · Context · Registry · 生命周期 · 事件总线
                  · 配置 · 日志/追踪 · 错误模型
~~~

### 3.2 一次请求的数据流

~~~
用户输入
  ↓
[L8] 传输层收到 UserInput ──► [L1] 追加 user/message 事件（得到 seq）
  ↓
[L4] AgentLoop.Turn()
  ├─► [L6] 装配上下文：system sections + skills + 历史(surface 视图) + 预算裁剪
  ├─► [L3] LlmRuntime.Stream(opts) ──► 先写 request/header（config/system/tools 快照）
  │        流式分块：text/reasoning 增量 → assistant/chunk
  │                  tool-call 增量拼装完整 → tool/call
  ├─► 若 finish=tool-calls：[L5] ToolRegistry.Execute
  │        审批 gate → 执行(超时/输出上限/溢写) → 追加 tool/result
  └─► 循环(step++) 直到 finish=stop / 预算耗尽 / 被取消
  ↓
[L1] turn/end(reason) ──► 可选触发 [L6] 压缩 → surfaceOp replace[start,end]
~~~

### 3.3 五条不可动摇的不变量

| # | 不变量 | 为什么 |
|---|---|---|
| I1 | 会话只追加不改写；压缩/重写表达为 surfaceOp replace(start,end) 视图叠加 | 可审计 + 可回放；模型看视图，历史永远在 |
| I2 | 每次模型请求都留快照（provider/model/config/system/tools 哈希） | 排查「同样输入为何这次答得不一样」的唯一手段 |
| I3 | 工具结果独立成事件，带 isError 与原始 callId | 支持结果级溢写、截断、重放，失败后精确续跑 |
| I4 | 所有 I/O 走 seam 接口（fs/exec/net/clock/credentials/approval） | 测试可注入假实现；安全策略可整体替换；禁全局单例 |
| I5 | 取消贯穿全链路（context 从传输层一路传到适配器与工具） | 长任务必须可中断，否则一个卡住的重试拖垮进程 |

---

## 4. 拆解 L0：运行时内核

### 4.1 职责

服务容器、生命周期、依赖解析、事件总线、配置、错误模型、日志/追踪。不含任何业务。

### 4.2 关键设计

**(1) Context 采用「类型化访问器 + 可选服务」，而不是 JS 的动态属性代理**

~~~go
// core/context.go
type Context struct {
    ctx     context.Context     // 取消/超时/值
    rt      *Runtime
    log     *slog.Logger
    slots   map[reflect.Type]any // 服务槽位：按接口类型索引
    dispose []func() error       // 逆序释放，等价 cordis effect
}

func Get[T any](c *Context) (T, bool)   // 缺失返回 (zero,false)，由调用方决定降级或致命
func Must[T any](c *Context) T          // 只在插件 Apply 阶段用，缺依赖即启动失败
~~~

**(2) 插件声明依赖，注册表拓扑排序（等价 cordis inject）**

~~~go
type Plugin interface {
    Name() string
    Requires() []string       // 依赖的服务名
    Apply(*Context) error     // 只在这里取服务、注册能力
    Close() error             // 可选，对应 cordis dispose
}
~~~

**(3) 事件总线：进程内、topic 前缀匹配、有序、背压可控**

~~~go
type Event struct {
    Topic string    // "session.event" / "llm.request" / "tool.result"
    Time  time.Time
    Data  any
}
type Bus interface {
    Publish(Event)
    Subscribe(topic string) (<-chan Event, func())  // 返回取消函数
}
~~~

### 4.3 验收标准

- 插件循环依赖 → 启动期报错并指出环
- 缺少必需服务 → 启动失败，错误信息含「谁需要谁」
- 任一插件 Close 失败不阻塞其他插件释放（记录并继续）
- 单测可注入假 Clock/Bus，不依赖真实时间

---

## 5. 拆解 L1：会话存储（事件溯源）

### 5.1 地基

DSH 实测行为：SessionEvent{type,seq,time,data} 追加到 session.jsonl.zstd；seq 单调递增；压缩不改历史，
而写入 surfaceOp{op:"replace",start,end}，读时把 [start,end) 从视图剔除/替换。
SessionHeader{version,id,createdAt,cwd,parentSession,origin:'subagent',delegationDepth,agentPreset}。

### 5.2 Go 侧接口

~~~go
type Event struct {
    Seq     uint64     `json:"seq"`
    Type    string     `json:"type"`   // turn/start, step/start, assistant/chunk, tool/call ...
    Time    int64      `json:"time"`
    Data    any        `json:"data"`
    Surface *SurfaceOp `json:"surfaceOp,omitempty"`
}

type Store interface {
    Header() Header
    Append(ctx context.Context, events ...Event) (lastSeq uint64, err error) // 单写者、批量原子
    Read(ctx context.Context, from, to uint64) ([]Event, error)             // [from,to)
    View(ctx context.Context) ([]Event, error)                              // 应用 surfaceOp 后的有效历史
    Close() error
}
~~~

### 5.3 实现要点（踩坑预警）

| 点 | 做法 | 原因 |
|---|---|---|
| 原子写 | 写临时文件 + fsync + rename | 本项目已验证：非原子写会让并发读读到半个 JSON，直接导致请求崩 |
| 分段 | zstd 流式写 + 定期 seal 成 segment，index 记录段尾 seq | 单文件无限增长会让恢复变慢 |
| 并发 | 单写者（mutex 或单 goroutine 串行）+ 多读者；读走内存尾缓存 | 避免 seq 冲突 |
| 恢复 | 启动校验尾部 CRC，坏尾截断到最后一个完整事件 | 断电后仍可用 |
| 分叉 | Fork(parentSeq) → 新文件，Header.parentSession 指向父，seed 复制前缀 | 支持「从第 N 步重来」 |
| 回放 | 纯函数 Replay(events) → State | 保证事件是唯一真相，便于测试 |
| 兼容 | 提供 jsonl.zstd 编解码器，可只读解析 DSH 会话文件 | 便于迁移已有数据 |

### 5.4 存储后端选型

| 后端 | 适用 | 说明 |
|---|---|---|
| jsonl+zstd（默认，兼容 DSH） | 单机、可被外部工具读 | 实现最简单，可直接 grep/回放 |
| sqlite（可选，推荐生产） | 需按 session/时间检索、事务、并发读 | events(session_id,seq,type,time,blob) + 索引，WAL 模式 |
| 对象存储（S3/OSS，可选） | 归档与多机 | 只追加段文件，本地 sqlite 做索引 |

---

## 6. 拆解 L2：领域模型

### 6.1 内容块（对应 DSH 的 ContentBlockMap）

~~~go
type Block interface{ BlockType() string }

type TextBlock       struct{ Text string }
type ReasoningBlock  struct{ Text string }
type ImageBlock      struct{ Attachment AttachmentRef }
type ToolCallBlock   struct{ ID, Name string; Arguments json.RawMessage }
type ToolResultBlock struct{ ToolCallID string; Content []Block; IsError bool }

var blocks = registry.New[Block]("content-block")
func init() {
    blocks.Register("text", func() Block { return &TextBlock{} })
    blocks.Register("reasoning", func() Block { return &ReasoningBlock{} })
    // 插件可继续注册
}
~~~

**未知块策略**：解析到未注册类型 → 保留原始 JSON（RawBlock），转发模型时跳过或降级为文本摘要，绝不 panic。

### 6.2 会话事件类型（v1 最小集）

| 事件 | data 关键字段 | 说明 |
|---|---|---|
| turn/start | turn | 一轮开始 |
| step/start | turn, step | 一次模型调用开始 |
| request/header | provider, model, config, systemHash, toolsHash, startsSeries? | 每次请求的快照（不变量 I2） |
| assistant/chunk | turn, step, chunk{kind,text,toolCallDelta} | 增量（可批量合并落盘） |
| assistant/message | turn, step, message | 完整消息（与 chunk 冗余，便于快速读） |
| tool/call | turn, step, callId, name, arguments | 模型请求工具 |
| tool/result | turn, step, message, isError? | 工具结果 |
| step/end | turn, step, usage?, interrupted? | 结束 |
| turn/end | turn, reason{completed/aborted/blocked/error/max-tokens/interrupted} | 结束原因闭集 + 可扩展 |
| compaction/prune · session/title · surface | — | 元数据与压缩 |

### 6.3 会话头与「为什么需要谱系」

Header{Version,ID,CreatedAt,CWD,ParentSession,Origin,DelegationDepth,AgentPreset}。
subagent 派生会话需要谱系，否则审计时看不出「这条消息是主 agent 还是第 2 层代理说的」。

---

## 7. 拆解 L3：LLM 适配层

### 7.1 DSH 的抽象（实测）

LlmRuntime 暴露：`registerAdapter(providers[], adapter)` / `listProviders()` / `listModels(provider)` /
`resolveModelInfo(provider,model)` / `resolveCallConfig` / `prepareCall` / `stream(opts): AsyncIterable<StreamChunk>`
/ `providerRetryPolicy` / `discoverModels`。适配器只需实现 `stream(opts)`。
错误面：`LlmError{code, status, providerRetryAfterMs, requestId}` —— **code 是稳定路由键**（重试/降级依据），message 给人看。

### 7.2 Go 侧接口

~~~go
type Adapter interface {
    Provider() string
    // 能力声明：上层据此决定是否允许图片/推理强度/工具
    Capabilities(model string) (Caps, error)
    Stream(ctx context.Context, req Request) iter.Seq2[Chunk, error]
    ListModels(ctx context.Context) ([]ModelInfo, error) // 可选，返回 ErrNotImplemented 则用静态目录
}

type Runtime struct {
    adapters map[string]Adapter
    catalog  *Catalog        // 模型元数据：上下文长度/最大输出/模态/价格
    retry    map[string]RetryPolicy
}

type Chunk struct {
    Kind      ChunkKind      // TextDelta | ReasoningDelta | ToolCallDelta | Usage | Finish | Error
    Text      string
    ToolCall  *ToolCallDelta // {Index, ID, Name, ArgsDelta}
    Usage     *Usage
    Finish    *FinishReason
}
~~~

### 7.3 必做的四件工程活

| # | 活 | 细节 | 为什么容易翻车 |
|---|---|---|---|
| 1 | **tool-call 增量拼装** | 流式返回的 tool call 是分片来的：先 id+name，再参数 JSON 分片；必须按 index 归一化、字符串拼接、最后 JSON 校验 | 参数分片在 UTF-8/转义中间切断；不做缓冲会拼出坏 JSON |
| 2 | **SSE 解析健壮性** | 按空行分隔事件、忽略注释行、处理 CRLF/半包、超时与心跳 | 半包与分块边界是最常见的线上故障 |
| 3 | **错误归类** | 把 HTTP/协议错误映射成闭集 code：QUOTA / RATE_LIMIT / INVALID_REQUEST / CONTEXT_OVERFLOW / TIMEOUT / UNAVAILABLE | 上层要按 code 决定「重试/降级模型/压缩上下文/直接失败」 |
| 4 | **重试与抖动** | provider 级 retryPolicy（次数/退避/可重试 code 白名单），尊重 providerRetryAfterMs | 无白名单会把 400 也重试 5 次，白白烧钱 |

### 7.4 适配器分批落地

| 批次 | 适配器 | 说明 |
|---|---|---|
| P0 | OpenAI Chat Completions 兼容（含 DeepSeek/通义/Kimi/自建 vLLM） | 覆盖 80% 场景 |
| P0 | Anthropic Messages | 企业常备 |
| P1 | OpenRouter 这类「一条路由多模型」的兼容网关 | 注意 provider 路由与模型目录要分层 |
| P2 | 本地推理（llama.cpp server / Ollama） | 只需 openai-compatible + 关闭部分能力 |

---

## 8. 拆解 L4：Agent 循环

### 8.1 语义（对齐 DSH）

- **Turn**：一次用户交互。**Step**：Turn 内的一次模型调用 + 可能的工具执行。
- Turn 结束原因闭集：completed / aborted（user/parent/hook/disposed）/ blocked / error / max-tokens / interrupted。
- 循环终止条件：finish=stop、预算耗尽（步数/token/时间/费用）、被取消、无工具可调、策略要求停机。

### 8.2 骨架

~~~go
func (a *Agent) Turn(ctx context.Context, input UserInput) error {
    t := a.store.BeginTurn(ctx, input)          // 追加 turn/start + user/message
    for step := 0; ; step++ {
        if err := a.budget.Check(step); err != nil { return t.End(ctx, ReasonFrom(err)) }
        msgs, err := a.assembler.Build(ctx, t)   // L6
        if err != nil { return t.End(ctx, ErrReason(err)) }
        resp, err := a.callModel(ctx, t, step, msgs) // L3，内部自动写 request/header + chunk
        if err != nil { return t.End(ctx, ErrReason(err)) }
        if resp.NeedsTools() {
            if err := a.runTools(ctx, t, step, resp.ToolCalls); err != nil { return t.End(ctx, ErrReason(err)) }
            continue
        }
        return t.End(ctx, ReasonCompleted)
    }
}
~~~

### 8.3 必须解决的五件事

| # | 问题 | 方案 |
|---|---|---|
| 1 | **工具并发与顺序** | 默认可并发；声明 Serial 的工具（有副作用/需顺序）单独串行；结果按 callId 回填，顺序与调用顺序一致 |
| 2 | **取消传播** | Turn 级 ctx；工具拿到派生 ctx；取消后仍要写 turn/end(aborted) |
| 3 | **上下文溢出** | 遇到 CONTEXT_OVERFLOW → 触发压缩（L6）→ 重试同一 step，最多 N 次 |
| 4 | **失败续跑** | 工具执行到一半崩溃 → 恢复时凭 tool/result 事件判断哪些已成功，只重跑缺的 |
| 5 | **预算与熔断** | 步数/token/费用/墙钟四维预算；超限写 blocked 并保留可续跑状态 |

---

## 9. 拆解 L5：工具系统

### 9.1 定义与注册

~~~go
type Tool struct {
    Name        string
    Description string
    Schema      json.RawMessage        // JSON Schema（由 Args struct 反射生成）
    Execute     func(ctx context.Context, raw json.RawMessage) (Result, error)
    ReadOnly    bool
    Serial      bool
    MaxOutput   int                    // 字节上限，超出则溢写
}

type Registry struct{ /* ... */ }
func (r *Registry) Register(t Tool) error
func (r *Registry) Execute(ctx context.Context, call ToolCall) (Result, error)
~~~

### 9.2 执行管线（中间件链，顺序即语义）

~~~
审计(开始) → 权限/白名单 → 审批(需人工确认时) → schema 校验 → 超时 ctx
   → 执行 → 输出截断/溢写(大结果落盘 + 返回引用) → 审计(结束+耗时) → 写 tool/result 事件
~~~

### 9.3 PTC / run_code（模型写代码调工具）

| 方案 | 隔离性 | 语言 | 建议 |
|---|---|---|---|
| 子进程 worker（Python/Node）+ JSON-RPC 工具桥 | 强（进程级 + OS 沙箱） | 模型最熟的语言 | **推荐**（DSH 也是 worker 隔离） |
| 内嵌 goja（纯 Go JS 引擎） | 中（同进程，需自行限制 API） | 仅 JS | 轻量场景可选 |
| WASM（wazero） | 强（无系统调用，除非显式注入） | Rust/C/Go→wasm | 长期最优，生态弱 |

**必须配套**：CPU/内存/时间限额、网络默认禁、只读文件系统、工具调用需白名单。

### 9.4 工具清单（第一批）

| 类别 | 工具 | 备注 |
|---|---|---|
| 文件 | read / write / edit / glob / grep | 写操作要在 seam 层做沙箱判定 |
| 进程 | bash / exec | 超时 + 输出上限 + 工作目录约束 |
| 网络 | web_search / web_fetch | 返回内容标记为「不可信数据」 |
| 编排 | run_code（PTC）/ subagent / todo_write | subagent 需继承谱系头 |
| 交互 | ask_user_question / approval | 与传输层联动（CLI 提示 / Web 弹窗） |
| 扩展 | MCP 客户端（可选） | 复用生态最快的方式：把 MCP server 当工具源 |

---

## 10. 拆解 L6：上下文装配与压缩

### 10.1 System Prompt 分节（DSH 的 section{name, order, text}）

~~~go
type Section struct{ Name string; Order int; Text func(*Context) string }
type Assembler struct{ sections []Section }   // 按 Order 稳定排序
~~~

要点：**分节可插拔**（插件加一段「技能目录」「项目约定」），order 冲突要能检测；内容函数必须快（实时拼接会拖慢首 token）。

### 10.2 历史选择：视图 + 预算

~~~
View() 事件 → 折叠为消息列表 → 按预算裁剪：
  1) 保留 system + 最近 N 条完整消息
  2) 中间段做「工具结果降级」（大结果换成引用/摘要）
  3) 仍超预算 → 触发 summarize 压缩，写 surfaceOp replace
~~~

### 10.3 压缩策略（三层，可组合）

| 层 | 触发 | 动作 | 风险 |
|---|---|---|---|
| L1 结果剪枝 | 工具输出 > 阈值 | 换成「文件路径 + 前 200 字 + 行数」 | 模型可能失去细节，需保留可回读路径 |
| L2 滑窗 | token 超 80% | 丢弃最早的非 system 消息 | 丢前提，需在 system 里放「关键事实清单」 |
| L3 摘要 | 超 95% 或手动 | 模型总结旧段 → surfaceOp replace | 有损；必须写事件记录「被替换的 seq 区间」 |

---

## 11. 拆解 L7：扩展与配置

### 11.1 配置组合（1:1 复刻 DSH 的形态）

~~~yaml
# profiles/web.yaml —— 组合，不是代码
bundles: [core, tools-local, llm-openai, web]      # 按序加载
patch:
  - id: llm-openai
    config: { baseURL: "https://api.deepseek.com/v1", apiKeyEnv: DEEPSEEK_API_KEY }
  - insert: [{ id: my-tool, name: my-tool }]
~~~

合并顺序：内建默认 → bundle → profile patch → CLI --patch 覆盖；每层记录来源，冲突时报「谁覆盖了谁」。
配置校验用 JSON Schema（Go 侧 struct tag + 校验器），**启动期失败**而不是运行期。

### 11.2 扩展机制三方案（决策矩阵）

| 维度 | A. 链接期注册（init） | B. 进程外插件（gRPC/stdio） | C. WASM（wazero） |
|---|---|---|---|
| 实现难度 | 最低 | 中 | 中高 |
| 语言支持 | 仅 Go | 任意（提供 SDK） | 任意（编译到 wasm） |
| 隔离性 | 无（同进程） | 强（进程边界） | 强（沙箱） |
| 性能 | 最高（直接调用） | 中（IPC 序列化） | 中低（解释/编译开销） |
| 热更新 | 不支持 | 支持（重启子进程） | 支持（换模块） |
| 生态 | Go 包 | 各语言 SDK | 弱 |
| 适用 | 核心能力/自研工具 | 第三方扩展/高风险工具 | 用户上传的不可信扩展 |

**决策**：核心与内置工具用 A；第三方与需要热更/隔离的用 B；C 作为长期路线（尤其是「用户上传技能」）。
三者共用同一个 Registry 抽象，上层不感知来源。

### 11.3 凭据与审批 seam

~~~go
type Credentials interface {
    Resolve(ctx context.Context, ref string) (string, error) // ref 形如 OPENROUTER_API_KEY
    Store(ctx context.Context, ref, value string) error
}
type Approver interface {
    Ask(ctx context.Context, req ApprovalRequest) (Decision, error) // allow-once / allow-always / deny
}
~~~

要求：敏感值**永不进事件日志**（只记 ref 名）；审批请求与结果都写事件（可审计）。

---

## 12. 拆解 L8：传输与接口

| 形态 | 用途 | 技术选型 | 备注 |
|---|---|---|---|
| CLI headless | 单次任务、CI、定时任务 | cobra + 标准输出流式 | 对齐 DSH headless：一次任务、流式输出、结束即退出 |
| HTTP/SSE | Web UI 与第三方集成 | net/http + SSE | 浏览器兼容性最好 |
| WebSocket | 双向交互（工具审批、打断） | nhooyr/websocket | 与 SSE 二选一亦可 |
| Connect-RPC | 前后端类型化调用 | buf + protobuf | 一套 schema 生成 Go/TS 双端，等价 DSH typert |
| ACP/MCP | 与编辑器/其它 agent 互操作 | 各自协议实现 | 复用生态的捷径 |

**鉴权**：默认 loopback 白名单 + token cookie（DSH 就是这个模型：token 换 cookie）；对外暴露必须显式开启。

---

## 13. 拆解 L9：客户端与 UI

- 保持 TypeScript/React（不要为「全 Go」牺牲体验）
- 槽位机制复刻：`window.__ModuleLoader__.load({id, factory})`，宿主在固定 slot（侧边栏、消息卡、设置页）挂载插件组件
- 与后端用 Connect-RPC + 事件流；客户端插件只允许调用白名单 RPC
- 若短期不想做 UI：先只做 CLI + SSE，UI 用现成开源 chat 前端接流

---

## 14. 拆解 L10：可观测性与测试

### 14.1 可观测

| 维度 | 实现 |
|---|---|
| 事件级追踪 | 每个 turn/step 一条 trace；request/header 记 config 快照与 system/tools 哈希 |
| 指标 | 首 token 延迟、总时长、token 用量与费用、工具成功率/耗时分布、压缩触发次数 |
| 日志 | 结构化 slog，字段含 session_id/turn/step/tool；敏感字段脱敏 |
| 回放 | 给定事件日志 + 假适配器 → 确定性重演，用于复现线上问题 |

### 14.2 测试金字塔

| 层 | 内容 |
|---|---|
| 单元 | SSE 解析（半包/CRLF/心跳）、tool-call 增量拼装、schema 生成、surface 视图计算 |
| 契约 | 每个 LLM 适配器对同一组金丝雀用例（文本/推理/工具/图片/错误码）输出一致的结构 |
| 集成 | 真适配器 + 假工具，跑 3-5 个典型 turn，断言事件序列 |
| 回放 | 录制线上事件 → 离线重放 → 断言无回归 |
| 模糊 | 对流解析与 JSON 拼装做 fuzz（这两处最容易崩） |

---

## 15. 决策矩阵：Go 化过程中的关键技术选择

| 决策点 | 选项 | 结论 | 理由 |
|---|---|---|---|
| 依赖注入 | (a) 自研 typed registry (b) dig (c) wire 编译期 | **(a) 自研薄 registry** | DSH 是运行期可插拔，wire 编译期注入做不到热插；dig 反射错误在运行期才暴露 |
| 插件装载 | (a) 链接期 (b) gRPC 子进程 (c) WASM | **(a)+(b)**，C 预留 | 见 11.2 |
| 流式 | (a) channel (b) callback (c) iter.Seq2 | **(c) iter.Seq2 + ctx** | Go 1.23+ 标准；取消/背压天然，无需手动 close |
| 事件存储 | (a) JSONL (b) SQLite (c) Postgres | **(a) JSONL 起步，(b) 中期** | 事件溯源天生 append-only，JSONL 可 grep/可回放；量大后迁 SQLite |
| 并发模型 | (a) 每会话 goroutine (b) actor | **(a) 每会话一 goroutine + 消息队列** | 会话内天然串行，避免锁 |
| Schema | (a) 手写 (b) 反射生成 (c) protobuf | **(b) invopop/jsonschema 反射** | 工具作者只写 Go struct，零维护 |
| 传输 | (a) REST+手写 (b) gRPC (c) Connect | **(c) Connect** | 一套 schema 出 Go/TS 两端，浏览器可用，gRPC 兼容 |
| UI | (a) 全 Go 模板 (b) React | **(b) React/TS 复用 DSH 客户端插件协议** | 不牺牲体验，客户端插件可复用 |

---

## 16. 目录结构

~~~text
go-harness/
  cmd/
    gh/                      # CLI 入口（cobra）
    ghd/                     # 常驻服务（Web/API）
  pkg/
    kernel/                  # L0 服务容器、插件、生命周期、错误模型
    session/                 # L1 事件溯源存储 + 视图折叠
    domain/                  # L2 消息/内容块/结束原因/谱系
    llm/                     # L3 运行时 + 适配器
      adapters/openai/
      adapters/anthropic/
    agent/                   # L4 Turn/Step 循环 + 预算
    tools/                   # L5 注册表 + 中间件 + 内置工具
      ptc/                   # run_code 子进程桥
    context/                 # L6 装配 + 压缩
    config/                  # L7 bundle/patch 合并 + 校验
    runtime/plugin/          # L7 进程外插件宿主（gRPC）
    transport/               # L8 CLI / SSE / Connect / ACP
      httpapi/
    observe/                 # L10 trace/metrics/log/replay
  proto/                     # Connect-RPC schema
  plugins/                   # 内置插件（每个一个子目录，注册到 kernel）
    tools-files/
    tools-bash/
    tools-web/
    tool-subagent/
  web/                       # 可选：React UI + 客户端插件宿主
  testdata/
    events/                  # 录制的事件日志（回放用）
    canary/                  # 适配器金丝雀用例
  docs/
    ARCHITECTURE.md          # 本文
~~~

---

## 17. 里程碑与工时（单人，按 1 人日 = 6 小时有效编码）

| 里程碑 | 交付 | 验收标准（可执行） | 工时 |
|---|---|---|---|
| **M0 骨架** | kernel + 配置 + CLI 骨架 | gh --help 可用；装载 2 个插件；LIFO 关闭顺序有测试 | 3 人日 |
| **M1 会话** | 事件存储 + 视图 + 回放 | 写入 1000 事件后重放视图一致；断电写不坏文件（有 fuzz） | 4 人日 |
| **M2 LLM** | openai 适配器 + 错误归类 + 重试 | 流式文本/推理/工具/图片四类金丝雀全过；半包与 CRLF fuzz 不过夜 | 6 人日 |
| **M3 工具** | 注册表 + 中间件 + read/write/bash | 一个 turn 内 tool-call 往返成功；输出溢写与截断有测试 | 5 人日 |
| **M4 Agent** | Turn/Step 循环 + 预算 + 取消 | 3 个端到端场景（纯对话/单工具/多工具并发）；取消后事件完整 | 6 人日 |
| **M5 可用** | CLI 流式 + 配置 profile + 审计 | 真实任务跑通 20 轮不崩；token/费用统计准确 | 5 人日 |
| 合计 MVP | | | **29 人日 ≈ 6 周** |

后续（不含在 MVP）：

| 阶段 | 内容 | 工时 |
|---|---|---|
| M6 | 上下文压缩三层 + surface 替换 | 6 人日 |
| M7 | subagent + 谱系 + 并发编排 | 6 人日 |
| M8 | PTC（run_code 子进程桥 + 沙箱） | 8 人日 |
| M9 | Web UI（SSE + React 复用） | 12 人日 |
| M10 | 进程外插件 gRPC + SDK | 8 人日 |
| M11 | MCP 客户端 + ACP | 6 人日 |
| M12 | 可观测 + 回放 + 指标面板 | 5 人日 |

**总账**：MVP 6 周；到「能自用替代 DSH 的 CLI」约 2.5 人月；含 UI 与生态适配约 5-6 人月。

---

## 18. 风险登记与缓解

| # | 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|---|
| 1 | 适配器碎片化（每家 API 细节不同） | 高 | 中 | 契约测试先行；能力声明 + 降级路径；只承诺 P0 两家 |
| 2 | 事件存储写坏/并发写冲突 | 中 | 高 | 单写者 + 原子追加 + fsync 策略 + 启动自愈（扫描尾部残行截断） |
| 3 | 上下文超限处理不当导致反复失败 | 高 | 中 | 预算预留 + 溢出即压缩 + 最多 N 次 + 明确失败码 |
| 4 | 工具执行安全（shell 逃逸） | 中 | 高 | 沙箱 seam + 默认拒绝 + 审批 + 网络默认关 |
| 5 | PTC 沙箱被绕过 | 低 | 高 | 进程级隔离 + 无网络 + 只读卷 + 资源 cgroup/rlimit |
| 6 | 范围蔓延（想一次做到 DSH 全等） | 高 | 高 | 严格按里程碑，M5 之前不碰 UI/PTC/插件 |
| 7 | Go 泛型/反射写 schema 踩坑 | 中 | 低 | 只支持一个可控的 struct 子集；不支持的类型启动期报错 |
| 8 | 单人项目动力断层 | 中 | 中 | 每里程碑都有可跑 demo；M3 后即可自用 |

---

## 19. 与 DSH 的差异清单（**必须显式向使用者说明**）

| 能力 | DSH | Go Harness（本方案） | 说明 |
|---|---|---|---|
| 插件热插拔 | cordis 运行期服务注入，插件可动态上下线 | 链接期注册 + 进程外 gRPC（可选热更） | Go 没有运行期类型图；接受这个损失换稳定与部署简单 |
| 内容块/结束原因扩展 | TS 声明合并（merge-extensible map） | interface + registry，插件注册新 kind | 语义等价，扩展点从「类型系统」变「注册表」 |
| 客户端插件 | `__ModuleLoader__` 动态加载 ESM | 同协议保留（若做 UI） | 若不做 UI 则缺失 |
| PTC 运行环境 | worker thread + TS 类型 | 子进程（Python/Node）+ JSON-RPC | 隔离更强，启动稍慢 |
| 同会话多 profile 切换 | 支持（client runner/host runner 分离） | 需自行设计（进程外插件下可支持） | 列入 M10 |
| 包数量 | 222 个包 | 目标 12-18 个模块 | 刻意收敛，避免「包地狱」 |

---

## 20. 附录 A：DSH 222 包 → Go 模块映射

| DSH 包族 | 包数（约） | 归并到 | 备注 |
|---|---|---|---|
| cordis / cordis-host-runner / cordis-client-runner 等运行时内核 | 12 | `pkg/kernel` | 服务容器 + 插件生命周期 |
| session / storage / atomic-write / spill | 14 | `pkg/session` | 事件溯源 + 溢出存储 |
| llm / llm-*（各家适配器） | 18 | `pkg/llm` + `adapters/` | 只保留两家 P0 |
| tools / tool-*（40+ 内置工具） | 46 | `pkg/tools` + `plugins/` | 第一批只做 10 个工具 |
| agent / agent-loop / subagent | 10 | `pkg/agent` | Turn/Step + 谱系 |
| compaction / compaction-basic / tool-result-pruner | 8 | `pkg/context` | 三层压缩 |
| skill / skill-filesystem / mcp / acp | 16 | `plugins/` | 可选，M11 |
| config / profiles / bundles | 10 | `pkg/config` | bundle+patch |
| sandbox / bash-local / bash-sandbox / authorization | 12 | `pkg/tools` 沙箱 seam | 平台相关，先只做 Unix |
| api-gateway / remotes / api-session-controller | 10 | `pkg/transport` | Connect-RPC 替代 typert |
| 客户端 UI（client-ui-* 等） | 60+ | `web/`（可选） | 不复刻，直接写 React |
| 其它（attachment/cmdline/code-runtime/observability …） | 20+ | 分散归并 | 按需 |

---

## 21. 附录 B：第一周起步清单（可直接照做）

1. `go mod init github.com/you/go-harness`；Go 1.23+（要 iter.Seq2）
2. 写 `pkg/kernel`：Service registry + Get[T]/Must[T] + Plugin{Name,Requires,Apply,Close} + LIFO 关闭 + 依赖拓扑排序检测
3. 写 `pkg/domain`：ContentBlock interface + 四个实现 + FinishReason 常量 + TurnEndReason
4. 写 `pkg/session`：Event{Type,Seq,Time,Data} + Append（原子追加 + fsync）+ View()（顺序折叠到消息列表）
5. 写 `pkg/llm`：Adapter 接口 + Request/Chunk 结构 + SSE 解析器（**先写测试再写实现**：半包、CRLF、心跳、注释行）
6. 写 `pkg/llm/adapters/openai`：把 Chunk 流正确映射，含 tool-call 增量拼装（先造 6 个分片用例）
7. 写 `pkg/tools`：Registry + 中间件链 + read/write 两个工具（文件工具最先，因为要用它自查）
8. 写 `pkg/agent`：Turn 循环（先只支持「文本 + 单工具」）
9. 写 `cmd/gh`：`gh run "读一下 README 并总结"`，事件打印到 stderr，最终消息打 stdout
10. 加 `pkg/observe/replay`：把一次运行的事件存 testdata/，重放断言一致性

**第一周的可验证成果**：一条命令让模型真实读写本地文件并流式输出，事件日志可回放。
从这一刻起，你会一直在用一个自己造的 harness —— 这是这个项目唯一能长期活下去的形态。

---

## 22. 结论

- **技术可行性：可行。** DSH 的核心不是 TS 语言特性，而是「分层 + 事件溯源 + 注册表 + 插件 + 流式」这套结构；这些在 Go 里都有等价物，只有「运行期类型图」需要换成注册表并失去热插拔。
- **代价要认**：插件生态、客户端插件协议、PTC 体验这三块会明显弱于 DSH；如果这三块是你的刚需，就只做「Go 内核 + 复用 DSH 前端」的混合体，而不是全重写。
- **建议路径**：按 M0→M5 做 6 周 MVP，M3 后即自用；UI 与插件化放在能自用之后，避免范围蔓延。
- **最低成本的正确选择**：如果你只是想要 Go 的性能/部署便利，**先写「深裁剪的子集」**（会话 + LLM + 工具 + CLI，约 10 人日），而不是追求与 DSH 功能对等。
