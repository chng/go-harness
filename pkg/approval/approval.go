// Package approval 实现人工审批门：高风险/涉钱动作必须有人点确认。
//
// 工作方式：中间件拦下动作 → 落一张审批单 → 返回 tools.Suspend，让 turn 停在
// waiting-approval 终态；审批人决策后，agent.Resume 补跑这次调用。
package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/zhichengtrang/go-harness/pkg/action"
	"github.com/zhichengtrang/go-harness/pkg/llm"
	"github.com/zhichengtrang/go-harness/pkg/tools"
)

// Status 是审批状态。
type Status string

const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusRejected Status = "rejected"
)

// Request 是一张审批单。
type Request struct {
	ID        string        `json:"id"`
	Tenant    string        `json:"tenant"`
	SessionID string        `json:"sessionId"`
	CallID    string        `json:"callId"`
	Tool      string        `json:"tool"`
	Args      string        `json:"args"`
	Risk      string        `json:"risk"`
	Summary   string        `json:"summary"`
	Status    Status        `json:"status"`
	CreatedAt time.Time     `json:"createdAt"`
	DecidedAt time.Time     `json:"decidedAt,omitempty"`
	DecidedBy string        `json:"decidedBy,omitempty"`
	Reason    string        `json:"reason,omitempty"`
	TTL       time.Duration `json:"-"`
}

// Store 是审批存储接口（内存实现用于 demo，生产换数据库）。
type Store interface {
	Create(r Request) (Request, error)
	Get(id string) (Request, bool)
	Decide(id string, approved bool, by, reason string) (Request, error)
	ListPending(tenant string) []Request
	FindByCall(tenant, sessionID, callID string) (Request, bool)
}

// MemoryStore 是内存实现。
type MemoryStore struct {
	mu     sync.Mutex
	seq    int
	byID   map[string]Request
	byCall map[string]string
	ttl    time.Duration
}

// NewMemoryStore 创建内存审批存储。ttl<=0 表示不过期。
func NewMemoryStore(ttl time.Duration) *MemoryStore {
	return &MemoryStore{byID: map[string]Request{}, byCall: map[string]string{}, ttl: ttl}
}

func callKey(tenant, sessionID, callID string) string {
	return tenant + string('/') + sessionID + string('/') + callID
}

// Create 建单。
func (m *MemoryStore) Create(r Request) (Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Tool == "" {
		return Request{}, fmt.Errorf("approval: tool 不能为空")
	}
	m.seq++
	r.ID = fmt.Sprintf("ap-%04d", m.seq)
	r.Status = StatusPending
	r.CreatedAt = time.Now().UTC()
	if r.TTL == 0 {
		r.TTL = m.ttl
	}
	m.byID[r.ID] = r
	m.byCall[callKey(r.Tenant, r.SessionID, r.CallID)] = r.ID
	return r, nil
}

// Get 查单。
func (m *MemoryStore) Get(id string) (Request, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byID[id]
	if !ok {
		return Request{}, false
	}
	return r, true
}

// Decide 审批。
func (m *MemoryStore) Decide(id string, approved bool, by, reason string) (Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byID[id]
	if !ok {
		return Request{}, fmt.Errorf("approval: 审批单 %q 不存在", id)
	}
	if r.Status != StatusPending {
		return Request{}, fmt.Errorf("approval: 审批单 %q 已经是 %s 状态", id, r.Status)
	}
	if approved {
		r.Status = StatusApproved
	} else {
		r.Status = StatusRejected
	}
	r.DecidedAt = time.Now().UTC()
	r.DecidedBy = by
	r.Reason = reason
	m.byID[id] = r
	return r, nil
}

// ListPending 列出待审单。
func (m *MemoryStore) ListPending(tenant string) []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Request, 0, len(m.byID))
	for _, r := range m.byID {
		if r.Status != StatusPending {
			continue
		}
		if tenant != "" && r.Tenant != tenant {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// FindByCall 按调用定位审批单（同一 (会话, callId) 只会有一张）。
func (m *MemoryStore) FindByCall(tenant, sessionID, callID string) (Request, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byCall[callKey(tenant, sessionID, callID)]
	if !ok {
		return Request{}, false
	}
	r, ok := m.byID[id]
	return r, ok
}

// Middleware 把审批接进工具管线（建议顺序：审计 → 策略 → 审批 → 幂等）。
func (m *MemoryStore) Middleware(cat *action.Catalog) tools.Middleware {
	return func(next tools.ExecFunc) tools.ExecFunc {
		return func(ctx context.Context, call llm.ToolCall) tools.Result {
			meta, ok := cat.Get(call.Name)
			if !ok || !meta.Risk.NeedsApproval() {
				return next(ctx, call)
			}
			caller := action.GetCaller(ctx)

			// 预检：业务上就不成立的动作直接拒绝，不浪费审批人的时间
			if meta.Validate != nil {
				if err := meta.Validate(ctx, json.RawMessage(call.Args)); err != nil {
					return tools.Result{Content: err.Error(), IsError: true}
				}
			}

			if req, found := m.FindByCall(caller.Tenant, caller.SessionID, call.ID); found {
				switch req.Status {
				case StatusApproved:
					res := next(ctx, call)
					res.Content += "\n[审批单 " + req.ID + " 已批准，审批人 " + req.DecidedBy + "]"
					return res
				case StatusRejected:
					return tools.Result{
						Content: fmt.Sprintf("操作被拒绝：审批单 %s 由 %s 驳回（理由：%s），未执行任何改动", req.ID, req.DecidedBy, req.Reason),
						IsError: true,
					}
				case StatusPending:
					return tools.Result{
						Content: fmt.Sprintf("等待人工审批：审批单 %s（%s）。审批通过后会自动继续执行。", req.ID, req.Summary),
						Suspend: &tools.Suspend{Kind: "approval", ID: req.ID, Reason: req.Summary},
					}
				}
			}
			summary := action.Summarize(ctx, cat, call)
			req, err := m.Create(Request{
				Tenant: caller.Tenant, SessionID: caller.SessionID, CallID: call.ID,
				Tool: call.Name, Args: call.Args, Risk: meta.Risk.String(), Summary: summary,
			})
			if err != nil {
				return tools.Result{Content: "审批单创建失败: " + err.Error(), IsError: true}
			}
			return tools.Result{
				Content: fmt.Sprintf("已提交人工审批：审批单 %s（%s）。审批通过后会自动继续执行。", req.ID, summary),
				Suspend: &tools.Suspend{Kind: "approval", ID: req.ID, Reason: summary},
			}
		}
	}
}
