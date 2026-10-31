// Package idem 是副作用幂等台账：保证同一个工具调用最多生效一次。
//
// 为什么需要它：模型重试、worker 崩溃后重跑、审批通过后 Resume，都会让同一次
// 调用被执行多次。台账以 (租户, 会话, callId) 为键，命中已成功就直接返回缓存结果，
// 命中「已领取但结果未知」就拒绝重试并要求人工核对。
package idem

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/zhichengtrang/go-harness/pkg/action"
	"github.com/zhichengtrang/go-harness/pkg/llm"
	"github.com/zhichengtrang/go-harness/pkg/tools"
)

// Status 是台账状态。
type Status string

const (
	StatusClaimed   Status = "claimed"   // 已领取，尚未有结果
	StatusSucceeded Status = "succeeded" // 已生效
	StatusUnknown   Status = "unknown"   // 领取后中断，结果未知
)

// Entry 是一条台账。
type Entry struct {
	Status  Status    `json:"status"`
	Result  string    `json:"result,omitempty"`
	IsError bool      `json:"isError,omitempty"`
	At      time.Time `json:"at"`
}

// Ledger 是台账接口。
type Ledger interface {
	Get(tenant, sessionID, callID string) (Entry, bool)
	Claim(tenant, sessionID, callID string) (Entry, bool)
	Complete(tenant, sessionID, callID string, e Entry)
}

// MemoryLedger 是内存实现。
type MemoryLedger struct {
	mu sync.Mutex
	m  map[string]Entry
}

// NewMemoryLedger 创建台账。
func NewMemoryLedger() *MemoryLedger { return &MemoryLedger{m: map[string]Entry{}} }

func key(tenant, sessionID, callID string) string {
	return tenant + string('/') + sessionID + string('/') + callID
}

// Get 查台账。
func (l *MemoryLedger) Get(tenant, sessionID, callID string) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.m[key(tenant, sessionID, callID)]
	return e, ok
}

// Claim 领取执行权；返回 (已有条目, 是否存在)。
func (l *MemoryLedger) Claim(tenant, sessionID, callID string) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := key(tenant, sessionID, callID)
	if e, ok := l.m[k]; ok {
		return e, true
	}
	l.m[k] = Entry{Status: StatusClaimed, At: time.Now().UTC()}
	return Entry{Status: StatusClaimed}, false
}

// Complete 记录结果。
func (l *MemoryLedger) Complete(tenant, sessionID, callID string, e Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	l.m[key(tenant, sessionID, callID)] = e
}

// Middleware 把幂等接进工具管线（应放在最内层，紧贴真正执行）。
func (l *MemoryLedger) Middleware(cat *action.Catalog) tools.Middleware {
	return func(next tools.ExecFunc) tools.ExecFunc {
		return func(ctx context.Context, call llm.ToolCall) tools.Result {
			meta, ok := cat.Get(call.Name)
			if !ok || (!meta.Risk.NeedsIdempotency() && !meta.Idempotent) {
				return next(ctx, call)
			}
			caller := action.GetCaller(ctx)
			existing, found := l.Claim(caller.Tenant, caller.SessionID, call.ID)
			if found {
				switch existing.Status {
				case StatusSucceeded:
					return tools.Result{
						Content: existing.Result + "\n[幂等命中：该调用此前已生效，本次未重复执行]",
						IsError: existing.IsError,
					}
				case StatusClaimed, StatusUnknown:
					return tools.Result{
						Content: fmt.Sprintf("该调用（%s）上次执行结果未知（台账状态 %s），为避免重复副作用已拒绝本次重试，请人工核对后处理。", call.ID, existing.Status),
						IsError: true,
					}
				}
			}
			res := next(ctx, call)
			if res.Suspend != nil {
				// 挂起不代表执行过：撤销领取，等审批通过后重新领
				l.mu.Lock()
				delete(l.m, key(caller.Tenant, caller.SessionID, call.ID))
				l.mu.Unlock()
				return res
			}
			status := StatusSucceeded
			if res.IsError {
				status = StatusUnknown
			}
			l.Complete(caller.Tenant, caller.SessionID, call.ID, Entry{Status: status, Result: res.Content, IsError: res.IsError})
			return res
		}
	}
}
