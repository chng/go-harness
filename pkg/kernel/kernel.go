// Package kernel 是 L0 运行时内核：服务注册表 + 插件生命周期。
//
// 设计取舍（对齐 DSH 的 cordis，但放弃运行期任意上下线）：
//   - 服务在启动期由插件一次性注册，运行期只读。
//   - 插件按 Provides/Requires 做稳定拓扑排序；缺失依赖或成环在启动期报错。
//   - 关闭顺序是加载顺序的逆序（LIFO），保证依赖者先关。
package kernel

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"
)

// Context 是插件看到的内核句柄。
type Context struct {
	k   *Kernel
	log *slog.Logger
}

// Plugin 是一个可加载单元。
type Plugin struct {
	Name     string
	Provides []string // 本插件注册的服务名
	Requires []string // 本插件依赖的服务名（必须是内核已提供或其它插件 Provides）
	Apply    func(c *Context) error
	Close    func() error
}

// Kernel 是服务容器。
type Kernel struct {
	mu       sync.RWMutex
	services map[string]any
	provided []string
	closers  []namedCloser
	log      *slog.Logger
	closed   bool
}

type namedCloser struct {
	name string
	fn   func() error
}

// New 创建内核。logger 为 nil 时用 slog.Default()。
func New(logger *slog.Logger) *Kernel {
	if logger == nil {
		logger = slog.Default()
	}
	return &Kernel{services: make(map[string]any), log: logger}
}

// Provide 注册一个服务。同名重复注册会 panic —— 启动期错误必须立刻暴露。
func (k *Kernel) Provide(name string, v any) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.services[name]; ok {
		panic(fmt.Sprintf("kernel: service %q already provided", name))
	}
	k.services[name] = v
	k.provided = append(k.provided, name)
}

// Has 报告服务是否存在。
func (k *Kernel) Has(name string) bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	_, ok := k.services[name]
	return ok
}

// Lookup 取服务（any）。
func (k *Kernel) Lookup(name string) (any, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	v, ok := k.services[name]
	return v, ok
}

// Get 取类型化服务。
func Get[T any](k *Kernel, name string) (T, error) {
	var zero T
	v, ok := k.Lookup(name)
	if !ok {
		return zero, fmt.Errorf("kernel: service %q not found", name)
	}
	t, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("kernel: service %q has type %T, want %T", name, v, zero)
	}
	return t, nil
}

// Must 取类型化服务，失败即 panic（只在启动期使用）。
func Must[T any](k *Kernel, name string) T {
	v, err := Get[T](k, name)
	if err != nil {
		panic(err)
	}
	return v
}

// Services 返回已注册服务名（按提供顺序）。
func (k *Kernel) Services() []string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make([]string, len(k.provided))
	copy(out, k.provided)
	return out
}

// Logger 返回内核 logger。
func (k *Kernel) Logger() *slog.Logger { return k.log }

// Provide 在本插件命名空间下注册服务。
func (c *Context) Provide(name string, v any) { c.k.Provide(name, v) }

// Lookup 取 any 服务。
func (c *Context) Lookup(name string) (any, bool) { return c.k.Lookup(name) }

// Has 报告服务是否存在。
func (c *Context) Has(name string) bool { return c.k.Has(name) }

// Get 取类型化服务。
func GetT[T any](c *Context, name string) (T, error) { return Get[T](c.k, name) }

// Logger 返回带插件名的 logger。
func (c *Context) Logger() *slog.Logger { return c.log }

// OnClose 注册额外清理钩子（逆序执行）。
func (c *Context) OnClose(name string, fn func() error) { c.k.addCloser(name, fn) }

func (k *Kernel) addCloser(name string, fn func() error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.closers = append(k.closers, namedCloser{name: name, fn: fn})
}

// Use 按依赖顺序加载插件。加载后不支持运行期增删。
func (k *Kernel) Use(plugins ...Plugin) error {
	ordered, err := order(k, plugins)
	if err != nil {
		return err
	}
	for _, p := range ordered {
		c := &Context{k: k, log: k.log.With("plugin", p.Name)}
		if err := p.Apply(c); err != nil {
			return fmt.Errorf("kernel: plugin %q apply: %w", p.Name, err)
		}
		if p.Close != nil {
			k.addCloser(p.Name, p.Close)
		}
		k.log.Debug("plugin loaded", "plugin", p.Name)
	}
	return nil
}

// order 做稳定拓扑排序：先校验依赖可满足，再按声明顺序挑 indeg==0 的插件。
func order(k *Kernel, plugins []Plugin) ([]Plugin, error) {
	byName := make(map[string]Plugin, len(plugins))
	provider := make(map[string]string, len(plugins))
	for _, p := range plugins {
		if p.Name == "" {
			return nil, fmt.Errorf("kernel: plugin with empty name")
		}
		if _, dup := byName[p.Name]; dup {
			return nil, fmt.Errorf("kernel: duplicate plugin %q", p.Name)
		}
		byName[p.Name] = p
		for _, s := range p.Provides {
			if owner, dup := provider[s]; dup {
				return nil, fmt.Errorf("kernel: service %q provided by both %q and %q", s, owner, p.Name)
			}
			provider[s] = p.Name
		}
	}
	edges := make(map[string][]string, len(plugins))
	indeg := make(map[string]int, len(plugins))
	for _, p := range plugins {
		indeg[p.Name] = 0
	}
	for _, p := range plugins {
		for _, req := range p.Requires {
			owner, ok := provider[req]
			if !ok {
				if k.Has(req) {
					continue
				}
				return nil, fmt.Errorf("kernel: plugin %q requires service %q, but nobody provides it", p.Name, req)
			}
			if owner == p.Name {
				return nil, fmt.Errorf("kernel: plugin %q requires its own service %q", p.Name, req)
			}
			edges[owner] = append(edges[owner], p.Name)
			indeg[p.Name]++
		}
	}
	out := make([]Plugin, 0, len(plugins))
	done := make(map[string]bool, len(plugins))
	for len(out) < len(plugins) {
		progress := false
		for _, p := range plugins {
			if done[p.Name] || indeg[p.Name] != 0 {
				continue
			}
			done[p.Name] = true
			out = append(out, p)
			for _, next := range edges[p.Name] {
				indeg[next]--
			}
			progress = true
			break
		}
		if !progress {
			remaining := make([]string, 0, len(plugins)-len(out))
			for _, p := range plugins {
				if !done[p.Name] {
					remaining = append(remaining, p.Name)
				}
			}
			sort.Strings(remaining)
			return nil, fmt.Errorf("kernel: dependency cycle among plugins %v", remaining)
		}
	}
	return out, nil
}

// Close 逆序关闭所有注册的 close 钩子。
func (k *Kernel) Close() error {
	k.mu.Lock()
	if k.closed {
		k.mu.Unlock()
		return nil
	}
	k.closed = true
	closers := k.closers
	k.closers = nil
	k.mu.Unlock()

	var firstErr error
	for i := len(closers) - 1; i >= 0; i-- {
		if err := closers[i].fn(); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("kernel: close %q: %w", closers[i].name, err)
			}
			k.log.Error("close hook failed", "hook", closers[i].name, "err", err)
		}
	}
	return firstErr
}
