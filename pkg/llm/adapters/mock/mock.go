// Package mock 是一个确定性的「假模型」，用于无需 API key 的离线 demo 与测试。
//
// 它不假装聪明：按关键词 + 当前已发生的工具结果，走固定的几条脚本，
// 保证框架链路（策略 → 审批 → 幂等 → 工具 → 事件 → 续跑）可被完整验证。
package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/zhichengtrang/go-harness/pkg/domain"
	"github.com/zhichengtrang/go-harness/pkg/llm"
)

// Adapter 是假模型适配器。
type Adapter struct {
	seq atomic.Int64
}

// New 创建假模型。
func New() *Adapter { return &Adapter{} }

// Provider 返回名字。
func (a *Adapter) Provider() string { return "mock" }

// Caps 声明能力。
func (a *Adapter) Caps(string) llm.Caps { return llm.Caps{Tools: true} }

var orderIDRe = regexp.MustCompile("`[A-Z][0-9]{3,}`")
var skuRe = regexp.MustCompile("`SKU-[0-9]+`")

// plan 依据对话状态决定下一步。
func (a *Adapter) plan(req llm.Request) (string, *domain.ToolCallBlock, domain.FinishReason) {
	user := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == domain.RoleUser {
			user = req.Messages[i].Text()
			break
		}
	}
	called := map[string]bool{}
	lastLookup := ""
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			switch v := b.(type) {
			case domain.ToolCallBlock:
				called[v.Name] = true
			case domain.ToolResultBlock:
				if v.Name == "order.lookup" {
					lastLookup = v.Content
				}
			}
		}
	}
	orderID := "A1001"
	if m := orderIDRe.FindString(user); m != "" {
		orderID = m
	}
	mkCall := func(name, args string) *domain.ToolCallBlock {
		return &domain.ToolCallBlock{ID: fmt.Sprintf("mock-%d", a.seq.Add(1)), Name: name, Args: args}
	}

	switch {
	case strings.Contains(user, "退款") || strings.Contains(user, "退钱"):
		if !called["order.lookup"] {
			return "我先查一下这个订单的详情。", mkCall("order.lookup", fmt.Sprintf("{\"orderId\":%q}", orderID)), domain.FinishToolCalls
		}
		if !called["order.refund"] {
			amount := refundableFrom(lastLookup)
			args := fmt.Sprintf("{\"orderId\":%q,\"amount\":%.2f,\"reason\":\"客户申请退款\"}", orderID, amount)
			return "订单信息已确认，我发起退款（需要人工审批）。", mkCall("order.refund", args), domain.FinishToolCalls
		}
		return "退款已经处理完成，请留意到账通知。如需进一步协助，我可以帮你建一张售后工单。", nil, domain.FinishStop

	case strings.Contains(user, "库存") || strings.Contains(user, "有货") || strings.Contains(user, "什么时候发"):
		if !called["inventory.check"] {
			sku := "SKU-1001"
			if m := skuRe.FindString(user); m != "" {
				sku = m
			}
			return "我查一下库存。", mkCall("inventory.check", fmt.Sprintf("{\"sku\":%q}", sku)), domain.FinishToolCalls
		}
		return "库存已查到：有货可以立刻发，缺货则按预计到货时间安排。", nil, domain.FinishStop

	case strings.Contains(user, "投诉") || strings.Contains(user, "工单") || strings.Contains(user, "人工"):
		if !called["ticket.create"] {
			args := fmt.Sprintf("{\"customerId\":\"C-88\",\"orderId\":%q,\"subject\":\"客户投诉跟进\",\"detail\":%q}", orderID, user)
			return "好的，我给你建一张售后工单转人工跟进。", mkCall("ticket.create", args), domain.FinishToolCalls
		}
		return "工单已创建，客服会在 24 小时内联系你。", nil, domain.FinishStop
	}
	return "我是售后助手，可以帮你查订单、查库存、发起退款或建工单。请告诉我订单号或具体诉求。", nil, domain.FinishStop
}

// refundableFrom 从 order.lookup 的结果里取可退金额。
func refundableFrom(content string) float64 {
	var parsed struct {
		Orders []struct {
			Refundable float64 `json:"refundable"`
		} `json:"orders"`
	}
	if err := json.Unmarshal([]byte(content), &parsed); err == nil && len(parsed.Orders) > 0 {
		if v := parsed.Orders[0].Refundable; v > 0 {
			return v
		}
	}
	return 100
}

// Stream 产出流式片段。
func (a *Adapter) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.Chunk, error] {
	return func(yield func(llm.Chunk, error) bool) {
		text, call, finish := a.plan(req)
		for _, part := range splitText(text) {
			if !yield(llm.Chunk{Kind: llm.ChunkText, Text: part}, nil) {
				return
			}
		}
		if call != nil {
			if !yield(llm.Chunk{Kind: llm.ChunkToolCall, ToolCall: call}, nil) {
				return
			}
		}
		prompt := estimateTokens(req)
		completion := len([]rune(text))
		if !yield(llm.Chunk{Kind: llm.ChunkUsage, Usage: &llm.Usage{
			PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion,
		}}, nil) {
			return
		}
		yield(llm.Chunk{Kind: llm.ChunkFinish, Finish: finish}, nil)
	}
}

func splitText(s string) []string {
	if s == "" {
		return nil
	}
	runes := []rune(s)
	var out []string
	for i := 0; i < len(runes); i += 8 {
		end := i + 8
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

func estimateTokens(req llm.Request) int {
	n := len(req.System) / 3
	for _, m := range req.Messages {
		n += len(m.Text())/3 + 10
	}
	return n
}
