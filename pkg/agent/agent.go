// Package agent 是 L4：Turn/Step 循环 + 可恢复执行。
//
// 不变量：
//   - 每个 turn 恰好一条 turn/end（正常、失败、挂起都要写）；
//   - 工具结果按调用顺序写回事件，与模型给出的 callId 对应；
//   - 工具被挂起（等待审批）时，turn 以 waiting-approval 收尾，之后由 Resume 继续同一个 turn。
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/zhichengtrang/go-harness/pkg/domain"
	"github.com/zhichengtrang/go-harness/pkg/llm"
	"github.com/zhichengtrang/go-harness/pkg/session"
	"github.com/zhichengtrang/go-harness/pkg/tools"
)

// Config 是 agent 配置。
type Config struct {
	Provider        string
	Model           string
	System          string
	SystemExtra     string
	MaxSteps        int
	Temperature     *float64
	MaxTokens       int
	ReasoningEffort string
	Retry           llm.RetryPolicy
	StartTurn       int
}

// Outcome 是一次 Run/Resume 的结果。
type Outcome struct {
	Text     string
	Steps    int
	Usage    llm.Usage
	Reason   domain.TurnEndReason
	Detail   string
	Duration time.Duration
	Turn     int
}

// Agent 驱动一个会话。
type Agent struct {
	adapter       llm.Adapter
	store         session.EventStore
	tools         *tools.Registry
	cfg           Config
	log           *slog.Logger
	out           io.Writer
	showReasoning bool
	startTurn     int
}

// New 创建 agent。
func New(adapter llm.Adapter, store session.EventStore, reg *tools.Registry, cfg Config, log *slog.Logger, out io.Writer) *Agent {
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 12
	}
	if cfg.Retry.MaxAttempts <= 0 {
		cfg.Retry = llm.DefaultRetryPolicy()
	}
	if log == nil {
		log = slog.Default()
	}
	if out == nil {
		out = io.Discard
	}
	startTurn := cfg.StartTurn
	for _, ev := range store.Events() {
		if ev.Type != session.EvTurnStart {
			continue
		}
		var d session.TurnStartData
		if json.Unmarshal(ev.Data, &d) == nil && d.Turn > startTurn {
			startTurn = d.Turn
		}
	}
	return &Agent{adapter: adapter, store: store, tools: reg, cfg: cfg, log: log, out: out, startTurn: startTurn}
}

// ShowReasoning 控制是否把推理内容写到输出。
func (a *Agent) ShowReasoning(v bool) { a.showReasoning = v }

// Run 处理一条用户输入。
func (a *Agent) Run(ctx context.Context, userText string) (Outcome, error) {
	a.startTurn++
	turn := a.startTurn
	if _, err := a.store.Append(session.EvTurnStart, session.TurnStartData{Turn: turn}); err != nil {
		return Outcome{}, err
	}
	if _, err := a.store.Append(session.EvUserMessage, session.UserMessageData{Text: userText, Turn: turn}); err != nil {
		return Outcome{}, err
	}
	return a.runLoop(ctx, turn, 1)
}

// PendingTurn 描述一个未结束的 turn。
type PendingTurn struct {
	Turn     int
	LastStep int
	Pending  []domain.ToolCallBlock
	OpenTurn bool
}

