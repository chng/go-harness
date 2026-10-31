// Package app 把框架装配成一个可运行的业务 agent 应用。
//
// 装配顺序即治理顺序：
//
//	内核插件（业务能力）→ 动作目录 → 策略 → 审批 → 幂等 → 模型网关
package app

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/zhichengtrang/go-harness/pkg/action"
	"github.com/zhichengtrang/go-harness/pkg/agent"
	"github.com/zhichengtrang/go-harness/pkg/approval"
	"github.com/zhichengtrang/go-harness/pkg/gateway"
	"github.com/zhichengtrang/go-harness/pkg/idem"
	"github.com/zhichengtrang/go-harness/pkg/kernel"
	"github.com/zhichengtrang/go-harness/pkg/llm"
	"github.com/zhichengtrang/go-harness/pkg/llm/adapters/mock"
	"github.com/zhichengtrang/go-harness/pkg/llm/adapters/openai"
	"github.com/zhichengtrang/go-harness/pkg/policy"
	"github.com/zhichengtrang/go-harness/pkg/quota"
	"github.com/zhichengtrang/go-harness/pkg/session"
	"github.com/zhichengtrang/go-harness/pkg/tools"
	"github.com/zhichengtrang/go-harness/plugins/inventory"
	"github.com/zhichengtrang/go-harness/plugins/order"
	"github.com/zhichengtrang/go-harness/plugins/ticket"
)

// Config 是应用配置。
type Config struct {
	DataDir     string // 会话数据目录
	Tenant      string
	User        string
	Scenario    string
	Model       string
	MaxSteps    int
	System      string
	QuotaTokens int    // 每租户每日 token 预算，0 表示不限
	UseMock     bool   // true 时用确定性假模型（离线 demo / 测试）
	BaseURL     string // 真实供应商地址
	APIKey      string
	Logger      *slog.Logger
}

// App 是装配好的应用。
type App struct {
	cfg         Config
	Cat         *action.Catalog
	Registry    *tools.Registry
	Approvals   *approval.MemoryStore
	Ledger      *idem.MemoryLedger
	Policy      *policy.Engine
	Limits      *quota.Limiter
	Gateway     *gateway.Gateway
	Log         *slog.Logger
	PluginNames []string
}

// DefaultSystem 是售后场景的系统提示。
const DefaultSystem = `你是电商售后客服助手。你可以查询订单、查询库存、发起退款、创建售后工单。
规则：
1. 涉及订单的诉求先用 order.lookup 查清楚，不要凭猜测回答。
2. 退款是涉钱操作，会进入人工审批；提交后如实告诉用户"已提交审批，等待人工确认"，不要说已经退成功。
3. 只回答查到的事实；金额、单号必须来自工具结果，不得编造。
4. 回复用中文，简洁、口语化。`

