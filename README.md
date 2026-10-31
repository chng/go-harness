# go-harness

参考 DSH（@deepseek-ai/dsh）架构，用 Go 实现的可扩展 agent harness 业务框架。

- 技术方案：[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)（739 行，自顶向下拆到 L0-L10）
- 实现说明：[docs/IMPLEMENTATION.md](docs/IMPLEMENTATION.md)（M0-M4 已跑通，含实测证据）
- **后端业务系统重评估**：[docs/BACKEND.md](docs/BACKEND.md)（存储/执行模型/策略/网关/评测的差距与三档交付）
- **可运行业务 Agent Demo**：[docs/DEMO.md](docs/DEMO.md)（售后场景，3 个业务能力插件，审批/幂等/续跑全链路）
- 结论：技术可行；核心 MVP 约 29 人日；本仓库已落地内核/会话/LLM/工具/Agent 循环/CLI，**零第三方依赖**

要求：Go ≥ 1.23（用到 `iter.Seq2` 与 range-over-func）。本机开发用 1.27.3；仓库无第三方依赖，离线也能编译（`GOFLAGS=-mod=mod GOPROXY=off`）。

## 快速开始

~~~bash
make build                       # → bin/gh（若 go 不在一级 PATH，用 make go=/path/to/go）
./bin/gh tools                   # 列出工具与 JSON Schema
export DEEPSEEK_API_KEY=sk-...   # 或 GH_API_KEY / OPENAI_API_KEY
./bin/gh run "列出当前目录结构并总结" --workdir .
./bin/gh replay ~/.go-harness/sessions/<时间戳>   # 回放事件日志
~~~

## 跑一个业务 Agent Demo（推荐先看这个）

~~~bash
make ghd                                              # → bin/ghd
./bin/ghd --provider mock --addr 127.0.0.1:8088       # 零依赖离线跑（确定性假模型）
# 或接真实模型（key 走环境变量，不落盘）：
export DEEPSEEK_API_KEY=sk-...
./bin/ghd --provider deepseek --addr 127.0.0.1:8089
~~~

浏览器打开 http://127.0.0.1:8088 ：查订单 / 查库存 / 申请退款（涉钱走人工审批）/ 建工单。
三个业务能力都是插件（§plugins/order§、§plugins/inventory§、§plugins/ticket§），
框架负责策略、审批、幂等、预算、事件溯源与暂停续跑。细节与实测证据见 [docs/DEMO.md](docs/DEMO.md)。

## 当前状态

| 层 | 状态 |
|---|---|
| L0 内核（服务注册表 + 插件拓扑加载） | ✅ |
| L1 会话（事件溯源 JSONL + 崩溃自愈 + surfaceOp） | ✅ |
| L2 领域模型（内容块/结束原因） | ✅ |
| L3 LLM（流式 + 错误闭集 + 重试 + SSE + tool-call 装配） | ✅ OpenAI 兼容 |
| L5 工具（注册表 + schema 反射 + read/write/edit/glob/grep/bash） | ✅ |
| L4 Agent 循环（步数预算 / 重试 / 并发只读工具 / 终态） | ✅ |
| L8 CLI（run/replay/tools） | ✅ |
| L6 压缩 / L7 配置 profile / 子代理 / PTC / UI / 进程外插件 | ⏳ 见 IMPLEMENTATION.md `7 |

## 用于后端业务系统时（重要）

当前代码是「编码助手形态」的 MVP：本地 JSONL 存储、阻塞式 Run、工具直接执行。
要作为后端业务系统的框架，必须补：Postgres 事件表 + 写者租约、异步作业与断点续跑、
幂等台账 + 策略/审批、模型网关（配额/路由/降级/成本）、OTel 与评测门禁。
存储层已抽出 `EventStore` 接口（`pkg/session/store.go`），agent 不认识文件实现，
换库不需要改上层——细节与工作量见 [docs/BACKEND.md](docs/BACKEND.md)。

## 取舍（按你的两个决定）

- **不要求运行期任意上下线**：插件在启动期一次性加载，按 `Provides/Requires` 拓扑排序，运行期服务只读。
- **UI 不用 Go**：后端只暴露流式与事件；需要界面时用 React 接 SSE/Connect，不复刻客户端插件运行时。
