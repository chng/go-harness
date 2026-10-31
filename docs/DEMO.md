# 可运行 Demo：售后业务 Agent（3 个业务能力插件）

> 框架：go-harness MVP ｜ 场景：电商售后客服 ｜ 实测：2026-10-30（mock 与真实 DeepSeek 双跑）

## 1. 一句话

一条命令起服务，浏览器打开就能对话：查订单 / 查库存 / 发起退款（涉钱必须人工审批）/ 建工单。
**三个业务能力都是插件**，框架负责策略、审批、幂等、预算、事件与续跑。

~~~bash
export PATH=$HOME/sdk/go/bin:$PATH
cd ~/go-harness && make ghd

# 方式 A：零依赖离线 demo（确定性假模型）
./bin/ghd --provider mock --addr 127.0.0.1:8088

# 方式 B：接真实模型（key 从 DSH 的凭据文件读，不落盘到本项目）
./bin/ghd --provider deepseek --cred-file ~/.dsh/.credentials.yaml --addr 127.0.0.1:8089
~~~

打开 http://127.0.0.1:8088 ，试试右上角的四个快捷按钮。

## 2. 三个业务能力插件

| 插件 | 动作 | 风险等级 | 审批 | 幂等 | 说明 |
|---|---|---|---|---|---|
| `capability-order` | `order.lookup` | read | 否 | — | 查订单状态/金额/可退金额 |
| | `order.refund` | **money** | **需要** | 需要 | 退款；超可退金额在审批前就被预检挡掉 |
| `capability-inventory` | `inventory.check` | read | 否 | — | 查 SKU 在各仓库存与预计发货 |
| `capability-ticket` | `ticket.create` | write-soft | 否 | 需要 | 建售后工单（可逆写） |
| | `ticket.list` | read | 否 | — | 列客户工单 |

插件写法（`plugins/order/order.go`）：

~~~go
func Plugin(svc *Service) kernel.Plugin {
    return kernel.Plugin{Name: "capability-order", Provides: []string{"capability.order"}}
}
func Register(svc *Service, cat *action.Catalog, reg *tools.Registry) error {
    return action.Register(cat, reg, action.Meta{
        Name: "order.refund", Capability: "order", Risk: action.RiskMoney,
        Scopes: []string{"order:refund"}, Idempotent: true,
        Confirm:  func(ctx context.Context, raw json.RawMessage) (string, bool) { /* 审批摘要 */ },
        Validate: func(ctx context.Context, raw json.RawMessage) error { /* 审批前预检 */ },
    }, func(ctx context.Context, in RefundArgs) (RefundResult, error) { return svc.Refund(in) })
}
~~~

业务方只写这些，不写 prompt、不写重试、不写审批、不写审计。

## 3. 框架在这一层做了什么

~~~text
HTTP/SSE (pkg/server)
   ↓ action.WithCaller{tenant, user, scenario, session, scopes}
agent 循环 (pkg/agent)  ←→  事件溯源 (pkg/session, JSONL)
   ↓ 工具调用
中间件链 (pkg/tools.Use)
   ├─ policy   (pkg/policy)    允许哪些动作 / 最高风险 / 权限点     → 拒绝即返回，不进模型下一轮
   ├─ approval (pkg/approval)  涉钱动作建审批单 + 挂起 turn         → turn 停在 waiting-approval
   └─ idem     (pkg/idem)      (租户,会话,callId) 台账             → 重复调用只生效一次
   ↓
