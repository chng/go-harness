// Package llm 是 L3 模型适配层：类型、错误闭集、重试策略、流式装配。
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"strings"
	"time"

	"github.com/zhichengtrang/go-harness/pkg/domain"
)

// ToolCall 是模型请求的一次工具调用（别名，避免上层重复 import domain）。
type ToolCall = domain.ToolCallBlock

// ToolSpec 是给模型看的工具声明。
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// Request 是一次模型调用的输入。
type Request struct {
	Model           string
	System          string
	Messages        []domain.Message
	Tools           []ToolSpec
	Temperature     *float64
	MaxTokens       int
	ReasoningEffort string
}

// Usage 是 token 用量。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// ChunkKind 是流片段类型。
type ChunkKind int

const (
	ChunkText      ChunkKind = iota // 正文增量
	ChunkReasoning                  // 推理增量
	ChunkToolCall                   // 一个拼装完成的工具调用
	ChunkUsage                      // 用量
	ChunkFinish                     // 结束原因
)

// Chunk 是一个流片段。
type Chunk struct {
	Kind     ChunkKind
	Text     string
	ToolCall *domain.ToolCallBlock
	Usage    *Usage
	Finish   domain.FinishReason
}

// Caps 声明一个模型支持的能力，上层据此决定是否传图片/推理强度/工具。
type Caps struct {
	Tools     bool
	Images    bool
	Reasoning bool
}

// Adapter 是一个模型供应商适配器。
type Adapter interface {
	Provider() string
	Caps(model string) Caps
	// Stream 返回流式片段。调用方必须消费完或提前 return(yield 返回 false)。
	Stream(ctx context.Context, req Request) iter.Seq2[Chunk, error]
}

// Code 是错误闭集：上层据此决定重试/降级/压缩/失败。
type Code string

const (
	CodeAuth            Code = "AUTH"
	CodeQuota           Code = "QUOTA"
	CodeRateLimit       Code = "RATE_LIMIT"
	CodeInvalidRequest  Code = "INVALID_REQUEST"
	CodeContextOverflow Code = "CONTEXT_OVERFLOW"
	CodeTimeout         Code = "TIMEOUT"
	CodeUnavailable     Code = "UNAVAILABLE"
	CodeCanceled        Code = "CANCELED"
	CodeUnknown         Code = "UNKNOWN"
)

// Error 是适配层的统一错误。
type Error struct {
	Code       Code
	Message    string
	Status     int
	RetryAfter time.Duration
	RequestID  string
	Provider   string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("llm %s: %s", e.Code, e.Message)
	if e.Status != 0 {
		msg += fmt.Sprintf(" (status %d)", e.Status)
	}
	return msg
}

// CodeOf 提取错误码；非 Error 时按 ctx 取消/超时/未知归类。
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var le *Error
	if errors.As(err, &le) {
		return le.Code
	}
	if errors.Is(err, context.Canceled) {
		return CodeCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return CodeTimeout
	}
	return CodeUnknown
}

// IsRetryable 报告错误是否值得重试（白名单，避免把 400 也重试 5 次）。
func IsRetryable(err error) bool {
	switch CodeOf(err) {
	case CodeRateLimit, CodeTimeout, CodeUnavailable:
		return true
	}
	return false
}

// RetryPolicy 控制重试。
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// DefaultRetryPolicy 是默认策略：4 次、0.5s 起、上限 8s。
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 4, BaseDelay: 500 * time.Millisecond, MaxDelay: 8 * time.Second}
}

// Delay 计算第 attempt 次（从 1 开始）重试前的等待，尊重 RetryAfter。
func (p RetryPolicy) Delay(attempt int, err error) time.Duration {
	var le *Error
	if errors.As(err, &le) && le.RetryAfter > 0 {
		return min(le.RetryAfter, p.MaxDelay)
	}
	d := p.BaseDelay
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= p.MaxDelay {
			return p.MaxDelay
		}
	}
	return d
}

// ToolCallAccumulator 负责把分片到达的 tool call 拼回完整调用。
//
// 流式协议里参数是 JSON 字符串分片，可能在任何字节边界切断；
// 这里只做字符串拼接与按 index 归一化，最终由上层做 JSON 校验。
type ToolCallAccumulator struct {
	order []int
	byIdx map[int]*partialCall
}

type partialCall struct {
	id   strings.Builder
	name strings.Builder
	args strings.Builder
}

// NewToolCallAccumulator 创建一个空装配器。
func NewToolCallAccumulator() *ToolCallAccumulator {
	return &ToolCallAccumulator{byIdx: make(map[int]*partialCall)}
}

// Add 追加一个分片。
func (a *ToolCallAccumulator) Add(index int, id, name, argsDelta string) {
	p, ok := a.byIdx[index]
	if !ok {
		p = &partialCall{}
		a.byIdx[index] = p
		a.order = append(a.order, index)
	}
	if id != "" {
		p.id.Reset()
		p.id.WriteString(id)
	}
	if name != "" {
		p.name.Reset()
		p.name.WriteString(name)
	}
	if argsDelta != "" {
		p.args.WriteString(argsDelta)
	}
}

// Len 返回已见到的 tool call 数量。
func (a *ToolCallAccumulator) Len() int { return len(a.order) }

// Calls 按 index 顺序返回拼装结果。
func (a *ToolCallAccumulator) Calls() []domain.ToolCallBlock {
	out := make([]domain.ToolCallBlock, 0, len(a.order))
	for _, idx := range a.order {
		p := a.byIdx[idx]
		args := p.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out = append(out, domain.ToolCallBlock{
			ID:   p.id.String(),
			Name: p.name.String(),
			Args: args,
		})
	}
	return out
}

// ClassifyHTTP 把 HTTP 状态与响应体映射成错误码。
func ClassifyHTTP(status int, body string, header http.Header) *Error {
	e := &Error{Status: status, Message: strings.TrimSpace(body)}
	if e.Message == "" {
		e.Message = http.StatusText(status)
	}
	if ra := header.Get("Retry-After"); ra != "" {
		if d, err := time.ParseDuration(ra + "s"); err == nil {
			e.RetryAfter = d
		}
	}
	lower := strings.ToLower(body)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.Code = CodeAuth
	case status == http.StatusPaymentRequired || strings.Contains(lower, "insufficient") || strings.Contains(lower, "quota"):
		e.Code = CodeQuota
	case status == http.StatusTooManyRequests:
		e.Code = CodeRateLimit
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		e.Code = CodeTimeout
	case strings.Contains(lower, "context length") || strings.Contains(lower, "too long") ||
		strings.Contains(lower, "maximum context") || strings.Contains(lower, "context_length_exceeded"):
		e.Code = CodeContextOverflow
	case status >= 400 && status < 500:
		e.Code = CodeInvalidRequest
	case status >= 500:
		e.Code = CodeUnavailable
	default:
		e.Code = CodeUnknown
	}
	return e
}