// FindPendingTurn 查出未完成的 turn（供 Resume 与外部调度判断）。
func (a *Agent) FindPendingTurn() (PendingTurn, bool, error) {
	events := a.store.Events()
	turn := 0
	lastStep := 0
	ended := map[int]domain.TurnEndReason{}
	results := map[int]map[string]bool{}
	calls := map[int]map[int][]domain.ToolCallBlock{}

	for _, ev := range events {
		switch ev.Type {
		case session.EvTurnStart:
			var d session.TurnStartData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return PendingTurn{}, false, err
			}
			if d.Turn > turn {
				turn = d.Turn
			}
		case session.EvTurnEnd:
			var d session.TurnEndData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return PendingTurn{}, false, err
			}
			ended[d.Turn] = d.Reason
		case session.EvAssistantMsg:
			var d session.AssistantMessageData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return PendingTurn{}, false, err
			}
			if len(d.ToolCalls) > 0 {
				if calls[d.Turn] == nil {
					calls[d.Turn] = map[int][]domain.ToolCallBlock{}
				}
				calls[d.Turn][d.Step] = d.ToolCalls
				if d.Turn == turn && d.Step > lastStep {
					lastStep = d.Step
				}
			}
		case session.EvToolResult:
			var d session.ToolResultData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return PendingTurn{}, false, err
			}
			if results[d.Turn] == nil {
				results[d.Turn] = map[string]bool{}
			}
			results[d.Turn][d.CallID] = true
		}
	}
	if turn == 0 {
		return PendingTurn{}, false, nil
	}
	reason, hasEnd := ended[turn]
	if hasEnd && reason != domain.EndWaitingApproval {
		return PendingTurn{}, false, nil
	}
	pt := PendingTurn{Turn: turn, LastStep: lastStep, OpenTurn: !hasEnd}
	for _, call := range calls[turn][lastStep] {
		if !results[turn][call.ID] {
			pt.Pending = append(pt.Pending, call)
		}
	}
	return pt, pt.OpenTurn || len(pt.Pending) > 0, nil
}

// Resume 继续一个未完成的 turn：先补跑缺失的工具调用，再从下一步继续循环。
func (a *Agent) Resume(ctx context.Context) (Outcome, error) {
	pt, ok, err := a.FindPendingTurn()
	if err != nil {
		return Outcome{}, err
	}
	if !ok {
		return Outcome{}, errors.New("agent: 没有可继续的 turn")
	}
	start := time.Now()
	if len(pt.Pending) > 0 {
		suspended, runErr := a.runTools(ctx, pt.Turn, pt.LastStep, pt.Pending)
		if runErr != nil {
			return a.finish(pt.Turn, domain.EndError, "tools: "+runErr.Error(), pt.LastStep, llm.Usage{}, start, "")
		}
		if suspended != nil {
			return a.finish(pt.Turn, domain.EndWaitingApproval, "approval:"+suspended.ID, pt.LastStep, llm.Usage{}, start, "")
		}
	}
	return a.runLoop(ctx, pt.Turn, pt.LastStep+1)
}

