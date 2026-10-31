// Package server 是后端业务 agent 的 HTTP 服务：会话、异步执行、SSE 流、审批。
//
// 与 CLI 的区别：
//   - POST 消息立即返回，执行在后台 goroutine（对应后端的 job）；
//   - SSE 按 seq 续传，断线可凭 Last-Event-ID 接上；
//   - 高风险动作会挂起 turn，审批通过后 Resume 继续同一个 turn。
package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zhichengtrang/go-harness/pkg/action"
	"github.com/zhichengtrang/go-harness/pkg/agent"
	"github.com/zhichengtrang/go-harness/pkg/app"
	"github.com/zhichengtrang/go-harness/pkg/domain"
	"github.com/zhichengtrang/go-harness/pkg/session"
)

//go:embed static/index.html
var staticFS embed.FS

// Server 是 HTTP 服务。
type Server struct {
	app *app.App
	log *slog.Logger
	mux *http.ServeMux

	mu       sync.Mutex
	sessions map[string]*runtime
	seq      atomic.Int64
}

type runtime struct {
	id    string
	store *session.Store
	ag    *agent.Agent
	mu    sync.Mutex
	busy  bool
	turns int
}

// New 创建服务。
func New(a *app.App) *Server {
	s := &Server{app: a, log: a.Log, mux: http.NewServeMux(), sessions: map[string]*runtime{}}
	s.routes()
	return s
}

// Handler 返回 http.Handler。
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.Handle("GET /", s.handleIndex())
	s.mux.HandleFunc("POST /api/sessions", s.handleCreateSession)
	s.mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	s.mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	s.mux.HandleFunc("POST /api/sessions/{id}/messages", s.handleMessage)
	s.mux.HandleFunc("GET /api/sessions/{id}/stream", s.handleStream)
	s.mux.HandleFunc("GET /api/approvals", s.handleListApprovals)
	s.mux.HandleFunc("POST /api/approvals/{id}", s.handleDecide)
	s.mux.HandleFunc("GET /api/actions", s.handleActions)
	s.mux.HandleFunc("GET /api/usage", s.handleUsage)
}

func (s *Server) handleIndex() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// ---- 会话 ----

type createReq struct {
	Title string `json:"title"`
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createReq
	_ = json.NewDecoder(r.Body).Decode(&req)
	id := fmt.Sprintf("s-%d", s.seq.Add(1))
	rt, err := s.openRuntime(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "创建会话失败: %v", err)
		return
	}
	s.mu.Lock()
	s.sessions[id] = rt
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "tenant": s.app.Config().Tenant, "scenario": s.app.Config().Scenario,
		"model": s.app.Config().Model, "title": req.Title,
	})
}

func (s *Server) openRuntime(id string) (*runtime, error) {
	store, ag, err := s.app.OpenSession(id)
	if err != nil {
		return nil, err
	}
	return &runtime{id: id, store: store, ag: ag}, nil
}

func (s *Server) getRuntime(id string) (*runtime, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, ok := s.sessions[id]
	return rt, ok
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	out := make([]map[string]any, 0, len(s.sessions))
	for _, rt := range s.sessions {
		out = append(out, map[string]any{
			"id": rt.id, "turns": rt.turns, "seq": rt.store.Seq(), "busy": rt.busy,
		})
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rt, ok := s.getRuntime(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "会话 %s 不存在", id)
		return
	}
	events := rt.store.Events()
	msgs, err := session.Fold(events)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "折叠事件失败: %v", err)
		return
	}
	pt, pending, _ := rt.ag.FindPendingTurn()
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "seq": rt.store.Seq(), "busy": rt.busy,
		"events": events, "messages": viewMessages(msgs),
		"pendingTurn": pending, "pending": pt,
		"approvals": s.app.Approvals.ListPending(s.app.Config().Tenant),
	})
}

type messageView struct {
	Role  string   `json:"role"`
	Text  string   `json:"text"`
	Turn  int      `json:"turn"`
	Tools []string `json:"tools,omitempty"`
}

func viewMessages(msgs []domain.Message) []messageView {
	out := make([]messageView, 0, len(msgs))
	for _, m := range msgs {
		v := messageView{Role: string(m.Role), Text: m.Text(), Turn: m.Turn}
		for _, tc := range m.ToolCalls() {
			v.Tools = append(v.Tools, tc.Name)
		}
		for _, b := range m.Blocks {
			if tr, ok := b.(domain.ToolResultBlock); ok {
				v.Text = tr.Content
			}
		}
		out = append(out, v)
	}
	return out
}

