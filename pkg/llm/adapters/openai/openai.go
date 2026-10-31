// Package openai 实现 OpenAI Chat Completions 兼容协议的流式适配器。
//
// 覆盖：DeepSeek / OpenRouter / 通义 / Kimi / 自建 vLLM 等。
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"

	"github.com/zhichengtrang/go-harness/pkg/domain"
	"github.com/zhichengtrang/go-harness/pkg/llm"
)

// Config 是适配器配置。
type Config struct {
	ProviderName string // 展示用名称，默认 "openai"
	BaseURL      string // 例：https://api.deepseek.com/v1
	APIKey       string
	Client       *http.Client
	Headers      map[string]string // 额外请求头（OpenRouter 的 HTTP-Referer 等）
}

// Adapter 是 openai 兼容适配器。
type Adapter struct {
	cfg    Config
	client *http.Client
}

// New 创建适配器。
func New(cfg Config) *Adapter {
	if cfg.ProviderName == "" {
		cfg.ProviderName = "openai"
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	c := cfg.Client
	if c == nil {
		c = &http.Client{Timeout: 0} // 流式请求不设整体超时，用 ctx 控制
	}
	return &Adapter{cfg: cfg, client: c}
}

// Provider 返回供应商名。
func (a *Adapter) Provider() string { return a.cfg.ProviderName }

// Caps 声明能力。
func (a *Adapter) Caps(model string) llm.Caps {
	m := strings.ToLower(model)
	images := strings.Contains(m, "vision") || strings.Contains(m, "vl") ||
		strings.Contains(m, "4o") || strings.Contains(m, "gpt-5") || strings.Contains(m, "claude")
	return llm.Caps{Tools: true, Images: images, Reasoning: true}
}

type wireTool struct {
	Type     string          `json:"type"`
	Function wireToolDefFunc `json:"function"`
}

// wireToolDefFunc 是工具定义里的 function（name/description/parameters）。
type wireToolDefFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// wireCallFunc 是 assistant 消息里 tool_calls 的 function：name 与 arguments 都是字符串。
type wireCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function wireCallFunc `json:"function"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatRequest struct {
	Model           string         `json:"model"`
	Messages        []wireMessage  `json:"messages"`
	Tools           []wireTool     `json:"tools,omitempty"`
	Stream          bool           `json:"stream"`
	StreamOptions   *streamOptions `json:"stream_options,omitempty"`
	Temperature     *float64       `json:"temperature,omitempty"`
	MaxTokens       int            `json:"max_tokens,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
}