// runLoop 是 turn 的主循环。
func (a *Agent) runLoop(ctx context.Context, turn, fromStep int) (Outcome, error) {
	start := time.Now()
	oc := Outcome{Turn: turn}
	for _, ev := range a.store.Events() {
		if ev.Type != session.EvAssistantMsg {
			continue
		}
		var d session.AssistantMessageData
		if json.Unmarshal(ev.Data, &d) != nil || d.Turn != turn || d.Usage == nil {
			continue
		}
		oc.Usage.PromptTokens += d.Usage.PromptTokens
		oc.Usage.CompletionTokens += d.Usage.CompletionTokens
		oc.Usage.TotalTokens += d.Usage.TotalTokens
	}

	var textAll strings.Builder
	for step := fromStep; step <= a.cfg.MaxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return a.finish(turn, domain.EndAborted, "canceled: "+err.Error(), step-1, oc.Usage, start, textAll.String())
		}
		messages, err := session.Fold(a.store.Events())
		if err != nil {
			return a.finish(turn, domain.EndError, "fold: "+err.Error(), step-1, oc.Usage, start, textAll.String())
		}
		specs := a.tools.Specs()
		if _, err := a.store.Append(session.EvRequestHeader, session.RequestHeaderData{
			Turn: turn, Step: step, Provider: a.cfg.Provider, Model: a.cfg.Model,
			SystemHash:   hashString(a.systemPrompt()),
			ToolsHash:    hashJSON(specs),
			MessageCount: len(messages),
		}); err != nil {
			return Outcome{}, err
		}

		res, err := a.callModel(ctx, turn, step, messages, specs)
		if err != nil {
			code := llm.CodeOf(err)
			if errors.Is(err, context.Canceled) || code == llm.CodeCanceled {
				return a.finish(turn, domain.EndAborted, "canceled: "+err.Error(), step, oc.Usage, start, textAll.String())
			}
			if code == llm.CodeQuota {
				return a.finish(turn, domain.EndBlocked, "quota: "+err.Error(), step, oc.Usage, start, textAll.String())
			}
			return a.finish(turn, domain.EndError, fmt.Sprintf("%s: %v", code, err), step, oc.Usage, start, textAll.String())
		}
		oc.Usage.PromptTokens += res.usage.PromptTokens
		oc.Usage.CompletionTokens += res.usage.CompletionTokens
		oc.Usage.TotalTokens += res.usage.TotalTokens
		textAll.WriteString(res.text)

		if _, err := a.store.Append(session.EvAssistantMsg, session.AssistantMessageData{
			Turn: turn, Step: step, Text: res.text, Reasoning: res.reasoning,
			ToolCalls: res.calls, Finish: res.finish,
			Usage: &session.UsageData{PromptTokens: res.usage.PromptTokens, CompletionTokens: res.usage.CompletionTokens, TotalTokens: res.usage.TotalTokens},
		}); err != nil {
			return Outcome{}, err
		}

		if res.finish == domain.FinishMaxTokens {
			oc.Text = textAll.String()
			oc.Steps = step
			return a.finish(turn, domain.EndMaxTokens, "模型达到输出上限", step, oc.Usage, start, textAll.String())
		}
		if len(res.calls) == 0 {
			oc.Text = textAll.String()
			oc.Steps = step
			return a.finish(turn, domain.EndCompleted, "", step, oc.Usage, start, textAll.String())
		}
		suspended, runErr := a.runTools(ctx, turn, step, res.calls)
		if runErr != nil {
			return a.finish(turn, domain.EndError, "tools: "+runErr.Error(), step, oc.Usage, start, textAll.String())
		}
		if suspended != nil {
			oc.Text = textAll.String()
			oc.Steps = step
			return a.finish(turn, domain.EndWaitingApproval, "approval:"+suspended.ID, step, oc.Usage, start, textAll.String())
		}
	}
	oc.Text = textAll.String()
	return a.finish(turn, domain.EndBlocked, fmt.Sprintf("达到最大步数 %d", a.cfg.MaxSteps), a.cfg.MaxSteps, oc.Usage, start, textAll.String())
}

// finish 写 turn/end 并组装 Outcome。
func (a *Agent) finish(turn int, reason domain.TurnEndReason, detail string, steps int, usage llm.Usage, start time.Time, text string) (Outcome, error) {
	oc := Outcome{Turn: turn, Reason: reason, Detail: detail, Steps: steps, Usage: usage, Duration: time.Since(start), Text: text}
	if _, err := a.store.Append(session.EvTurnEnd, session.TurnEndData{
		Turn: turn, Reason: reason, Detail: detail, Steps: steps,
		Usage:    &session.UsageData{PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens, TotalTokens: usage.TotalTokens},
		Duration: oc.Duration.String(),
	}); err != nil {
		return oc, err
	}
	return oc, nil
}

type stepResult struct {
	text      string
	reasoning string
	calls     []domain.ToolCallBlock
	usage     llm.Usage
	finish    domain.FinishReason
}

// callModel 调一次模型，带重试（只重试白名单错误）。
func (a *Agent) callModel(ctx context.Context, turn, step int, messages []domain.Message, specs []llm.ToolSpec) (stepResult, error) {
	req := llm.Request{
		Model:           a.cfg.Model,
		System:          a.systemPrompt(),
		Messages:        messages,
		Tools:           specs,
		Temperature:     a.cfg.Temperature,
		MaxTokens:       a.cfg.MaxTokens,
		ReasoningEffort: a.cfg.ReasoningEffort,
	}
	var lastErr error
	for attempt := 1; attempt <= a.cfg.Retry.MaxAttempts; attempt++ {
		res, err := a.streamOnce(ctx, turn, step, req)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !llm.IsRetryable(err) || attempt == a.cfg.Retry.MaxAttempts {
			break
		}
		delay := a.cfg.Retry.Delay(attempt, err)
		a.log.Warn("llm call failed, retrying", "turn", turn, "step", step, "attempt", attempt, "code", llm.CodeOf(err), "delay", delay.String())
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return stepResult{}, ctx.Err()
		}
	}
	return stepResult{}, lastErr
}

