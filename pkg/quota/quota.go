// Package quota 是租户级 token 预算：调用前判定，超限直接拒绝。
package quota

import (
	"fmt"
	"sync"
	"time"

	"github.com/zhichengtrang/go-harness/pkg/llm"
)

type window struct {
	day  string
	used int
}

// Limiter 按租户维护「当日 token 预算」。
type Limiter struct {
	mu      sync.Mutex
	m       map[string]*window
	limits  map[string]int
	def     int
	nowFunc func() time.Time
}

// NewLimiter 创建限流器。defTokens<=0 表示不限制。
func NewLimiter(defTokens int) *Limiter {
	return &Limiter{m: map[string]*window{}, limits: map[string]int{}, def: defTokens, nowFunc: time.Now}
}

// SetLimit 覆盖某个租户的上限。
func (l *Limiter) SetLimit(tenant string, tokens int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limits[tenant] = tokens
}

func (l *Limiter) limitOf(tenant string) int {
	if v, ok := l.limits[tenant]; ok {
		return v
	}
	return l.def
}

// Reserve 在调用前预占额度。返回剩余额度；超限返回带 QUOTA 错误码的错误。
func (l *Limiter) Reserve(tenant string, est int) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	limit := l.limitOf(tenant)
	if limit <= 0 {
		return -1, nil
	}
	today := l.nowFunc().UTC().Format("2006-01-02")
	w, ok := l.m[tenant]
	if !ok || w.day != today {
		w = &window{day: today}
		l.m[tenant] = w
	}
	if w.used+est > limit {
		return limit - w.used, &llm.Error{
			Code:    llm.CodeQuota,
			Message: fmt.Sprintf("租户 %s 今日 token 预算已用 %d/%d，本次预估 %d，已拒绝调用", tenant, w.used, limit, est),
		}
	}
	return limit - w.used - est, nil
}

// Commit 记录实际用量。
func (l *Limiter) Commit(tenant string, tokens int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	today := l.nowFunc().UTC().Format("2006-01-02")
	w, ok := l.m[tenant]
	if !ok || w.day != today {
		w = &window{day: today}
		l.m[tenant] = w
	}
	w.used += tokens
}

// Usage 返回 (已用, 上限)。
func (l *Limiter) Usage(tenant string) (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	used := 0
	if w, ok := l.m[tenant]; ok {
		used = w.used
	}
	return used, l.limitOf(tenant)
}
