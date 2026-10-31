package order

import (
	"context"
	"strings"
	"testing"

	"github.com/zhichengtrang/go-harness/pkg/action"
	"github.com/zhichengtrang/go-harness/pkg/approval"
	"github.com/zhichengtrang/go-harness/pkg/llm"
	"github.com/zhichengtrang/go-harness/pkg/tools"
)

func setup(t *testing.T) (*tools.Registry, *approval.MemoryStore) {
	t.Helper()
	svc := NewService()
	cat := action.NewCatalog()
	reg := tools.NewRegistry()
	if err := Register(svc, cat, reg); err != nil {
		t.Fatal(err)
	}
	store := approval.NewMemoryStore(0)
	reg.Use(store.Middleware(cat))
	return reg, store
}

func ctxFor() context.Context {
	return action.WithCaller(context.Background(), action.Caller{Tenant: "t1", SessionID: "s1", Scenario: "aftersale"})
}

func TestLookup(t *testing.T) {
	reg, _ := setup(t)
	res := reg.Execute(ctxFor(), llm.ToolCall{ID: "c1", Name: "order.lookup", Args: `{"orderId":"A1001"}`})
	if res.IsError || !strings.Contains(res.Content, "A1001") || !strings.Contains(res.Content, "refundable") {
		t.Fatalf("res = %+v", res)
	}
	res = reg.Execute(ctxFor(), llm.ToolCall{ID: "c2", Name: "order.lookup", Args: `{"customerId":"C-88"}`})
	if res.IsError {
		t.Fatalf("customer lookup = %+v", res)
	}
	if got := strings.Count(res.Content, "orderId"); got != 2 {
		t.Fatalf("customer C-88 should have 2 orders, got %d (%s)", got, res.Content)
	}
	res = reg.Execute(ctxFor(), llm.ToolCall{ID: "c3", Name: "order.lookup", Args: `{"orderId":"A9999"}`})
	if !res.IsError || !strings.Contains(res.Content, "不存在") {
		t.Fatalf("missing order = %+v", res)
	}
}

func TestRefundNeedsApprovalThenApplies(t *testing.T) {
	reg, store := setup(t)
	call := llm.ToolCall{ID: "r1", Name: "order.refund", Args: `{"orderId":"A1001","amount":100,"reason":"测试"}`}
	res := reg.Execute(ctxFor(), call)
	if res.Suspend == nil {
		t.Fatalf("refund must require approval: %+v", res)
	}
	if _, err := store.Decide(res.Suspend.ID, true, "alice", "同意"); err != nil {
		t.Fatal(err)
	}
	res = reg.Execute(ctxFor(), call)
	if res.IsError || !strings.Contains(res.Content, "RF-A1001") {
		t.Fatalf("refund after approval = %+v", res)
	}
	// 超额
	res = reg.Execute(ctxFor(), llm.ToolCall{ID: "r2", Name: "order.refund", Args: `{"orderId":"A1001","amount":100000,"reason":"测试"}`})
	if !res.IsError || !strings.Contains(res.Content, "超过可退金额") {
		t.Fatalf("over refund = %+v", res)
	}
	// 已关闭订单
	res = reg.Execute(ctxFor(), llm.ToolCall{ID: "r3", Name: "order.refund", Args: `{"orderId":"A1003","amount":10,"reason":"测试"}`})
	if !res.IsError || !strings.Contains(res.Content, "已关闭") {
		t.Fatalf("closed order = %+v", res)
	}
}

func TestRefundRejectionBlocksExecution(t *testing.T) {
	reg, store := setup(t)
	call := llm.ToolCall{ID: "r9", Name: "order.refund", Args: `{"orderId":"A1002","amount":50,"reason":"测试"}`}
	res := reg.Execute(ctxFor(), call)
	store.Decide(res.Suspend.ID, false, "bob", "金额不对")
	res = reg.Execute(ctxFor(), call)
	if !res.IsError || !strings.Contains(res.Content, "驳回") {
		t.Fatalf("rejected refund = %+v", res)
	}
}
