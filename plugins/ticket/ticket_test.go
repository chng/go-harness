package ticket

import (
	"context"
	"strings"
	"testing"

	"github.com/zhichengtrang/go-harness/pkg/action"
	"github.com/zhichengtrang/go-harness/pkg/llm"
	"github.com/zhichengtrang/go-harness/pkg/tools"
)

func TestCreateAndList(t *testing.T) {
	cat := action.NewCatalog()
	reg := tools.NewRegistry()
	if err := Register(NewService(), cat, reg); err != nil {
		t.Fatal(err)
	}
	ctx := action.WithCaller(context.Background(), action.Caller{Tenant: "t1"})
	res := reg.Execute(ctx, llm.ToolCall{ID: "c1", Name: "ticket.create", Args: `{"customerId":"C-88","orderId":"A1001","subject":"投诉跟进"}`})
	if res.IsError || !strings.Contains(res.Content, "TK-00001") {
		t.Fatalf("create = %+v", res)
	}
	res = reg.Execute(ctx, llm.ToolCall{ID: "c2", Name: "ticket.list", Args: `{"customerId":"C-88"}`})
	if res.IsError || !strings.Contains(res.Content, "TK-00001") {
		t.Fatalf("list = %+v", res)
	}
	if meta, ok := cat.Get("ticket.create"); !ok || meta.Risk.NeedsApproval() {
		t.Fatalf("ticket.create should be write-soft (no approval), meta = %+v", meta)
	}
}
