package policy

import (
	"context"
	"strings"
	"testing"

	"github.com/zhichengtrang/go-harness/pkg/action"
	"github.com/zhichengtrang/go-harness/pkg/llm"
	"github.com/zhichengtrang/go-harness/pkg/tools"
)

type noArgs struct{}

func newEngine(t *testing.T) (*Engine, *tools.Registry, *action.Catalog) {
	t.Helper()
	cat := action.NewCatalog()
	reg := tools.NewRegistry()
	mk := func(name string, risk action.Risk, scopes ...string) {
		if err := action.Register(cat, reg, action.Meta{Name: name, Risk: risk, Scopes: scopes},
			func(ctx context.Context, in noArgs) (map[string]any, error) {
				return map[string]any{"name": name}, nil
			}); err != nil {
			t.Fatal(err)
		}
	}
	mk("order.lookup", action.RiskRead)
	mk("order.refund", action.RiskMoney, "order:refund")
	mk("ticket.create", action.RiskWriteHard)
	return New(cat), reg, cat
}

func ctxFor(tenant, scenario string, scopes ...string) context.Context {
	return action.WithCaller(context.Background(), action.Caller{Tenant: tenant, Scenario: scenario, Scopes: scopes, SessionID: "s1"})
}

func TestDefaultRuleIsReadOnly(t *testing.T) {
	e, reg, _ := newEngine(t)
	reg.Use(e.Middleware())
	ctx := ctxFor("t1", "unknown")

	res := reg.Execute(ctx, llm.ToolCall{Name: "order.lookup", Args: "{}"})
	if res.IsError {
		t.Fatalf("read should pass by default: %+v", res)
	}
	res = reg.Execute(ctx, llm.ToolCall{Name: "order.refund", Args: "{}"})
	if !res.IsError || !strings.Contains(res.Content, "风险等级") {
		t.Fatalf("money action must be blocked by default: %+v", res)
	}
}

func TestAllowListAndRiskCeiling(t *testing.T) {
	e, reg, _ := newEngine(t)
	e.SetRule(Rule{Tenant: "t1", Scenario: "aftersale", Allow: []string{"order.*"}, MaxRisk: action.RiskMoney})
	reg.Use(e.Middleware())
	ctx := ctxFor("t1", "aftersale")

	if res := reg.Execute(ctx, llm.ToolCall{Name: "order.refund", Args: "{}"}); res.IsError {
		t.Fatalf("allowed action blocked: %+v", res)
	}
	if res := reg.Execute(ctx, llm.ToolCall{Name: "ticket.create", Args: "{}"}); !res.IsError {
		t.Fatalf("non-allowlisted action leaked: %+v", res)
	}
}

func TestDenyWinsOverAllow(t *testing.T) {
	e, reg, _ := newEngine(t)
	e.SetRule(Rule{Tenant: "t1", Scenario: "aftersale", Allow: []string{"order.*"}, Deny: []string{"order.refund"}, MaxRisk: action.RiskMoney})
	reg.Use(e.Middleware())
	res := reg.Execute(ctxFor("t1", "aftersale"), llm.ToolCall{Name: "order.refund", Args: "{}"})
	if !res.IsError || !strings.Contains(res.Content, "拒绝列表") {
		t.Fatalf("deny should win: %+v", res)
	}
}

func TestScopeRequired(t *testing.T) {
	e, reg, _ := newEngine(t)
	e.SetRule(Rule{Tenant: "t1", Scenario: "aftersale", MaxRisk: action.RiskMoney, RequireScope: true})
	reg.Use(e.Middleware())
	if res := reg.Execute(ctxFor("t1", "aftersale"), llm.ToolCall{Name: "order.refund", Args: "{}"}); !res.IsError {
		t.Fatal("missing scope should block")
	}
	if res := reg.Execute(ctxFor("t1", "aftersale", "order:refund"), llm.ToolCall{Name: "order.refund", Args: "{}"}); res.IsError {
		t.Fatalf("with scope should pass: %+v", res)
	}
}

func TestUnknownActionRejected(t *testing.T) {
	e, _, _ := newEngine(t)
	if err := e.Check(ctxFor("t1", "s"), llm.ToolCall{Name: "nope"}); err == nil {
		t.Fatal("unknown action must be rejected")
	}
}

func TestPatternMatch(t *testing.T) {
	if !match("order.*", "order.refund") || match("order.*", "ticket.create") {
		t.Fatal("prefix match wrong")
	}
	if !match("*", "anything") || !match("a.b", "a.b") {
		t.Fatal("exact match wrong")
	}
}