// ---- 发消息（异步执行）----

type messageReq struct {
	Text string `json:"text"`
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rt, ok := s.getRuntime(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "会话 %s 不存在", id)
		return
	}
	var req messageReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		writeErr(w, http.StatusBadRequest, "text 不能为空")
		return
	}
	rt.mu.Lock()
	if rt.busy {
		rt.mu.Unlock()
		writeErr(w, http.StatusConflict, "该会话上一轮还在执行，请等待完成")
		return
	}
	rt.busy = true
	rt.mu.Unlock()

	go func() {
		defer func() {
			rt.mu.Lock()
			rt.busy = false
			rt.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		ctx = action.WithCaller(ctx, s.app.Caller(id))
		oc, err := rt.ag.Run(ctx, req.Text)
		if err != nil {
			s.log.Error("turn failed", "session", id, "err", err)
			return
		}
		rt.mu.Lock()
		rt.turns = oc.Turn
		rt.mu.Unlock()
		s.log.Info("turn done", "session", id, "turn", oc.Turn, "reason", oc.Reason, "steps", oc.Steps, "tokens", oc.Usage.TotalTokens)
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"sessionId": id, "accepted": true})
}

// ---- SSE 流 ----

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rt, ok := s.getRuntime(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "会话 %s 不存在", id)
		return
	}
	after := int64(0)
	if v := r.URL.Query().Get("after"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			after = n
		}
	}
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > after {
			after = n
		}
	}
	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		writeErr(w, http.StatusInternalServerError, "不支持流式")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	deadline := time.Now().Add(2 * time.Minute)
	ticks := 0
	sawTerminal := false
	for {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		events := rt.store.Events()
		for _, ev := range events {
			if ev.Seq <= after {
				continue
			}
			after = ev.Seq
			if ev.Type == session.EvTurnEnd {
				sawTerminal = true
			}
			payload, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, payload)
		}
		flusher.Flush()
		rt.mu.Lock()
		busy := rt.busy
		rt.mu.Unlock()
		if sawTerminal && !busy {
			fmt.Fprintf(w, "event: done\ndata: {\"seq\":%d}\n\n", after)
			flusher.Flush()
			return
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(w, "event: timeout\ndata: {\"seq\":%d}\n\n", after)
			flusher.Flush()
			return
		}
		ticks++
		if ticks%50 == 0 {
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		}
		time.Sleep(120 * time.Millisecond)
	}
}

// ---- 审批 ----

func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	tenant := r.URL.Query().Get("tenant")
	if tenant == "" {
		tenant = s.app.Config().Tenant
	}
	writeJSON(w, http.StatusOK, s.app.Approvals.ListPending(tenant))
}

type decideReq struct {
	Approved bool   `json:"approved"`
	By       string `json:"by"`
	Reason   string `json:"reason"`
}

func (s *Server) handleDecide(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req decideReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	if req.By == "" {
		req.By = "approver"
	}
	apr, ok := s.app.Approvals.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "审批单 %s 不存在", id)
		return
	}
	res, err := s.app.Approvals.Decide(id, req.Approved, req.By, req.Reason)
	if err != nil {
		writeErr(w, http.StatusConflict, "%v", err)
		return
	}
	// 决策后都续跑同一个 turn：
	//   批准 → 真正执行该动作；
	//   驳回 → 中间件把「被驳回」作为工具结果回给模型，由它给用户解释。
	resumed := false
	if rt, ok := s.getRuntime(apr.SessionID); ok {
		resumed = s.resume(rt)
	}
	writeJSON(w, http.StatusOK, map[string]any{"approval": res, "resumed": resumed})
}

func (s *Server) resume(rt *runtime) bool {
	rt.mu.Lock()
	if rt.busy {
		rt.mu.Unlock()
		return false
	}
	rt.busy = true
	rt.mu.Unlock()
	go func() {
		defer func() {
			rt.mu.Lock()
			rt.busy = false
			rt.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		ctx = action.WithCaller(ctx, s.app.Caller(rt.id))
		oc, err := rt.ag.Resume(ctx)
		if err != nil {
			s.log.Warn("resume failed", "session", rt.id, "err", err)
			return
		}
		s.log.Info("turn resumed", "session", rt.id, "turn", oc.Turn, "reason", oc.Reason, "steps", oc.Steps)
	}()
	return true
}

// ---- 元信息 ----

func (s *Server) handleActions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"plugins": s.app.PluginNames,
		"actions": s.app.Actions(),
	})
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.app.Usage())
}