// streamOnce 消费一次流。
func (a *Agent) streamOnce(ctx context.Context, turn, step int, req llm.Request) (stepResult, error) {
	var res stepResult
	var text, reasoning strings.Builder
	for chunk, err := range a.adapter.Stream(ctx, req) {
		if err != nil {
			return stepResult{}, err
		}
		switch chunk.Kind {
		case llm.ChunkText:
			text.WriteString(chunk.Text)
			fmt.Fprint(a.out, chunk.Text)
		case llm.ChunkReasoning:
			reasoning.WriteString(chunk.Text)
			if a.showReasoning {
				fmt.Fprint(a.out, chunk.Text)
			}
		case llm.ChunkToolCall:
			if chunk.ToolCall != nil {
				res.calls = append(res.calls, *chunk.ToolCall)
			}
		case llm.ChunkUsage:
			if chunk.Usage != nil {
				res.usage = *chunk.Usage
			}
		case llm.ChunkFinish:
			res.finish = chunk.Finish
		}
	}
	res.text = text.String()
	res.reasoning = reasoning.String()
	if res.finish == "" {
		if len(res.calls) > 0 {
			res.finish = domain.FinishToolCalls
		} else {
			res.finish = domain.FinishStop
		}
	}
	if res.text != "" || res.reasoning != "" {
		fmt.Fprintln(a.out)
	}
	return res, nil
}

// runTools 执行工具：只读且非串行的可并发，其余串行；结果按调用顺序写事件。
// 返回第一个挂起信息（非 nil 表示该 step 被挂起）。
func (a *Agent) runTools(ctx context.Context, turn, step int, calls []domain.ToolCallBlock) (*tools.Suspend, error) {
	type slot struct {
		call   domain.ToolCallBlock
		result tools.Result
	}
	slots := make([]slot, len(calls))
	var wg sync.WaitGroup
	for i, call := range calls {
		slots[i].call = call
	}
	for i := range slots {
		tool, ok := a.tools.Lookup(slots[i].call.Name)
		serial := !ok || tool.Serial || !tool.ReadOnly
		if serial {
			slots[i].result = a.tools.Execute(ctx, slots[i].call)
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			slots[i].result = a.tools.Execute(ctx, slots[i].call)
		}(i)
	}
	wg.Wait()

	var suspended *tools.Suspend
	for _, s := range slots {
		if s.result.Suspend != nil {
			// 挂起表示「没有执行」：不写 tool/result，否则 Resume 会以为已经处理过。
			// 审批单本身在审批队列里可见，turn/end 也记了 approval id。
			if suspended == nil {
				suspended = s.result.Suspend
			}
			continue
		}
		if _, err := a.store.Append(session.EvToolResult, session.ToolResultData{
			Turn: turn, Step: step, CallID: s.call.ID, Name: s.call.Name,
			Content: s.result.Content, IsError: s.result.IsError,
		}); err != nil {
			return nil, err
		}
		a.log.Debug("tool done", "tool", s.call.Name, "error", s.result.IsError, "suspended", s.result.Suspend != nil, "elapsed", s.result.Elapsed.String())
	}
	return suspended, nil
}

// systemPrompt 组合系统提示。
func (a *Agent) systemPrompt() string {
	parts := []string{a.cfg.System}
	if names := a.tools.Names(); len(names) > 0 {
		parts = append(parts, "可用工具: "+strings.Join(names, ", "))
	}
	if a.cfg.SystemExtra != "" {
		parts = append(parts, a.cfg.SystemExtra)
	}
	return strings.Join(parts, "\n\n")
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func hashJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}
