// Package tools 是 L5 工具系统：注册表 + 执行管线 + JSON Schema 生成。
//
// 执行管线（顺序即语义）：
//
//	守卫（白名单/审批）→ 参数 JSON 校验 → 超时 ctx → 执行 → 输出截断 → 结果
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zhichengtrang/go-harness/pkg/llm"
)

// Context 是工具执行上下文别名（缩短 Define 的函数签名）。
type Context = context.Context

// DefaultMaxOutput 是工具输出上限（字节），超出则截断并标记。
const DefaultMaxOutput = 32 * 1024

// DefaultTimeout 是单个工具默认超时。
const DefaultTimeout = 120 * time.Second

// Tool 是一个工具定义。
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	ReadOnly    bool          // 只读工具可并发执行
	Serial      bool          // 需要独占执行（有副作用/依赖顺序）
	MaxOutput   int           // 0 表示用 DefaultMaxOutput
	Timeout     time.Duration // 0 表示用 DefaultTimeout
	Execute     func(ctx context.Context, args json.RawMessage) (string, error)
}

// Result 是工具执行结果。
type Result struct {
	Content   string
	IsError   bool
	Truncated bool
	Elapsed   time.Duration
	// Suspend 非 nil 表示这次调用没有真正执行，被挂起等待外部决策（如人工审批）。
	// agent 会因此把 turn 停在 waiting-approval 终态，之后由 Resume 继续。
	Suspend *Suspend
}

// Suspend 描述一次挂起。
type Suspend struct {
	Kind   string // "approval"
	ID     string // 审批单 ID
	Reason string
}

// ExecFunc 是工具执行的可组合签名。
type ExecFunc func(ctx context.Context, call llm.ToolCall) Result

// Middleware 是工具执行的中间件：审计、策略、审批、幂等都在这一层。
// 顺序即语义：先注册的先执行（最外层）。
type Middleware func(next ExecFunc) ExecFunc

// Guard 是执行前检查（白名单、审批、沙箱）。
type Guard interface {
	Check(ctx context.Context, name string, args json.RawMessage) error
}

// GuardFunc 把函数适配成 Guard。
type GuardFunc func(ctx context.Context, name string, args json.RawMessage) error

// Check 实现 Guard。
func (f GuardFunc) Check(ctx context.Context, name string, args json.RawMessage) error {
	return f(ctx, name, args)
}

// Registry 是工具注册表。
type Registry struct {
	mu          sync.RWMutex
	byName      map[string]Tool
	guards      []Guard
	middlewares []Middleware
}

// NewRegistry 创建注册表。
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Tool)}
}

// Register 注册工具。启动期错误立即返回，便于 fail fast。
func (r *Registry) Register(t Tool) error {
	if t.Name == "" {
		return fmt.Errorf("tools: tool with empty name")
	}
	if t.Execute == nil {
		return fmt.Errorf("tools: tool %q has no Execute", t.Name)
	}
	if len(t.Schema) > 0 && !json.Valid(t.Schema) {
		return fmt.Errorf("tools: tool %q has invalid JSON schema", t.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[t.Name]; dup {
		return fmt.Errorf("tools: tool %q already registered", t.Name)
	}
	if t.MaxOutput == 0 {
		t.MaxOutput = DefaultMaxOutput
	}
	if t.Timeout == 0 {
		t.Timeout = DefaultTimeout
	}
	r.byName[t.Name] = t
	return nil
}

// Use 追加工具执行中间件（最外层最先执行）。
func (r *Registry) Use(mw ...Middleware) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.middlewares = append(r.middlewares, mw...)
}

// UseGuard 追加一个执行前守卫（按注册顺序调用）。
func (r *Registry) UseGuard(g Guard) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guards = append(r.guards, g)
}

// Lookup 取工具。
func (r *Registry) Lookup(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.byName[name]
	return t, ok
}

// Names 返回已注册工具名（稳定排序）。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byName))
	for name := range r.byName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Specs 返回给模型看的工具声明。
func (r *Registry) Specs() []llm.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]llm.ToolSpec, 0, len(names))
	for _, name := range names {
		t := r.byName[name]
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage("{\"type\":\"object\",\"properties\":{}}")
		}
		out = append(out, llm.ToolSpec{Name: t.Name, Description: t.Description, Schema: schema})
	}
	return out
}

// Execute 走完整管线（中间件 → 守卫 → 执行 → 截断）执行一次工具调用。
func (r *Registry) Execute(ctx context.Context, call llm.ToolCall) Result {
	r.mu.RLock()
	mws := r.middlewares
	r.mu.RUnlock()
	if len(mws) == 0 {
		return r.execute(ctx, call)
	}
	next := ExecFunc(func(ctx context.Context, c llm.ToolCall) Result { return r.execute(ctx, c) })
	for i := len(mws) - 1; i >= 0; i-- {
		next = mws[i](next)
	}
	return next(ctx, call)
}

// execute 是管线内核：守卫校验 + 执行 + 输出截断。
func (r *Registry) execute(ctx context.Context, call llm.ToolCall) Result {
	start := time.Now()
	finish := func(res Result) Result {
		res.Elapsed = time.Since(start)
		return res
	}
	t, ok := r.Lookup(call.Name)
	if !ok {
		return finish(Result{Content: fmt.Sprintf("unknown tool %q; available: %s", call.Name, strings.Join(r.Names(), ", ")), IsError: true})
	}
	args := strings.TrimSpace(call.Args)
	if args == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		return finish(Result{Content: "invalid JSON arguments: " + truncate(args, 200), IsError: true})
	}
	raw := json.RawMessage(args)

	r.mu.RLock()
	guards := r.guards
	r.mu.RUnlock()
	for _, g := range guards {
		if err := g.Check(ctx, t.Name, raw); err != nil {
			return finish(Result{Content: err.Error(), IsError: true})
		}
	}

	cctx := ctx
	var cancel context.CancelFunc
	if t.Timeout > 0 {
		cctx, cancel = context.WithTimeout(ctx, t.Timeout)
		defer cancel()
	}
	out, err := t.Execute(cctx, raw)
	if err != nil {
		msg := err.Error()
		if cctx.Err() == context.DeadlineExceeded {
			msg = fmt.Sprintf("tool %q timed out after %s", t.Name, t.Timeout)
		}
		return finish(Result{Content: msg, IsError: true})
	}
	content, truncated := truncateWithFlag(out, t.MaxOutput)
	return finish(Result{Content: content, Truncated: truncated})
}

func truncate(s string, limit int) string {
	out, _ := truncateWithFlag(s, limit)
	return out
}

// truncateWithFlag 按 UTF-8 安全截断。
func truncateWithFlag(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	b := []byte(s)[:limit]
	// 回退到合法 UTF-8 边界
	for len(b) > 0 && !utf8ValidTail(b) {
		b = b[:len(b)-1]
	}
	return string(b) + fmt.Sprintf("\n... [输出被截断，原长 %d 字节]", len(s)), true
}

func utf8ValidTail(b []byte) bool {
	for i := len(b) - 1; i >= 0 && i >= len(b)-4; i-- {
		if b[i]&0xC0 != 0x80 {
			return isValidRuneStart(b[i:])
		}
	}
	return true
}

func isValidRuneStart(b []byte) bool {
	switch {
	case len(b) == 0:
		return true
	case b[0] < 0x80:
		return true
	case b[0]&0xE0 == 0xC0:
		return len(b) >= 2
	case b[0]&0xF0 == 0xE0:
		return len(b) >= 3
	case b[0]&0xF8 == 0xF0:
		return len(b) >= 4
	}
	return false
}
