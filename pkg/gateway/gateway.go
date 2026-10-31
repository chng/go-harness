// Package gateway 是模型网关的最小可用版：预算闸门 + 供应商降级 + 成本台账。
//
// 它实现 llm.Adapter，所以在 agent 看来就是一个普通适配器；
// 但内部做了三件事：调用前查预算、主供应商失败时切备供应商、把用量记账。
package gateway

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"sync"
	"time"

	"github.com/zhichengtrang/go-harness/pkg/llm"
	"github.com/zhichengtrang/go-harness/pkg/quota"
)

// Provider 是一家供应商。
type Provider struct {
	Name    string
	Adapter llm.Adapter
}

// Cost 是一次调用的成本记录。
type Cost struct {
	Tenant           string    `json:"tenant"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	PromptTokens     int       `json:"promptTokens"`
	CompletionTokens int       `json:"completionTokens"`
	TotalTokens      int       `json:"totalTokens"`
	Amount           float64   `json:"amount"`
	At               time.Time `json:"at"`
}

// CostLedger 记录成本（内存实现）。
type CostLedger struct {
	mu   sync.Mutex
	rows []Cost
}

// NewCostLedger 创建台账。
func NewCostLedger() *CostLedger { return &CostLedger{} }

// Record 记录一条。
func (l *CostLedger) Record(c Cost) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c.At.IsZero() {
		c.At = time.Now().UTC()
	}
	l.rows = append(l.rows, c)
}

// Rows 返回副本。
func (l *CostLedger) Rows() []Cost {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Cost, len(l.rows))
	copy(out, l.rows)
	return out
}

// Total 汇总某租户的 tokens 与金额；tenant 为空表示全部。
func (l *CostLedger) Total(tenant string) (tokens int, amount float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.rows {
		if tenant != "" && r.Tenant != tenant {
			continue
		}
		tokens += r.TotalTokens
		amount += r.Amount
	}
	return tokens, amount
}

// Pricing 是每百万 token 的价格（人民币，示例值）。
type Pricing struct {
	InputPerM  float64
	OutputPerM float64
}

var pricingTable = map[string]Pricing{
	"deepseek-chat":     {InputPerM: 0.5, OutputPerM: 2.0},
	"deepseek-reasoner": {InputPerM: 1.0, OutputPerM: 4.0},
	"default":           {InputPerM: 1.0, OutputPerM: 3.0},
}

// Price 查价格。
func Price(model string) Pricing {
	if p, ok := pricingTable[model]; ok {
		return p
	}
	return pricingTable["default"]
}

// Gateway 是网关本身。
type Gateway struct {
	chain  []Provider
	limits *quota.Limiter
	costs  *CostLedger
	tenant string
	log    *slog.Logger
}

// New 创建网关。
func New(chain []Provider, limits *quota.Limiter, costs *CostLedger, log *slog.Logger) *Gateway {
	if log == nil {
		log = slog.Default()
	}
	if costs == nil {
		costs = NewCostLedger()
	}
	return &Gateway{chain: chain, limits: limits, costs: costs, log: log}
}

// WithTenant 返回绑定租户的副本（并发安全：原对象不可变）。
func (g *Gateway) WithTenant(tenant string) *Gateway {
	cp := *g
	cp.tenant = tenant
	return &cp
}

// Provider 返回网关名（便于日志）。
func (g *Gateway) Provider() string {
	name := "gateway"
	for i, p := range g.chain {
		if i == 0 {
			name = "gateway(" + p.Name
			continue
		}
		name += "→" + p.Name
	}
	return name + ")"
}

// Caps 取主供应商能力。
func (g *Gateway) Caps(model string) llm.Caps {
	if len(g.chain) == 0 {
		return llm.Caps{}
	}
	return g.chain[0].Adapter.Caps(model)
}

// Costs 暴露台账（给 API/测试用）。
func (g *Gateway) Costs() *CostLedger { return g.costs }

// estimate 粗略估算一次调用的 token（用于调用前预算判定）。
func estimate(req llm.Request) int {
	n := len(req.System)/3 + 16
	for _, m := range req.Messages {
		n += len(m.Text())/3 + 8
	}
	out := req.MaxTokens
	if out <= 0 {
		out = 512
	}
	return n + out
}

// Stream 实现 llm.Adapter。
func (g *Gateway) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.Chunk, error] {
	return func(yield func(llm.Chunk, error) bool) {
		emit := func(c llm.Chunk) bool { return yield(c, nil) }
		if len(g.chain) == 0 {
			yield(llm.Chunk{}, &llm.Error{Code: llm.CodeUnavailable, Message: "gateway: 没有配置任何供应商"})
			return
		}
		est := estimate(req)
		if g.limits != nil {
			if _, err := g.limits.Reserve(g.tenant, est); err != nil {
				yield(llm.Chunk{}, err)
				return
			}
		}
		var lastErr error
		for i, p := range g.chain {
			lastErr = nil
			emitted := false
			var usage llm.Usage
			aborted := false
			for chunk, err := range p.Adapter.Stream(ctx, req) {
				if err != nil {
					lastErr = err
					break
				}
				switch chunk.Kind {
				case llm.ChunkUsage:
					if chunk.Usage != nil {
						usage = *chunk.Usage
					}
				case llm.ChunkText, llm.ChunkReasoning, llm.ChunkToolCall:
					emitted = true
				}
				if !emit(chunk) {
					aborted = true
					break
				}
			}
			if aborted {
				return
			}
			if lastErr == nil {
				if usage.TotalTokens > 0 && g.limits != nil {
					g.limits.Commit(g.tenant, usage.TotalTokens)
				}
				if usage.TotalTokens > 0 {
					price := Price(req.Model)
					amount := float64(usage.PromptTokens)/1e6*price.InputPerM + float64(usage.CompletionTokens)/1e6*price.OutputPerM
					g.costs.Record(Cost{
						Tenant: g.tenant, Provider: p.Name, Model: req.Model,
						PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
						TotalTokens: usage.TotalTokens, Amount: amount,
					})
				}
				return
			}
			// 已经吐了内容就不能换供应商（否则客户端会看到重复文本）
			if emitted || !llm.IsRetryable(lastErr) || i == len(g.chain)-1 {
				yield(llm.Chunk{}, lastErr)
				return
			}
			g.log.Warn("gateway fallback", "from", p.Name, "to", g.chain[i+1].Name, "code", llm.CodeOf(lastErr))
		}
		if lastErr != nil {
			yield(llm.Chunk{}, lastErr)
		}
	}
}

// Describe 返回一行摘要（给健康检查/审计用）。
func (g *Gateway) Describe() string {
	names := ""
	for i, p := range g.chain {
		if i > 0 {
			names += ","
		}
		names += p.Name
	}
	return fmt.Sprintf("gateway{tenant:%s chain:%s}", g.tenant, names)
}
