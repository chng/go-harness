package openai

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zhichengtrang/go-harness/pkg/domain"
	"github.com/zhichengtrang/go-harness/pkg/llm"
)

// sseServer 起一个假供应商，按给定 SSE 文本回放。
func sseServer(t *testing.T, payload string, status int, header map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		// 分两次写，制造分包边界
		half := len(payload) / 2
		w.Write([]byte(payload[:half]))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		w.Write([]byte(payload[half:]))
	}))
}

func drain(t *testing.T, seq iter.Seq2[llm.Chunk, error]) ([]llm.Chunk, error) {
	t.Helper()
	var out []llm.Chunk
	for c, err := range seq {
		if err != nil {
			return out, err
		}
		out = append(out, c)
	}
	return out, nil
}

func TestStreamTextReasoningUsage(t *testing.T) {
	payload := strings.Join([]string{
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"思考中\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"，世界\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n",
		"data: [DONE]\n\n",
	}, "")
	srv := sseServer(t, payload, 200, nil)
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL, APIKey: "k"})
	chunks, err := drain(t, a.Stream(context.Background(), llm.Request{Model: "m"}))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var text, reasoning string
	var usage *llm.Usage
	var finish domain.FinishReason
	for _, c := range chunks {
		switch c.Kind {
		case llm.ChunkText:
			text += c.Text
		case llm.ChunkReasoning:
			reasoning += c.Text
		case llm.ChunkUsage:
			usage = c.Usage
		case llm.ChunkFinish:
			finish = c.Finish
		}
	}
	if text != "你好，世界" {
		t.Fatalf("text = %q", text)
	}
	if reasoning != "思考中" {
		t.Fatalf("reasoning = %q", reasoning)
	}
	if usage == nil || usage.TotalTokens != 18 {
		t.Fatalf("usage = %+v", usage)
	}
	if finish != domain.FinishStop {
		t.Fatalf("finish = %q", finish)
	}
}

func TestStreamToolCallFragments(t *testing.T) {
	payload := strings.Join([]string{
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"\"}}]}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\" \\\"main.go\\\"}\"}}]}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
		"data: [DONE]\n\n",
	}, "")
	srv := sseServer(t, payload, 200, nil)
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL})
	chunks, err := drain(t, a.Stream(context.Background(), llm.Request{Model: "m"}))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var calls []domain.ToolCallBlock
	var finish domain.FinishReason
	for _, c := range chunks {
		if c.Kind == llm.ChunkToolCall {
			calls = append(calls, *c.ToolCall)
		}
		if c.Kind == llm.ChunkFinish {
			finish = c.Finish
		}
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Name != "read" {
		t.Fatalf("call = %+v", calls[0])
	}
	if calls[0].Args != "{\"path\": \"main.go\"}" {
		t.Fatalf("args = %q", calls[0].Args)
	}
	if finish != domain.FinishToolCalls {
		t.Fatalf("finish = %q", finish)
	}
}

func TestStreamRateLimitError(t *testing.T) {
	srv := sseServer(t, "{\"error\":{\"message\":\"Rate limit reached\"}}", 429, map[string]string{"Retry-After": "2"})
	defer srv.Close()
	a := New(Config{BaseURL: srv.URL})
	_, err := drain(t, a.Stream(context.Background(), llm.Request{Model: "m"}))
	if err == nil {
		t.Fatal("want error")
	}
	if got := llm.CodeOf(err); got != llm.CodeRateLimit {
		t.Fatalf("code = %s (%v)", got, err)
	}
	var le *llm.Error
	if e, ok := err.(*llm.Error); ok {
		le = e
	}
	if le == nil || le.RetryAfter.Seconds() != 2 {
		t.Fatalf("retryAfter = %+v", err)
	}
	if !llm.IsRetryable(err) {
		t.Fatal("rate limit should be retryable")
	}
}

func TestStreamContextOverflowError(t *testing.T) {
	srv := sseServer(t, "{\"error\":{\"message\":\"maximum context length is 65536 tokens\"}}", 400, nil)
	defer srv.Close()
	a := New(Config{BaseURL: srv.URL})
	_, err := drain(t, a.Stream(context.Background(), llm.Request{Model: "m"}))
	if llm.CodeOf(err) != llm.CodeContextOverflow {
		t.Fatalf("code = %s (%v)", llm.CodeOf(err), err)
	}
}