业务动作 (plugins/*)   →  真实执行
   ↑
模型网关 (pkg/gateway)  预算闸门 → 供应商降级 → 成本台账
~~~

关键设计（都已在后端重评估文档里论证过，这里是可运行的实现）：

1. **turn 可暂停可续跑**：审批挂起时写 `turn/end reason=waiting-approval`，批准后 `agent.Resume` 从事件里找回缺失的工具调用，补跑后继续同一 turn。挂起的调用**不写 tool/result**（因为没执行），Resume 才知道要补跑。
2. **模型不是事务管理器**：涉钱动作走「预检 → 审批 → 幂等台账 → 执行」，跨系统的重复副作用由框架兜。
3. **预算前置**：网关在调用前 `Reserve`，超限直接以 `QUOTA` 错误码中止，而不是事后统计。
4. **审计即事件**：谁在什么时候批准了哪笔钱、模型看到了哪些工具结果，全在事件日志里，`gh replay`/`/api/sessions/{id}` 可回放。

## 4. 实测证据

### 4.1 离线（mock 模型，确定性）

~~~text
1 turn/start
2 user/message       我的订单 A1001 一直没发货，我要退款
3 request/header
4 assistant/message  step=1 finish=tool-calls calls=[order.lookup]
5 tool/result        order.lookup -> {"refundable":899,...}
6 request/header
7 assistant/message  step=2 finish=tool-calls calls=[order.refund]
8 turn/end           reason=waiting-approval detail=approval:ap-0001

审批队列：ap-0001  order.refund  money  订单 A1001 退款 899.00 元，原因：客户申请退款
→ 人工批准 alice
tool/result  order.refund -> {"refundId":"RF-A1001-001","status":"refunded"}  [审批单 ap-0001 已批准]
assistant    退款已经处理完成，请留意到账通知。
turn/end     reason=completed steps=3
~~~

只读与可逆写的对比（无审批）：

~~~text
SKU-1001 还有货吗？ → inventory.check → turn/end completed steps=2, approvals=0
我要投诉转人工     → ticket.create   → TK-00001 → turn/end completed steps=2, approvals=0
~~~

### 4.2 真实模型（DeepSeek deepseek-chat）

第一轮（模型先查单再征求用户确认，没有直接动钱）：

~~~text
step=1 模型: 我先查一下订单情况。    调用 order.lookup {"orderId":"A1002"}
tool   order.lookup -> {"amount":459,"refundable":459,"status":"paid"}
step=2 模型: 查到了…… 需要我按全额 459 元发起退款吗？退款会进入人工审批
turn/end completed steps=2
~~~

第二轮（用户确认后，模型发起退款 → 挂起 → 人工批准 → 续跑）：

~~~text
step=1 调用 order.refund {"orderId":"A1002","amount":459,"reason":"订单长时间未发货，客户申请全额退款"}
turn/end waiting-approval approval:ap-0001
审批单 ap-0001：订单 A1002 退款 459.00 元
→ 人工批准 alice「同意全额退款」
工具 order.refund -> {"refundId":"RF-A1002-001","amount":459,"status":"refunded"}
模型: 退款已提交，审批单号 ap-0001，退款单号 RF-A1002-001，状态已完成退款……
turn/end completed steps=2
用量: 4379 tokens / 0.002574 CNY
~~~

注意最终回答里的单号是 **RF-A1002-001**（真实工具结果），不是模型编的——这是「金额/单号必须来自工具结果」这条系统提示 + 出参 schema 的联合效果。

## 5. HTTP 接口（后端集成用）

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/sessions` | 建会话 → `{id, tenant, scenario, model}` |
| POST | `/api/sessions/{id}/messages` | 发消息（立即返回，后台执行） |
| GET | `/api/sessions/{id}/stream?after=N` | SSE 事件流，`id:` 字段 = seq，可断线续传 |
| GET | `/api/sessions/{id}` | 事件 + 折叠后的消息 + 待审批 + 是否挂起 |
| GET | `/api/approvals` | 待审批列表 |
| POST | `/api/approvals/{id}` | `{approved, by, reason}`，决策后自动续跑 |
| GET | `/api/actions` | 插件与动作目录（含风险等级） |
| GET | `/api/usage` | token 预算使用与成本台账 |

## 6. 与生产形态的差距（诚实清单）

| 项 | demo 现状 | 生产要补 |
|---|---|---|
| 存储 | 本地 JSONL（`~/.go-harness/app`） | Postgres 事件表 + 写者租约 + 分区 |
| 执行 | 进程内 goroutine | job 表 + `SKIP LOCKED` + 续租 + 优雅停机 |
| 幂等/审批 | 内存台账与内存审批单（重启即丢） | 落库，带 TTL 与审计 |
| 鉴权 | 无（loopback 单租户） | API key/OIDC + 租户隔离 |
| 观测 | 结构化日志 | OTel trace/metrics + 告警 |
| 评测 | 无 | 金标集 + 回放回归 + 发布门禁 |

也就是说：**这一版证明了架构与治理链路能跑通**，把它变成生产系统还需要 [BACKEND.md](BACKEND.md) `13 里列的 W1-W10。

## 7. 开发中踩到并修掉的两个真问题

1. **工具名不能带点**：DeepSeek 要求 `function.name` 匹配 `^[a-zA-Z0-9_-]+$`，我们原本用 `order.refund` 直接 400。
   修法：协议边界做 `wireName` 映射（`order.refund → order_refund`），解析回来再映射成原名，上层与事件日志始终是点号；
   并对 `a.b`/`a_b` 的映射冲突做启动期拒绝。回归测试：`TestToolNameMapping` / `TestToolNameCollisionRejected`。
2. **挂起不能写 tool/result**：第一版挂起时也写了 `tool/result(suspended=true)`，导致 `agent.Resume` 以为调用已完成、提示「没有可继续的 turn」。
   修法：挂起即「未执行」，不写结果；Resume 从事件里找不到结果的调用才是待补跑集合。

这两条都属于「只有真跑才会暴露」的问题，已固化成测试。
