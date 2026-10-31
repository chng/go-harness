package idem

import (
	"context"
	"strings"
	"testing"

	"github.com/zhichengtrang/go-harness/pkg/action"
	"github.com/zhichengtrang/go-harness/pkg/llm"
	"github.com/zhichengtrang/go-harness/pkg/tools"
)

type args struct {
	Amount int `json:"amount"`
}

func setup(t *testing.T) (*tools.Registry, *int) {
	t.Helper()
	cat := action.NewCatalog()
	reg := tools.NewRegistry()
	count := 0
	for _, name := range []string{"order.refund", "order.lookup"} {
		risk := action.RiskMoney
		if name == "order.lookup" {
			risk = action.RiskRead
		}
		if err := action.Register(cat, reg, action.Meta{Name: name, Risk: risk},
			func(ctx context.Context, in args) (map[string]any, error) {
				count++
				return map[string]any{"n": count}, nil
			}); err != nil {
			t.Fatal(err)
		}
	}
	reg.Use(NewMemoryLedger().Middleware(cat))
	return reg, &count
}

func ctxFor() context.Context {
	return action.WithCaller(context.Background(), action.Caller{Tenant: "t1", SessionID: "s1"})
}

func TestSameCallExecutesOnce(t *testing.T) {
	reg, count := setup(t)
	call := llm.ToolCall{ID: "c1", Name: "order.refund", Args: `{"amount":10}`}
	first := reg.Execute(ctxFor(), call)
	if first.IsError {
		t.Fatalf("first = %+v", first)
	}
	second := reg.Execute(ctxFor(), call)
	if second.IsError {
		t.Fatalf("second should be idempotent hit: %+v", second)
	}
	if *count != 1 {
		t.Fatalf("executed %d times", *count)
	}
	if !strings.Contains(second.Content, "幂等命中") {
		t.Fatalf("second = %q", second.Content)
	}
}

func TestDifferentCallIDsExecuteSeparately(t *testing.T) {
	reg, count := setup(t)
	reg.Execute(ctxFor(), llm.ToolCall{ID: "c1", Name: "order.refund", Args: `{"amount":10}`})
	reg.Execute(ctxFor(), llm.ToolCall{ID: "c2", Name: "order.refund", Args: `{"amount":10}`})
	if *count != 2 {
		t.Fatalf("count = %d", *count)
	}
}

func TestReadActionsNotTracked(t *testing.T) {
	reg, count := setup(t)
	reg.Execute(ctxFor(), llm.ToolCall{ID: "c1", Name: "order.lookup", Args: "{}"})
	reg.Execute(ctxFor(), llm.ToolCall{ID: "c1", Name: "order.lookup", Args: "{}"})
	if *count != 2 {
		t.Fatalf("read action should run both times, count = %d", *count)
	}
}

func TestUnknownStateBlocksRetry(t *testing.T) {
	reg, count := setup(t)
	l := NewMemoryLedger()
	_ = l
	// 手工制造「已领取但无结果」的状态
	cat := action.NewCatalog()
	reg2 := tools.NewRegistry()
	n := 0
	action.Register(cat, reg2, action.Meta{Name: "order.refund", Risk: action.RiskMoney},
		func(ctx context.Context, in args) (map[string]any, error) {
			n++
			return nil, context.DeadlineExceeded
		})
	ledger := NewMemoryLedger()
	reg2.Use(ledger.Middleware(cat))
	call := llm.ToolCall{ID: "cx", Name: "order.refund", Args: `{"amount":1}`}
	res := reg2.Execute(ctxFor(), call)
	if !res.IsError {
		t.Fatalf("first failing call should be error: %+v", res)
	}
	res = reg2.Execute(ctxFor(), call)
	if !res.IsError || !strings.Contains(res.Content, "结果未知") {
		t.Fatalf("retry after unknown must be blocked: %+v", res)
	}
	if n != 1 {
		t.Fatalf("executed %d times, want 1", n)
	}
	_ = reg
	_ = count
}