func TestBuildBodyTranslatesHistory(t *testing.T) {
	temp := 0.2
	req := llm.Request{
		Model:       "m",
		System:      "sys",
		Temperature: &temp,
		Tools:       []llm.ToolSpec{{Name: "read", Description: "read file"}},
		Messages: []domain.Message{
			{Role: domain.RoleUser, Blocks: []domain.Block{domain.TextBlock{Text: "hi"}}},
			{Role: domain.RoleAssistant, Blocks: []domain.Block{
				domain.TextBlock{Text: "let me look"},
				domain.ToolCallBlock{ID: "c1", Name: "read", Args: "{\"path\":\"a\"}"},
			}},
			{Role: domain.RoleTool, Blocks: []domain.Block{
				domain.ToolResultBlock{CallID: "c1", Name: "read", Content: "file body"},
			}},
		},
	}
	body, err := buildBody(req)
	if err != nil {
		t.Fatal(err)
	}
	if !body.Stream || body.StreamOptions == nil || !body.StreamOptions.IncludeUsage {
		t.Fatal("stream options wrong")
	}
	if len(body.Messages) != 4 {
		t.Fatalf("messages = %d, want 4 (system+3)", len(body.Messages))
	}
	if body.Messages[0].Role != "system" || body.Messages[0].Content != "sys" {
		t.Fatalf("msg0 = %+v", body.Messages[0])
	}
	if len(body.Messages[2].ToolCalls) != 1 || body.Messages[2].ToolCalls[0].ID != "c1" {
		t.Fatalf("assistant tool_calls = %+v", body.Messages[2])
	}
	if body.Messages[3].Role != "tool" || body.Messages[3].ToolCallID != "c1" {
		t.Fatalf("tool msg = %+v", body.Messages[3])
	}
	if len(body.Tools) != 1 || body.Tools[0].Function.Name != "read" {
		t.Fatalf("tools = %+v", body.Tools)
	}
	if body.ReasoningEffort != "" {
		t.Fatalf("reasoning effort should be empty, got %q", body.ReasoningEffort)
	}
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), "\"parameters\"") {
		t.Fatalf("default parameters missing: %s", raw)
	}
}

// TestAssistantToolCallsSerializeArgumentsAsString 回归测试：
// 曾经把 tool_calls 的 function 与工具定义的 function 用同一个结构体，
// 结果发出去的是 "parameters" 而不是 "arguments"，DeepSeek 直接 422。
func TestAssistantToolCallsSerializeArgumentsAsString(t *testing.T) {
	body, err := buildBody(llm.Request{
		Model: "m",
		Messages: []domain.Message{{
			Role: domain.RoleAssistant,
			Blocks: []domain.Block{
				domain.ToolCallBlock{ID: "c1", Name: "read", Args: `{"path":"a"}`},
				domain.ToolCallBlock{ID: "c2", Name: "list", Args: ""},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, `"arguments":"{\"path\":\"a\"}"`) {
		t.Fatalf("arguments missing/misencoded: %s", s)
	}
	if strings.Contains(s, `"parameters":{"path"`) {
		t.Fatalf("tool_call leaked parameters field: %s", s)
	}
	// 空参数要归一化成 {}，不能是空字符串
	if !strings.Contains(s, `"arguments":"{}"`) {
		t.Fatalf("empty args not normalized: %s", s)
	}
}

// TestToolNameMapping 回归测试：DeepSeek 要求 function.name 匹配 ^[a-zA-Z0-9_-]+$，
// 我们用点号命名空间（order.refund），所以必须在协议边界做映射，并在解析时映射回来。
func TestToolNameMapping(t *testing.T) {
	specs := []llm.ToolSpec{
		{Name: "order.lookup", Description: "lookup"},
		{Name: "order.refund", Description: "refund"},
	}
	body, err := buildBody(llm.Request{Model: "m", Tools: specs})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "order.lookup") || strings.Contains(string(raw), "order.refund") {
		t.Fatalf("dotted names leaked into wire body: %s", raw)
	}
	if !strings.Contains(string(raw), `"name":"order_lookup"`) {
		t.Fatalf("sanitized name missing: %s", raw)
	}

	payload := strings.Join([]string{
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"order_refund\",\"arguments\":\"{}\"}}]}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
		"data: [DONE]\n\n",
	}, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(payload))
	}))
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL})
	chunks, err := drain(t, a.Stream(context.Background(), llm.Request{Model: "m", Tools: specs}))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	found := false
	for _, c := range chunks {
		if c.Kind == llm.ChunkToolCall && c.ToolCall != nil {
			found = true
			if c.ToolCall.Name != "order.refund" {
				t.Fatalf("name not mapped back: %q", c.ToolCall.Name)
			}
		}
	}
	if !found {
		t.Fatal("no tool call emitted")
	}
}

func TestToolNameCollisionRejected(t *testing.T) {
	_, err := nameMap([]llm.ToolSpec{{Name: "a.b"}, {Name: "a_b"}})
	if err == nil {
		t.Fatal("collision should be rejected")
	}
}
