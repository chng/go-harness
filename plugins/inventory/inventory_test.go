package inventory

import (
	"context"
	"strings"
	"testing"

	"github.com/zhichengtrang/go-harness/pkg/action"
	"github.com/zhichengtrang/go-harness/pkg/llm"
	"github.com/zhichengtrang/go-harness/pkg/tools"
)

func TestCheck(t *testing.T) {
	cat := action.NewCatalog()
	reg := tools.NewRegistry()
	if err := Register(NewService(), cat, reg); err != nil {
		t.Fatal(err)
	}
	ctx := action.WithCaller(context.Background(), action.Caller{Tenant: "t1"})
	res := reg.Execute(ctx, llm.ToolCall{ID: "c1", Name: "inventory.check", Args: `{"sku":"SKU-1001"}`})
	if res.IsError || !strings.Contains(res.Content, "杭州") {
		t.Fatalf("res = %+v", res)
	}
	res = reg.Execute(ctx, llm.ToolCall{ID: "c2", Name: "inventory.check", Args: `{"sku":"SKU-2002","warehouse":"杭州"}`})
	if res.IsError || !strings.Contains(res.Content, "3") {
		t.Fatalf("res = %+v", res)
	}
	res = reg.Execute(ctx, llm.ToolCall{ID: "c3", Name: "inventory.check", Args: `{"sku":"SKU-9999"}`})
	if !res.IsError || !strings.Contains(res.Content, "没有库存记录") {
		t.Fatalf("res = %+v", res)
	}
}
