// Package domain 是 L2 领域模型：内容块、消息、结束原因。
//
// DSH 用 TS 接口合并（ContentBlockMap / FinishReasonMap）让插件扩展新块类型；
// Go 没有这个机制，改用 Block 接口 + 未知块透传，扩展点从「类型系统」变成「实现接口」。
package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// Role 是消息角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Block 是消息中的一个内容块。
type Block interface {
	Kind() string
}

// TextBlock 是纯文本。
type TextBlock struct {
	Text string `json:"text"`
}

// ReasoningBlock 是模型推理内容（部分供应商单独返回）。
type ReasoningBlock struct {
	Text string `json:"text"`
}

// ImageBlock 是图片，二选一：内联数据或 URL。
type ImageBlock struct {
	MIME string `json:"mime"`
	Data []byte `json:"data,omitempty"`
	URL  string `json:"url,omitempty"`
}

// ToolCallBlock 是模型请求调用工具。
type ToolCallBlock struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"` // 原始 JSON 字符串
}

// ToolResultBlock 是工具执行结果。
type ToolResultBlock struct {
	CallID  string `json:"callId"`
	Name    string `json:"name,omitempty"`
	Content string `json:"content"`
	IsError bool   `json:"isError,omitempty"`
}

// UnknownBlock 用于透传本版本不认识的内容块（插件扩展的前向兼容兜底）。
type UnknownBlock struct {
	Type string          `json:"type"`
	Raw  json.RawMessage `json:"raw"`
}

func (TextBlock) Kind() string       { return "text" }
func (ReasoningBlock) Kind() string  { return "reasoning" }
func (ImageBlock) Kind() string      { return "image" }
func (ToolCallBlock) Kind() string   { return "tool-call" }
func (ToolResultBlock) Kind() string { return "tool-result" }
func (b UnknownBlock) Kind() string  { return "unknown:" + b.Type }

func (b UnknownBlock) KindName() string { return b.Type }

// Message 是一条消息。
type Message struct {
	Role   Role
	Blocks []Block
	Time   time.Time
	Turn   int
	Step   int
}

// Text 拼接所有文本块，便于日志与断言。
func (m Message) Text() string {
	out := ""
	for _, b := range m.Blocks {
		if t, ok := b.(TextBlock); ok {
			out += t.Text
		}
	}
	return out
}

// ToolCalls 返回消息中的工具调用。
func (m Message) ToolCalls() []ToolCallBlock {
	var out []ToolCallBlock
	for _, b := range m.Blocks {
		if tc, ok := b.(ToolCallBlock); ok {
			out = append(out, tc)
		}
	}
	return out
}

// FinishReason 是一次模型调用结束的原因。
type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishToolCalls FinishReason = "tool-calls"
	FinishMaxTokens FinishReason = "max-tokens"
	FinishLength    FinishReason = "length"
	FinishOther     FinishReason = "other"
)

// TurnEndReason 是一个 turn 的终态（闭集，对齐 DSH 的 TurnEndReasonMap）。
type TurnEndReason string

const (
	EndCompleted   TurnEndReason = "completed"
	EndAborted     TurnEndReason = "aborted"
	EndBlocked     TurnEndReason = "blocked"
	EndError       TurnEndReason = "error"
	EndMaxTokens   TurnEndReason = "max-tokens"
	EndInterrupted TurnEndReason = "interrupted"
	// EndWaitingApproval 是业务系统扩展的终态：turn 暂停，等人工审批后由 Resume 继续。
	EndWaitingApproval TurnEndReason = "waiting-approval"
)

// CancelCause 是取消来源。
type CancelCause string

const (
	CancelUser   CancelCause = "user"
	CancelParent CancelCause = "parent"
	CancelHook   CancelCause = "hook"
	CancelBudget CancelCause = "budget"
)

// Describe 给事件日志用的一行摘要。
func Describe(blocks []Block) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		switch v := b.(type) {
		case TextBlock:
			n := len([]rune(v.Text))
			if n > 40 {
				n = 40
			}
			parts = append(parts, fmt.Sprintf("text(%d)", n))
		case ToolCallBlock:
			parts = append(parts, "tool-call:"+v.Name)
		case ToolResultBlock:
			parts = append(parts, "tool-result:"+v.CallID)
		default:
			parts = append(parts, b.Kind())
		}
	}
	return fmt.Sprint(parts)
}