type streamDelta struct {
	Content          string `json:"content"`
	Reasoning        string `json:"reasoning"`
	ReasoningContent string `json:"reasoning_content"`
	ToolCalls        []struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type streamChunk struct {
	Choices []struct {
		Delta        streamDelta `json:"delta"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// wireName 把工具名映射成 OpenAI/DeepSeek 允许的字符集：^[a-zA-Z0-9_-]+$。
// 我们用点号做命名空间（order.refund），但协议不接受点号，所以在协议边界做映射，
// 解析回来的 tool_call 再映射回原名——上层与事件日志始终看到点号。
var nameSanitizer = strings.NewReplacer(".", "_", ":", "_", "/", "_", " ", "_")

func wireName(name string) string {
	out := nameSanitizer.Replace(name)
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		}
		return '_'
	}, out)
	if safe == "" {
		safe = "tool"
	}
	return safe
}

// nameMap 建立 协议名 -> 原名 的反查表。
func nameMap(tools []llm.ToolSpec) (map[string]string, error) {
	m := make(map[string]string, len(tools))
	used := make(map[string]string, len(tools))
	for _, t := range tools {
		w := wireName(t.Name)
		if prev, ok := used[w]; ok {
			return nil, fmt.Errorf("工具名 %q 与 %q 映射到同一个协议名 %q，请改名", prev, t.Name, w)
		}
		used[w] = t.Name
		m[w] = t.Name
	}
	return m, nil
}

// buildBody 把领域消息翻译成协议消息。
func buildBody(req llm.Request) (chatRequest, error) {
	body := chatRequest{
		Model:           req.Model,
		Stream:          true,
		StreamOptions:   &streamOptions{IncludeUsage: true},
		Temperature:     req.Temperature,
		MaxTokens:       req.MaxTokens,
		ReasoningEffort: req.ReasoningEffort,
	}
	if req.System != "" {
		body.Messages = append(body.Messages, wireMessage{Role: "system", Content: req.System})
	}
	if _, err := nameMap(req.Tools); err != nil {
		return body, err
	}
	for _, tool := range req.Tools {
		var params json.RawMessage = tool.Schema
		if len(params) == 0 {
			params = json.RawMessage("{\"type\":\"object\",\"properties\":{}}")
		}
		body.Tools = append(body.Tools, wireTool{
			Type:     "function",
			Function: wireToolDefFunc{Name: wireName(tool.Name), Description: tool.Description, Parameters: params},
		})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case domain.RoleUser, domain.RoleSystem:
			body.Messages = append(body.Messages, wireMessage{Role: string(m.Role), Content: m.Text()})
		case domain.RoleAssistant:
			wm := wireMessage{Role: "assistant", Content: m.Text()}
			for _, tc := range m.ToolCalls() {
				args := tc.Args
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
					ID:       tc.ID,
					Type:     "function",
					Function: wireCallFunc{Name: tc.Name, Arguments: args},
				})
			}
			body.Messages = append(body.Messages, wm)
		case domain.RoleTool:
			for _, b := range m.Blocks {
				if tr, ok := b.(domain.ToolResultBlock); ok {
					body.Messages = append(body.Messages, wireMessage{
						Role:       "tool",
						ToolCallID: tr.CallID,
						Content:    tr.Content,
					})
				}
			}
		}
	}
	return body, nil
}

var errStop = errors.New("stop iteration")

// Stream 发起流式调用。所有错误都以 (零值, err) 形式从流中产出，便于调用方统一处理。
func (a *Adapter) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.Chunk, error] {
	return func(yield func(llm.Chunk, error) bool) {
		emit := func(c llm.Chunk) bool { return yield(c, nil) }
		body, err := buildBody(req)
		if err != nil {
			yield(llm.Chunk{}, err)
			return
		}
		raw, err := json.Marshal(body)
		if err != nil {
			yield(llm.Chunk{}, &llm.Error{Code: llm.CodeInvalidRequest, Message: err.Error(), Provider: a.Provider()})
			return
		}
		url := a.cfg.BaseURL + "/chat/completions"
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
		if err != nil {
			yield(llm.Chunk{}, &llm.Error{Code: llm.CodeInvalidRequest, Message: err.Error(), Provider: a.Provider()})
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		if a.cfg.APIKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
		}
		for k, v := range a.cfg.Headers {
			httpReq.Header.Set(k, v)
		}

		resp, err := a.client.Do(httpReq)
		if err != nil {
			if ctx.Err() != nil {
				yield(llm.Chunk{}, &llm.Error{Code: llm.CodeCanceled, Message: ctx.Err().Error(), Provider: a.Provider()})
				return
			}
			var ne interface{ Timeout() bool }
			if errors.As(err, &ne) && ne.Timeout() {
				yield(llm.Chunk{}, &llm.Error{Code: llm.CodeTimeout, Message: err.Error(), Provider: a.Provider()})
				return
			}
			yield(llm.Chunk{}, &llm.Error{Code: llm.CodeUnavailable, Message: err.Error(), Provider: a.Provider()})
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			e := llm.ClassifyHTTP(resp.StatusCode, string(snippet), resp.Header)
			e.Provider = a.Provider()
			e.RequestID = resp.Header.Get("X-Request-Id")
			yield(llm.Chunk{}, e)
			return
		}

		reverse, mapErr := nameMap(req.Tools)
		if mapErr != nil {
			yield(llm.Chunk{}, &llm.Error{Code: llm.CodeInvalidRequest, Message: mapErr.Error(), Provider: a.Provider()})
			return
		}
		acc := llm.NewToolCallAccumulator()
		var usage *llm.Usage
		var finish domain.FinishReason
		stopped := false

		scanErr := llm.ScanSSE(resp.Body, func(ev llm.SSEvent) error {
			if ev.Data == "[DONE]" {
				return errStop
			}
			var chunk streamChunk
			if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
				return &llm.Error{Code: llm.CodeUnknown, Message: "bad chunk: " + err.Error(), Provider: a.Provider()}
			}
			if chunk.Error != nil {
				return &llm.Error{Code: classifyMessage(chunk.Error.Message), Message: chunk.Error.Message, Provider: a.Provider()}
			}
			if chunk.Usage != nil {
				usage = &llm.Usage{
					PromptTokens:     chunk.Usage.PromptTokens,
					CompletionTokens: chunk.Usage.CompletionTokens,
					TotalTokens:      chunk.Usage.TotalTokens,
				}
			}
			for _, ch := range chunk.Choices {
				d := ch.Delta
				reasoning := d.ReasoningContent
				if reasoning == "" {
					reasoning = d.Reasoning
				}
				if reasoning != "" {
					if !emit(llm.Chunk{Kind: llm.ChunkReasoning, Text: reasoning}) {
						stopped = true
						return errStop
					}
				}
				if d.Content != "" {
					if !emit(llm.Chunk{Kind: llm.ChunkText, Text: d.Content}) {
						stopped = true
						return errStop
					}
				}
				for _, tc := range d.ToolCalls {
					acc.Add(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
				}
				if ch.FinishReason != "" {
					finish = mapFinish(ch.FinishReason)
				}
			}
			return nil
		})
		if scanErr != nil && !errors.Is(scanErr, errStop) {
			yield(llm.Chunk{}, scanErr)
			return
		}
		if stopped {
			return
		}
		for _, call := range acc.Calls() {
			c := call
			if canonical, ok := reverse[c.Name]; ok {
				c.Name = canonical
			}
			if !emit(llm.Chunk{Kind: llm.ChunkToolCall, ToolCall: &c}) {
				return
			}
		}
		if usage != nil {
			if !emit(llm.Chunk{Kind: llm.ChunkUsage, Usage: usage}) {
				return
			}
		}
		if finish == "" {
			if acc.Len() > 0 {
				finish = domain.FinishToolCalls
			} else {
				finish = domain.FinishStop
			}
		}
		emit(llm.Chunk{Kind: llm.ChunkFinish, Finish: finish})
	}
}

func mapFinish(s string) domain.FinishReason {
	switch s {
	case "stop":
		return domain.FinishStop
	case "tool_calls", "function_call":
		return domain.FinishToolCalls
	case "length":
		return domain.FinishMaxTokens
	}
	return domain.FinishOther
}

func classifyMessage(msg string) llm.Code {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "context") && (strings.Contains(m, "length") || strings.Contains(m, "too long")):
		return llm.CodeContextOverflow
	case strings.Contains(m, "rate limit") || strings.Contains(m, "too many requests"):
		return llm.CodeRateLimit
	case strings.Contains(m, "quota") || strings.Contains(m, "insufficient"):
		return llm.CodeQuota
	}
	return llm.CodeUnknown
}

// TimeoutClient 返回带整体超时的 client，供非流式健康检查使用。
func TimeoutClient(d time.Duration) *http.Client { return &http.Client{Timeout: d} }

// HealthCheck 用一次极小的非流式请求验证凭据与连通性。
func (a *Adapter) HealthCheck(ctx context.Context, model string) error {
	body := map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	}
	for k, v := range a.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("health: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		e := llm.ClassifyHTTP(resp.StatusCode, string(snippet), resp.Header)
		e.Provider = a.Provider()
		return e
	}
	return nil
}
