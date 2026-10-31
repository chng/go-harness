package action

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zhichengtrang/go-harness/pkg/llm"
	"github.com/zhichengtrang/go-harness/pkg/tools"
)

type in struct {
	OrderID string  `json:"orderId" jsonschema:"订单号"`
	Amount  float64 `json:"amount"`
}

type out struct {
	OK bool `json:"ok"`
}

func TestRegisterAndExecute(t *testing.T) {
	cat := NewCatalog()
	reg := tools.NewRegistry()
	err := Register(cat, reg, Meta{
		Name: "order.refund", Desc: "退款", Risk: RiskMoney, Scopes: []string{"order:refund"},
	}, func(ctx context.Context, in in) (out, error) {
		return out{OK: in.Amount > 0}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	meta, ok := cat.Get("order.refund")
	if !ok || !meta.Risk.NeedsApproval() || !meta.Risk.NeedsIdempotency() {
		t.Fatalf("meta = %+v", meta)
	}
	spec, found := reg.Lookup("order.refund")
	if !found {
		t.Fatal("tool not registered")
	}
	if spec.ReadOnly || !spec.Serial {
		t.Fatal("money action must be serial and not read-only")
	}
	if !strings.Contains(string(spec.Schema), "orderId") {
		t.Fatalf("schema = %s", spec.Schema)
	}
	res := reg.Execute(context.Background(), llm.ToolCall{Name: "order.refund", Args: `{"orderId":"A1","amount":10}`})
	if res.IsError || !strings.Contains(res.Content, "\"ok\":true") {
		t.Fatalf("res = %+v", res)
	}
	res = reg.Execute(context.Background(), llm.ToolCall{Name: "order.refund", Args: `{"orderId":"A1","amount":0}`})
	if res.IsError || !strings.Contains(res.Content, "\"ok\":false") {
		t.Fatalf("res = %+v", res)
	}
}

func TestRegisterRejectsBadInput(t *testing.T) {
	cat := NewCatalog()
	reg := tools.NewRegistry()
	if err := Register[in, out](cat, reg, Meta{}, func(ctx context.Context, in in) (out, error) { return out{}, nil }); err == nil {
		t.Fatal("want empty-name error")
	}
	Register[in, out](cat, reg, Meta{Name: "a"}, func(ctx context.Context, in in) (out, error) { return out{}, nil })
	res := reg.Execute(context.Background(), llm.ToolCall{Name: "a", Args: `{"orderId":"x","nope":1}`})
	if !res.IsError || !strings.Contains(res.Content, "参数无效") {
		t.Fatalf("unknown field should be rejected: %+v", res)
	}
}

func TestSummarizeUsesConfirm(t *testing.T) {
	cat := NewCatalog()
	reg := tools.NewRegistry()
	Register[in, out](cat, reg, Meta{
		Name: "order.refund", Risk: RiskMoney,
		Confirm: func(ctx context.Context, raw json.RawMessage) (string, bool) {
			var v in
			json.Unmarshal(raw, &v)
			return "将退款 " + v.OrderID, true
		},
	}, func(ctx context.Context, in in) (out, error) { return out{}, nil })
	got := Summarize(context.Background(), cat, llm.ToolCall{Name: "order.refund", Args: `{"orderId":"A9","amount":1}`})
	if got != "将退款 A9" {
		t.Fatalf("summary = %q", got)
	}
}

func TestCallerRoundTrip(t *testing.T) {
	ctx := WithCaller(context.Background(), Caller{Tenant: "t1", Scenario: "aftersale", SessionID: "s1"})
	c := GetCaller(ctx)
	if c.Tenant != "t1" || c.Scenario != "aftersale" || c.SessionID != "s1" {
		t.Fatalf("caller = %+v", c)
	}
}
