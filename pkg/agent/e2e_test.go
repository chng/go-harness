package agent

// 端到端测试：假供应商（SSE）+ 真适配器 + 真工具 + 真事件存储。
// 不需要网络、不需要 API key，但覆盖了整条链路。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zhichengtrang/go-harness/pkg/domain"
	"github.com/zhichengtrang/go-harness/pkg/llm/adapters/openai"
	"github.com/zhichengtrang/go-harness/pkg/session"
	"github.com/zhichengtrang/go-harness/pkg/tools"
	"github.com/zhichengtrang/go-harness/pkg/tools/builtin"
)

func TestEndToEndWriteThenAnswer(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		n := calls.Add(1)
		switch n {
		case 1:
			// 第一轮：要求调用 write
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"write\",\"arguments\":\"\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"path\\\":\\\"out.txt\\\",\\\"content\\\":\\\"来自 e2e\\\"}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		default:
			// 第二轮：确认后给最终回答
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"已写入 out.txt\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":6,\"total_tokens\":26}}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	root := t.TempDir()
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	reg := tools.NewRegistry()
	if err := builtin.Register(reg, builtin.Options{Root: root}); err != nil {
		t.Fatal(err)
	}
	adapter := openai.New(openai.Config{BaseURL: srv.URL, APIKey: "test"})
	out := &bytes.Buffer{}
	ag := New(adapter, store, reg, Config{Provider: "openai", Model: "fake", System: "test"}, nil, out)

	oc, err := ag.Run(context.Background(), "写一个文件")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc.Reason != domain.EndCompleted || oc.Steps != 2 {
		t.Fatalf("outcome = %+v", oc)
	}
	data, err := os.ReadFile(filepath.Join(root, "out.txt"))
	if err != nil {
		t.Fatalf("tool did not write file: %v", err)
	}
	if string(data) != "来自 e2e" {
		t.Fatalf("file = %q", data)
	}
	if !strings.Contains(out.String(), "已写入 out.txt") {
		t.Fatalf("streamed out = %q", out.String())
	}
	if oc.Usage.TotalTokens != 26 {
		t.Fatalf("usage = %+v", oc.Usage)
	}

	// 事件序列必须是：turn start → user → (header → assistant → tool/result)* → turn end
	var types []string
	for _, ev := range store.Events() {
		types = append(types, ev.Type)
	}
	want := []string{
		session.EvTurnStart, session.EvUserMessage,
		session.EvRequestHeader, session.EvAssistantMsg, session.EvToolResult,
		session.EvRequestHeader, session.EvAssistantMsg,
		session.EvTurnEnd,
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence\n got: %v\nwant: %v", types, want)
	}

	// 事件能被折叠回可用的对话
	msgs, err := session.Fold(store.Events())
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("folded msgs = %d", len(msgs))
	}
	if got := msgs[1].ToolCalls(); len(got) != 1 || got[0].Name != "write" {
		t.Fatalf("assistant tool calls = %+v", got)
	}
}

// TestEndToEndToolErrorIsFedBack 验证工具失败不会炸掉 turn，而是作为结果回给模型。
func TestEndToEndToolErrorIsFedBack(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c9\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"missing.txt\\\"}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"文件不存在\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	store, _ := session.Open(t.TempDir())
	defer store.Close()
	reg := tools.NewRegistry()
	if err := builtin.Register(reg, builtin.Options{Root: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	ag := New(openai.New(openai.Config{BaseURL: srv.URL}), store, reg, Config{Provider: "openai", Model: "m"}, nil, &bytes.Buffer{})
	oc, err := ag.Run(context.Background(), "读一个不存在的文件")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if oc.Reason != domain.EndCompleted {
		t.Fatalf("outcome = %+v", oc)
	}
	var tr session.ToolResultData
	for _, ev := range store.Events() {
		if ev.Type == session.EvToolResult {
			json.Unmarshal(ev.Data, &tr)
		}
	}
	if !tr.IsError || !strings.Contains(tr.Content, "no such file") {
		t.Fatalf("tool result = %+v", tr)
	}
}
