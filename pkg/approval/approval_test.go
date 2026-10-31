package approval

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zhichengtrang/go-harness/pkg/action"
	"github.com/zhichengtrang/go-harness/pkg/llm"
	"github.com/zhichengtrang/go-harness/pkg/tools"
)

type refundIn struct {
	OrderID string `json:"orderId"`
	Amount  int    `json:"amount"`
}

func setup(t *testing.T) (*tools.Registry, *MemoryStore, *int) {
	t.Helper()
	cat := action.NewCatalog()
	reg := tools.NewRegistry()
	executed := 0
	if err := action.Register(cat, reg, action.Meta{
		Name: "order.refund", Risk: action.RiskMoney,
		Confirm: func(ctx context.Context, raw json.RawMessage) (string, bool) { return "退款确认", true },
	}, func(ctx context.Context, in refundIn) (map[string]any, error) {
		executed++
		return map[string]any{"refunded": in.Amount, "order": in.OrderID}, nil
	}); err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(0)
	reg.Use(store.Middleware(cat))
	return reg, store, &executed
}

func ctxFor(callID string) context.Context {
	return action.WithCaller(context.Background(), action.Caller{Tenant: "t1", SessionID: "s1", Scenario: "aftersale"})
}

func TestHighRiskCallSuspends(t *testing.T) {
	reg, store, executed := setup(t)
	res := reg.Execute(ctxFor("c1"), llm.ToolCall{ID: "c1", Name: "order.refund", Args: `{"orderId":"A1","amount":100}`})
	if res.Suspend == nil || res.Suspend.Kind != "approval" {
		t.Fatalf("want suspend, got %+v", res)
	}
	if *executed != 0 {
		t.Fatal("must not execute before approval")
	}
	if !strings.Contains(res.Content, res.Suspend.ID) {
		t.Fatalf("content should mention approval id: %q", res.Content)
	}
	pending := store.ListPending("t1")
	if len(pending) != 1 || pending[0].Risk != "money" {
		t.Fatalf("pending = %+v", pending)
	}
	// 同一个 callId 再次调用不应重复建单
	res2 := reg.Execute(ctxFor("c1"), llm.ToolCall{ID: "c1", Name: "order.refund", Args: `{"orderId":"A1","amount":100}`})
	if res2.Suspend == nil || res2.Suspend.ID != res.Suspend.ID {
		t.Fatalf("approval id changed: %+v vs %+v", res2.Suspend, res.Suspend)
	}
	if len(store.ListPending("t1")) != 1 {
		t.Fatal("duplicate approval created")
	}
}

func TestApprovedThenExecutes(t *testing.T) {
	reg, store, executed := setup(t)
	res := reg.Execute(ctxFor("c1"), llm.ToolCall{ID: "c1", Name: "order.refund", Args: `{"orderId":"A1","amount":100}`})
	id := res.Suspend.ID
	if _, err := store.Decide(id, true, "alice", "客户投诉属实"); err != nil {
		t.Fatal(err)
	}
	res = reg.Execute(ctxFor("c1"), llm.ToolCall{ID: "c1", Name: "order.refund", Args: `{"orderId":"A1","amount":100}`})
	if res.IsError || res.Suspend != nil {
		t.Fatalf("approved call should execute: %+v", res)
	}
	if *executed != 1 {
		t.Fatalf("executed = %d", *executed)
	}
	if !strings.Contains(res.Content, "已批准") {
		t.Fatalf("missing audit note: %q", res.Content)
	}
	if len(store.ListPending("t1")) != 0 {
		t.Fatal("approved request should not stay pending")
	}
}

func TestRejectedDoesNotExecute(t *testing.T) {
	reg, store, executed := setup(t)
	res := reg.Execute(ctxFor("c1"), llm.ToolCall{ID: "c1", Name: "order.refund", Args: `{"orderId":"A1","amount":100}`})
	store.Decide(res.Suspend.ID, false, "bob", "金额过高")
	res = reg.Execute(ctxFor("c1"), llm.ToolCall{ID: "c1", Name: "order.refund", Args: `{"orderId":"A1","amount":100}`})
	if !res.IsError || *executed != 0 {
		t.Fatalf("rejected call executed: %+v executed=%d", res, *executed)
	}
	if !strings.Contains(res.Content, "驳回") {
		t.Fatalf("content = %q", res.Content)
	}
}

func TestReadActionsBypassApproval(t *testing.T) {
	cat := action.NewCatalog()
	reg := tools.NewRegistry()
	action.Register(cat, reg, action.Meta{Name: "order.lookup", Risk: action.RiskRead},
		func(ctx context.Context, in refundIn) (map[string]any, error) { return map[string]any{"ok": true}, nil })
	store := NewMemoryStore(0)
	reg.Use(store.Middleware(cat))
	res := reg.Execute(ctxFor("c1"), llm.ToolCall{ID: "c1", Name: "order.lookup", Args: "{}"})
	if res.Suspend != nil || res.IsError {
		t.Fatalf("read action should pass: %+v", res)
	}
}
