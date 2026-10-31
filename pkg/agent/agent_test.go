package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhichengtrang/go-harness/pkg/domain"
	"github.com/zhichengtrang/go-harness/pkg/llm"
	"github.com/zhichengtrang/go-harness/pkg/session"
	"github.com/zhichengtrang/go-harness/pkg/tools"
)

type scripted struct {
	chunks []llm.Chunk
	err    error
}

type fakeAdapter struct {
	mu       sync.Mutex
	script   []scripted
	requests []llm.Request
}

func (f *fakeAdapter) Provider() string     { return "fake" }
func (f *fakeAdapter) Caps(string) llm.Caps { return llm.Caps{Tools: true} }
func (f *fakeAdapter) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.Chunk, error] {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	var s scripted
	if len(f.script) > 0 {
		s = f.script[0]
		f.script = f.script[1:]
	} else {
		s = scripted{chunks: []llm.Chunk{{Kind: llm.ChunkFinish, Finish: domain.FinishStop}}}
	}
	f.mu.Unlock()
	return func(yield func(llm.Chunk, error) bool) {
		if s.err != nil {
			yield(llm.Chunk{}, s.err)
			return
		}
		for _, c := range s.chunks {
			if !yield(c, nil) {
				return
			}
		}
	}
}

func textResponse(s string) scripted {
	return scripted{chunks: []llm.Chunk{
		{Kind: llm.ChunkText, Text: s},
		{Kind: llm.ChunkFinish, Finish: domain.FinishStop},
		{Kind: llm.ChunkUsage, Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}},
	}}
}

func toolResponse(id, name, args string) scripted {
	return scripted{chunks: []llm.Chunk{
		{Kind: llm.ChunkToolCall, ToolCall: &domain.ToolCallBlock{ID: id, Name: name, Args: args}},
		{Kind: llm.ChunkFinish, Finish: domain.FinishToolCalls},
	}}
}

