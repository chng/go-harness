// Package action 定义「业务动作」契约：业务能力以 Action 形式注册进框架。
//
// 业务方只写：名字、说明、风险等级、权限点、入参出参结构体、实现函数。
// 框架自动获得：JSON Schema（给模型看）、风险分级（决定是否需要审批）、
// 幂等键（防重复副作用）、审计记录（事件日志）。
package action

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/zhichengtrang/go-harness/pkg/llm"
	"github.com/zhichengtrang/go-harness/pkg/tools"
)

// Risk 是动作的风险等级，决定治理强度。
type Risk int

const (
	RiskRead      Risk = iota // 只读：查订单、查库存、检索
	RiskWriteSoft             // 可逆写：加备注、打标签
	RiskWriteHard             // 不可逆写：建单、发货、关闭
	RiskMoney                 // 涉及金额：退款、发券、扣款
)

func (r Risk) String() string {
	switch r {
	case RiskRead:
		return "read"
	case RiskWriteSoft:
		return "write-soft"
	case RiskWriteHard:
		return "write-hard"
	case RiskMoney:
		return "money"
	}
	return "unknown"
}

// NeedsApproval 表示该等级是否必须人工确认。
func (r Risk) NeedsApproval() bool { return r >= RiskWriteHard }

// NeedsIdempotency 表示该等级是否必须有幂等保护。
func (r Risk) NeedsIdempotency() bool { return r >= RiskWriteSoft }

// Meta 是动作的元数据（中间件据此做策略/审批/幂等判断）。
type Meta struct {
	Name       string
	Desc       string
	Risk       Risk
	Scopes     []string
	Idempotent bool // 业务侧是否天然幂等
	ReadOnly   bool // 可否并发执行
	Timeout    int  // 秒
	Confirm    func(ctx context.Context, args json.RawMessage) (string, bool)
	// Validate 是审批前的预检：业务上根本不成立的动作，不应该占用人工审批。
	Validate   func(ctx context.Context, args json.RawMessage) error
	Capability string // 归属的业务能力（插件名），仅用于展示/分组
}

// Catalog 按工具名索引动作元数据。
type Catalog struct {
	mu sync.RWMutex
	m  map[string]Meta
}

// NewCatalog 创建目录。
func NewCatalog() *Catalog { return &Catalog{m: make(map[string]Meta)} }

// Add 登记一个动作。
func (c *Catalog) Add(meta Meta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[meta.Name] = meta
}

// Get 查询动作元数据。
func (c *Catalog) Get(name string) (Meta, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	meta, ok := c.m[name]
	return meta, ok
}

// Names 返回全部动作名（排序）。
func (c *Catalog) Names() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.m))
	for k := range c.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Caller 是调用方身份，贯穿策略/审批/幂等/成本。
type Caller struct {
	Tenant    string
	User      string
	Scenario  string
	SessionID string
	Scopes    []string
}

type callerKey struct{}

// WithCaller 把调用方身份放进 ctx。
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// GetCaller 取出调用方身份。
func GetCaller(ctx context.Context) Caller {
	c, _ := ctx.Value(callerKey{}).(Caller)
	return c
}

// Register 把一个业务动作注册进工具注册表与目录。
func Register[In any, Out any](cat *Catalog, reg *tools.Registry, meta Meta, run func(ctx context.Context, in In) (Out, error)) error {
	if meta.Name == "" {
		return fmt.Errorf("action: 名字不能为空")
	}
	if meta.Confirm != nil {
		confirm := meta.Confirm
		meta.Confirm = func(ctx context.Context, raw json.RawMessage) (string, bool) {
			return confirm(ctx, raw)
		}
	}
	var zeroIn In
	schema, err := tools.SchemaFor(reflect.TypeOf(zeroIn))
	if err != nil {
		return fmt.Errorf("action %q: 入参 schema 生成失败: %w", meta.Name, err)
	}
	tool := tools.Tool{
		Name:        meta.Name,
		Description: meta.Desc,
		Schema:      schema,
		ReadOnly:    meta.ReadOnly || meta.Risk == RiskRead,
		Serial:      !meta.ReadOnly && meta.Risk != RiskRead,
		Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var in In
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&in); err != nil {
				return "", fmt.Errorf("参数无效: %w", err)
			}
			out, err := run(ctx, in)
			if err != nil {
				return "", err
			}
			encoded, err := json.Marshal(out)
			if err != nil {
				return "", fmt.Errorf("出参序列化失败: %w", err)
			}
			return string(encoded), nil
		},
	}
	if meta.Timeout > 0 {
		tool.Timeout = timeSeconds(meta.Timeout)
	}
	if err := reg.Register(tool); err != nil {
		return err
	}
	cat.Add(meta)
	return nil
}

// Summarize 生成给人看的审批摘要。
func Summarize(ctx context.Context, cat *Catalog, call llm.ToolCall) string {
	meta, ok := cat.Get(call.Name)
	if !ok {
		return fmt.Sprintf("调用 %s，参数 %s", call.Name, call.Args)
	}
	if meta.Confirm != nil {
		if summary, needed := meta.Confirm(ctx, json.RawMessage(call.Args)); needed || summary != "" {
			return summary
		}
	}
	return fmt.Sprintf("调用 %s（风险 %s），参数 %s", call.Name, meta.Risk, call.Args)
}