// New 装配应用。
func New(cfg Config) (*App, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Tenant == "" {
		cfg.Tenant = "demo"
	}
	if cfg.Scenario == "" {
		cfg.Scenario = "aftersale"
	}
	if cfg.Model == "" {
		cfg.Model = "deepseek-chat"
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 8
	}
	if cfg.System == "" {
		cfg.System = DefaultSystem
	}
	if cfg.DataDir == "" {
		cfg.DataDir = filepath.Join(os.TempDir(), "ghd-data")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}

	a := &App{
		cfg:       cfg,
		Cat:       action.NewCatalog(),
		Registry:  tools.NewRegistry(),
		Approvals: approval.NewMemoryStore(0),
		Ledger:    idem.NewMemoryLedger(),
		Limits:    quota.NewLimiter(cfg.QuotaTokens),
		Log:       cfg.Logger,
	}
	a.Policy = policy.New(a.Cat)

	// ---- 模型网关：主供应商 + 可选的确定性假模型 ----
	var chain []gateway.Provider
	if cfg.UseMock {
		chain = append(chain, gateway.Provider{Name: "mock", Adapter: mock.New()})
	} else {
		chain = append(chain, gateway.Provider{Name: "openai", Adapter: openai.New(openai.Config{
			ProviderName: "deepseek", BaseURL: cfg.BaseURL, APIKey: cfg.APIKey,
		})})
	}
	a.Gateway = gateway.New(chain, a.Limits, gateway.NewCostLedger(), cfg.Logger)

	// ---- 内核插件：三个业务能力 ----
	orderSvc := order.NewService()
	inventorySvc := inventory.NewService()
	ticketSvc := ticket.NewService()
	orderPlugin := order.Plugin(orderSvc)
	inventoryPlugin := inventory.Plugin()
	ticketPlugin := ticket.Plugin()

	k := kernel.New(cfg.Logger)
	err := k.Use(
		kernel.Plugin{
			Name: "framework-core", Provides: []string{"action.catalog", "tools.registry"},
			Apply: func(c *kernel.Context) error {
				c.Provide("action.catalog", a.Cat)
				c.Provide("tools.registry", a.Registry)
				return nil
			},
		},
		withDeps(orderPlugin, func(c *kernel.Context) error {
			cat, err := kernel.GetT[*action.Catalog](c, "action.catalog")
			if err != nil {
				return err
			}
			reg, err := kernel.GetT[*tools.Registry](c, "tools.registry")
			if err != nil {
				return err
			}
			return order.Register(orderSvc, cat, reg)
		}),
		withDeps(inventoryPlugin, func(c *kernel.Context) error {
			cat, err := kernel.GetT[*action.Catalog](c, "action.catalog")
			if err != nil {
				return err
			}
			reg, err := kernel.GetT[*tools.Registry](c, "tools.registry")
			if err != nil {
				return err
			}
			return inventory.Register(inventorySvc, cat, reg)
		}),
		withDeps(ticketPlugin, func(c *kernel.Context) error {
			cat, err := kernel.GetT[*action.Catalog](c, "action.catalog")
			if err != nil {
				return err
			}
			reg, err := kernel.GetT[*tools.Registry](c, "tools.registry")
			if err != nil {
				return err
			}
			return ticket.Register(ticketSvc, cat, reg)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("装配插件失败: %w", err)
	}
	a.PluginNames = []string{orderPlugin.Name, inventoryPlugin.Name, ticketPlugin.Name}

	// ---- 工具管线：策略 → 审批 → 幂等 ----
	a.Registry.Use(
		a.Policy.Middleware(),
		a.Approvals.Middleware(a.Cat),
		a.Ledger.Middleware(a.Cat),
	)

	// ---- 场景授权规则 ----
	a.Policy.SetDefault(policy.Rule{MaxRisk: action.RiskRead}) // 默认只读，fail-closed
	a.Policy.SetRule(policy.Rule{
		Tenant: cfg.Tenant, Scenario: cfg.Scenario,
		Allow:   []string{"order.*", "inventory.*", "ticket.*"},
		MaxRisk: action.RiskMoney,
	})
	return a, nil
}

// withDeps 给能力插件挂上对框架服务的依赖与装配逻辑。
func withDeps(p kernel.Plugin, apply func(*kernel.Context) error) kernel.Plugin {
	p.Requires = append(p.Requires, "action.catalog", "tools.registry")
	p.Apply = apply
	return p
}

// Config 暴露只读配置。
func (a *App) Config() Config { return a.cfg }

// OpenSession 打开（或创建）一个会话及其 agent。
func (a *App) OpenSession(sessionID string) (*session.Store, *agent.Agent, error) {
	dir := filepath.Join(a.cfg.DataDir, "sessions", sessionID)
	store, err := session.Open(dir)
	if err != nil {
		return nil, nil, err
	}
	adapter := a.Gateway.WithTenant(a.cfg.Tenant)
	ag := agent.New(adapter, store, a.Registry, agent.Config{
		Provider: a.Gateway.Provider(), Model: a.cfg.Model, System: a.cfg.System,
		MaxSteps: a.cfg.MaxSteps,
	}, a.Log, io.Discard)
	return store, ag, nil
}

// Caller 返回 demo 场景的调用方身份。
func (a *App) Caller(sessionID string) action.Caller {
	return action.Caller{
		Tenant: a.cfg.Tenant, User: a.cfg.User, Scenario: a.cfg.Scenario,
		SessionID: sessionID, Scopes: []string{"order:refund", "ticket:create"},
	}
}

// Usage 汇总用量。
func (a *App) Usage() map[string]any {
	used, limit := a.Limits.Usage(a.cfg.Tenant)
	tokens, amount := a.Gateway.Costs().Total(a.cfg.Tenant)
	return map[string]any{
		"tenant":      a.cfg.Tenant,
		"tokensToday": used,
		"tokenLimit":  limit,
		"costTokens":  tokens,
		"costAmount":  amount,
		"provider":    a.Gateway.Provider(),
		"model":       a.cfg.Model,
		"mock":        a.cfg.UseMock,
		"currency":    "CNY",
		"recordedAt":  time.Now().UTC().Format(time.RFC3339),
	}
}

// ActionView 是给前端展示的动作信息。
type ActionView struct {
	Name          string   `json:"name"`
	Desc          string   `json:"desc"`
	Risk          string   `json:"risk"`
	Capability    string   `json:"capability"`
	Scopes        []string `json:"scopes,omitempty"`
	NeedsApproval bool     `json:"needsApproval"`
	Idempotent    bool     `json:"idempotent"`
}

// Actions 列出动作目录。
func (a *App) Actions() []ActionView {
	out := make([]ActionView, 0)
	for _, name := range a.Cat.Names() {
		meta, _ := a.Cat.Get(name)
		out = append(out, ActionView{
			Name: meta.Name, Desc: meta.Desc, Risk: meta.Risk.String(), Capability: meta.Capability,
			Scopes: meta.Scopes, NeedsApproval: meta.Risk.NeedsApproval(), Idempotent: meta.Risk.NeedsIdempotency(),
		})
	}
	return out
}

var _ = llm.CodeUnknown
