// Package policy 是工具执行前的策略检查：允许哪些动作、最高风险等级、权限点。
//
// 默认拒绝（fail-closed）：没配规则的场景只能调只读动作。
package policy

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/zhichengtrang/go-harness/pkg/action"
	"github.com/zhichengtrang/go-harness/pkg/llm"
	"github.com/zhichengtrang/go-harness/pkg/tools"
)

// Rule 是一个场景的授权规则。
type Rule struct {
	Tenant       string      // 空表示不限租户
	Scenario     string      // 空表示不限场景
	Allow        []string    // 允许的动作名或前缀（`order.*`），空表示按 MaxRisk 放行
	Deny         []string    // 显式拒绝（优先于 Allow）
	MaxRisk      action.Risk // 允许的最高风险等级，零值即 RiskRead
	RequireScope bool        // 是否强制调用方持有动作声明的全部权限点
}

// Engine 做规则匹配。
type Engine struct {
	cat   *action.Catalog
	mu    sync.RWMutex
	rules map[string]Rule
	def   Rule
}

// New 创建策略引擎。默认规则是「只读放行」。
func New(cat *action.Catalog) *Engine {
	return &Engine{cat: cat, rules: map[string]Rule{}, def: Rule{MaxRisk: action.RiskRead}}
}

// SetDefault 设置兜底规则。
func (e *Engine) SetDefault(r Rule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.def = r
}

// SetRule 设置某个 (租户, 场景) 的规则。
func (e *Engine) SetRule(r Rule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules[key(r.Tenant, r.Scenario)] = r
}

func key(tenant, scenario string) string { return tenant + string('/') + scenario }

// resolve 按 场景精确 → 租户级 → 全局兜底 的顺序找规则。
func (e *Engine) resolve(c action.Caller) Rule {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if r, ok := e.rules[key(c.Tenant, c.Scenario)]; ok {
		return r
	}
	if r, ok := e.rules[key(c.Tenant, "")]; ok {
		return r
	}
	if r, ok := e.rules[key("", c.Scenario)]; ok {
		return r
	}
	if r, ok := e.rules[key("", "")]; ok {
		return r
	}
	return e.def
}

// Check 判定一次调用是否允许。
func (e *Engine) Check(ctx context.Context, call llm.ToolCall) error {
	c := action.GetCaller(ctx)
	meta, ok := e.cat.Get(call.Name)
	if !ok {
		return fmt.Errorf("策略拒绝：未知动作 %q", call.Name)
	}
	rule := e.resolve(c)
	for _, pattern := range rule.Deny {
		if match(pattern, call.Name) {
			return fmt.Errorf("策略拒绝：动作 %q 在拒绝列表中", call.Name)
		}
	}
	if len(rule.Allow) > 0 {
		allowed := false
		for _, pattern := range rule.Allow {
			if match(pattern, call.Name) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("策略拒绝：场景 %q 未授权动作 %q", c.Scenario, call.Name)
		}
	}
	if meta.Risk > rule.MaxRisk {
		return fmt.Errorf("策略拒绝：动作 %q 风险等级 %s 超出场景允许上限 %s", call.Name, meta.Risk, rule.MaxRisk)
	}
	if rule.RequireScope {
		for _, need := range meta.Scopes {
			if !hasScope(c.Scopes, need) {
				return fmt.Errorf("策略拒绝：缺少权限点 %q（动作 %q）", need, call.Name)
			}
		}
	}
	return nil
}

// Middleware 把策略接进工具管线（建议放在审计之后、审批之前）。
func (e *Engine) Middleware() tools.Middleware {
	return func(next tools.ExecFunc) tools.ExecFunc {
		return func(ctx context.Context, call llm.ToolCall) tools.Result {
			if err := e.Check(ctx, call); err != nil {
				return tools.Result{Content: err.Error(), IsError: true}
			}
			return next(ctx, call)
		}
	}
}

func hasScope(have []string, need string) bool {
	for _, s := range have {
		if s == need || s == "*" {
			return true
		}
	}
	return false
}

// match 支持精确匹配与 `prefix.*` 前缀匹配。
func match(pattern, name string) bool {
	if pattern == "*" || pattern == name {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	}
	return false
}