func setup(t *testing.T, script ...scripted) (*Agent, *session.Store, *fakeAdapter, *bytes.Buffer) {
	t.Helper()
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	fa := &fakeAdapter{script: script}
	reg := tools.NewRegistry()
	err = reg.Register(tools.Tool{
		Name: "echo", Description: "echo", ReadOnly: true,
		Execute: func(ctx context.Context, args json.RawMessage) (string, error) {
			return "echoed:" + string(args), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	a := New(fa, store, reg, Config{Provider: "fake", Model: "m", System: "you are a test"}, nil, out)
	return a, store, fa, out
}

func eventsOfType(store *session.Store, typ string) []session.Event {
	var out []session.Event
	for _, ev := range store.Events() {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

func TestRunTextOnly(t *testing.T) {
	a, store, fa, out := setup(t, textResponse("你好，我是测试"))
	oc, err := a.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc.Reason != domain.EndCompleted || oc.Steps != 1 {
		t.Fatalf("outcome = %+v", oc)
	}
	if oc.Text != "你好，我是测试" || !strings.Contains(out.String(), "你好") {
		t.Fatalf("text = %q out = %q", oc.Text, out.String())
	}
	if oc.Usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v", oc.Usage)
	}
	if len(fa.requests) != 1 {
		t.Fatalf("requests = %d", len(fa.requests))
	}
	if got := len(eventsOfType(store, session.EvTurnEnd)); got != 1 {
		t.Fatalf("turn/end count = %d", got)
	}
	msgs, err := session.Fold(store.Events())
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Role != domain.RoleUser || msgs[1].Role != domain.RoleAssistant {
		t.Fatalf("msgs = %+v", msgs)
	}
	// 第二次 Run 的 turn 编号要递增
	if _, err := a.Run(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	ends := eventsOfType(store, session.EvTurnEnd)
	var d session.TurnEndData
	json.Unmarshal(ends[1].Data, &d)
	if d.Turn != 2 {
		t.Fatalf("second turn = %d, want 2", d.Turn)
	}
}

func TestRunWithToolCall(t *testing.T) {
	a, store, fa, _ := setup(t,
		toolResponse("call_1", "echo", `{"x":1}`),
		textResponse("工具已经跑完了"),
	)
	oc, err := a.Run(context.Background(), "请调用 echo")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc.Reason != domain.EndCompleted || oc.Steps != 2 {
		t.Fatalf("outcome = %+v", oc)
	}
	if len(fa.requests) != 2 {
		t.Fatalf("requests = %d", len(fa.requests))
	}
	// 第二次请求必须包含工具结果
	second := fa.requests[1].Messages
	found := false
	for _, m := range second {
		for _, b := range m.Blocks {
			if tr, ok := b.(domain.ToolResultBlock); ok && tr.CallID == "call_1" && strings.Contains(tr.Content, "echoed") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("tool result not fed back to model: %+v", second)
	}
	results := eventsOfType(store, session.EvToolResult)
	if len(results) != 1 {
		t.Fatalf("tool results = %d", len(results))
	}
	var tr session.ToolResultData
	json.Unmarshal(results[0].Data, &tr)
	if tr.CallID != "call_1" || tr.Name != "echo" || tr.IsError {
		t.Fatalf("tool result = %+v", tr)
	}
	msgs, _ := session.Fold(store.Events())
	if len(msgs) != 4 { // user, assistant(tool-call), tool, assistant(text)
		t.Fatalf("msgs = %d (%+v)", len(msgs), msgs)
	}
}

func TestRunBlocksAtMaxSteps(t *testing.T) {
	a, store, _, _ := setup(t,
		toolResponse("c1", "echo", "{}"),
		toolResponse("c2", "echo", "{}"),
		toolResponse("c3", "echo", "{}"),
	)
	a.cfg.MaxSteps = 2
	oc, err := a.Run(context.Background(), "loop forever")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc.Reason != domain.EndBlocked || !strings.Contains(oc.Detail, "最大步数") {
		t.Fatalf("outcome = %+v", oc)
	}
	if got := len(eventsOfType(store, session.EvTurnEnd)); got != 1 {
		t.Fatalf("turn/end = %d, want exactly 1", got)
	}
}

func TestRunRetriesRetryableError(t *testing.T) {
	a, _, fa, _ := setup(t,
		scripted{err: &llm.Error{Code: llm.CodeRateLimit, Message: "slow down"}},
		textResponse("重试后成功"),
	)
	a.cfg.Retry = llm.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
	oc, err := a.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc.Reason != domain.EndCompleted || oc.Text != "重试后成功" {
		t.Fatalf("outcome = %+v", oc)
	}
	if len(fa.requests) != 2 {
		t.Fatalf("requests = %d, want 2 (1 fail + 1 success)", len(fa.requests))
	}
}

func TestRunDoesNotRetryPermanentError(t *testing.T) {
	a, store, fa, _ := setup(t,
		scripted{err: &llm.Error{Code: llm.CodeAuth, Message: "bad key"}},
		textResponse("never reached"),
	)
	a.cfg.Retry = llm.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
	oc, err := a.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run returned transport error: %v", err)
	}
	if oc.Reason != domain.EndError || !strings.Contains(oc.Detail, "AUTH") {
		t.Fatalf("outcome = %+v", oc)
	}
	if len(fa.requests) != 1 {
		t.Fatalf("requests = %d, want 1 (no retry on AUTH)", len(fa.requests))
	}
	if got := len(eventsOfType(store, session.EvTurnEnd)); got != 1 {
		t.Fatalf("turn/end = %d", got)
	}
}

func TestRunCanceledWritesAborted(t *testing.T) {
	a, store, _, _ := setup(t, textResponse("won't get here"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	oc, err := a.Run(ctx, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc.Reason != domain.EndAborted {
		t.Fatalf("outcome = %+v", oc)
	}
	ends := eventsOfType(store, session.EvTurnEnd)
	var d session.TurnEndData
	json.Unmarshal(ends[0].Data, &d)
	if d.Reason != domain.EndAborted {
		t.Fatalf("turn/end = %+v", d)
	}
}

func TestSystemPromptIncludesTools(t *testing.T) {
	a, _, fa, _ := setup(t, textResponse("ok"))
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fa.requests[0].System, "echo") {
		t.Fatalf("system prompt missing tool list: %q", fa.requests[0].System)
	}
	if !strings.Contains(fa.requests[0].System, "you are a test") {
		t.Fatalf("system prompt missing base: %q", fa.requests[0].System)
	}
	if len(fa.requests[0].Tools) != 1 {
		t.Fatalf("tools passed = %d", len(fa.requests[0].Tools))
	}
}
