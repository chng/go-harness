package gateway

import (
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/zhichengtrang/go-harness/pkg/domain"
	"github.com/zhichengtrang/go-harness/pkg/llm"
	"github.com/zhichengtrang/go-harness/pkg/quota"
)

type fakeAdapter struct {
	name  string
	chunk []llm.Chunk
	err   error
	calls int
	usage *llm.Usage
}

func (f *fakeAdapter) Provider() string     { return f.name }
func (f *fakeAdapter) Caps(string) llm.Caps { return llm.Caps{Tools: true} }
func (f *fakeAdapter) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.Chunk, error] {
	f.calls++
	return func(yield func(llm.Chunk, error) bool) {
		for _, c := range f.chunk {
			if !yield(c, nil) {
				return
			}
		}
		if f.err != nil {
			yield(llm.Chunk{}, f.err)
			return
		}
		if f.usage != nil {
			yield(llm.Chunk{Kind: llm.ChunkUsage, Usage: f.usage}, nil)
		}
		yield(llm.Chunk{Kind: llm.ChunkFinish, Finish: domain.FinishStop}, nil)
	}
}

func drain(t *testing.T, seq iter.Seq2[llm.Chunk, error]) (string, error) {
	t.Helper()
	var sb strings.Builder
	var outErr error
	for c, err := range seq {
		if err != nil {
			outErr = err
			continue
		}
		if c.Kind == llm.ChunkText {
			sb.WriteString(c.Text)
		}
	}
	return sb.String(), outErr
}

func TestFallbackWhenPrimaryFails(t *testing.T) {
	primary := &fakeAdapter{name: "primary", err: &llm.Error{Code: llm.CodeUnavailable, Message: "down"}}
	backup := &fakeAdapter{name: "backup", chunk: []llm.Chunk{{Kind: llm.ChunkText, Text: "来自备用"}},
		usage: &llm.Usage{PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000}}
	costs := NewCostLedger()
	g := New([]Provider{{"primary", primary}, {"backup", backup}}, quota.NewLimiter(0), costs, nil).WithTenant("t1")

	text, err := drain(t, g.Stream(context.Background(), llm.Request{Model: "deepseek-chat", System: "s"}))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if text != "来自备用" {
		t.Fatalf("text = %q", text)
	}
	if primary.calls != 1 || backup.calls != 1 {
		t.Fatalf("calls = %d/%d", primary.calls, backup.calls)
	}
	rows := costs.Rows()
	if len(rows) != 1 || rows[0].Provider != "backup" || rows[0].Tenant != "t1" {
		t.Fatalf("cost rows = %+v", rows)
	}
	if rows[0].Amount <= 0 {
		t.Fatalf("amount not computed: %+v", rows[0])
	}
}

func TestNoFallbackAfterPartialOutput(t *testing.T) {
	primary := &fakeAdapter{name: "primary",
		chunk: []llm.Chunk{{Kind: llm.ChunkText, Text: "半句"}},
		err:   &llm.Error{Code: llm.CodeUnavailable, Message: "down"}}
	backup := &fakeAdapter{name: "backup", chunk: []llm.Chunk{{Kind: llm.ChunkText, Text: "不该出现"}}}
	g := New([]Provider{{"primary", primary}, {"backup", backup}}, nil, nil, nil)

	text, err := drain(t, g.Stream(context.Background(), llm.Request{}))
	if err == nil {
		t.Fatal("want error after partial output")
	}
	if text != "半句" || backup.calls != 0 {
		t.Fatalf("text = %q backup calls = %d", text, backup.calls)
	}
}

func TestQuotaBlocksBeforeCall(t *testing.T) {
	primary := &fakeAdapter{name: "primary", chunk: []llm.Chunk{{Kind: llm.ChunkText, Text: "x"}}}
	limits := quota.NewLimiter(100)
	g := New([]Provider{{"primary", primary}}, limits, nil, nil).WithTenant("t1")
	_, err := drain(t, g.Stream(context.Background(), llm.Request{System: strings.Repeat("a", 3000)}))
	if err == nil || llm.CodeOf(err) != llm.CodeQuota {
		t.Fatalf("err = %v", err)
	}
	if primary.calls != 0 {
		t.Fatal("provider must not be called when over budget")
	}
}

func TestUsageCommittedToQuota(t *testing.T) {
	primary := &fakeAdapter{name: "p", chunk: []llm.Chunk{{Kind: llm.ChunkText, Text: "hi"}},
		usage: &llm.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}}
	limits := quota.NewLimiter(10000)
	g := New([]Provider{{"primary", primary}}, limits, nil, nil).WithTenant("t1")
	if _, err := drain(t, g.Stream(context.Background(), llm.Request{})); err != nil {
		t.Fatal(err)
	}
	used, limit := limits.Usage("t1")
	if used != 120 || limit != 10000 {
		t.Fatalf("usage = %d/%d", used, limit)
	}
}

func TestProviderName(t *testing.T) {
	g := New([]Provider{{"a", &fakeAdapter{name: "a"}}, {"b", &fakeAdapter{name: "b"}}}, nil, nil, nil)
	if got := g.Provider(); got != "gateway(a→b)" {
		t.Fatalf("provider name = %q", got)
	}
}
