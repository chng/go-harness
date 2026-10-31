package session

import (
	"encoding/json"
	"fmt"

	"github.com/zhichengtrang/go-harness/pkg/domain"
)

// TurnStartData 是 turn/start 的载荷。
type TurnStartData struct {
	Turn int    `json:"turn"`
	Text string `json:"text,omitempty"`
}

// UserMessageData 是 user/message 的载荷。
type UserMessageData struct {
	Text string `json:"text"`
	Turn int    `json:"turn"`
}

// RequestHeaderData 记录一次模型调用的请求快照（可审计、可回放）。
type RequestHeaderData struct {
	Turn         int    `json:"turn"`
	Step         int    `json:"step"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	SystemHash   string `json:"systemHash,omitempty"`
	ToolsHash    string `json:"toolsHash,omitempty"`
	MessageCount int    `json:"messageCount"`
}

// AssistantMessageData 是一条助手消息（含工具调用块）。
type AssistantMessageData struct {
	Turn      int                    `json:"turn"`
	Step      int                    `json:"step"`
	Text      string                 `json:"text,omitempty"`
	Reasoning string                 `json:"reasoning,omitempty"`
	ToolCalls []domain.ToolCallBlock `json:"toolCalls,omitempty"`
	Finish    domain.FinishReason    `json:"finish,omitempty"`
	Usage     *UsageData             `json:"usage,omitempty"`
}

// ToolResultData 是一次工具结果。
type ToolResultData struct {
	Turn      int    `json:"turn"`
	Step      int    `json:"step"`
	CallID    string `json:"callId"`
	Name      string `json:"name"`
	Content   string `json:"content"`
	IsError   bool   `json:"isError,omitempty"`
	Suspended bool   `json:"suspended,omitempty"` // 该调用被挂起（等待审批）
}

// TurnEndData 是 turn 终态。
type TurnEndData struct {
	Turn     int                  `json:"turn"`
	Reason   domain.TurnEndReason `json:"reason"`
	Detail   string               `json:"detail,omitempty"`
	Steps    int                  `json:"steps"`
	Usage    *UsageData           `json:"usage,omitempty"`
	Duration string               `json:"duration,omitempty"`
}

// UsageData 是 token 用量。
type UsageData struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	TotalTokens      int `json:"totalTokens"`
}

// CompactionData 是一次压缩。
type CompactionData struct {
	Summary       string `json:"summary"`
	ReplacedStart int64  `json:"replacedStart"`
	ReplacedEnd   int64  `json:"replacedEnd"`
}

// Fold 把事件链折叠成消息列表（视图）。surfaceOp replace 会替换指定区间。
func Fold(events EventChain) ([]domain.Message, error) {
	// 先应用 surface 操作：被完全替换掉的事件不再参与折叠
	dropped := make(map[int64]bool)
	summaries := make(map[int64]string)
	for _, ev := range events {
		if ev.SurfaceOp == nil || ev.SurfaceOp.Op != "replace" {
			continue
		}
		for seq := ev.SurfaceOp.Start; seq <= ev.SurfaceOp.End; seq++ {
			dropped[seq] = true
		}
		var cd CompactionData
		if len(ev.Data) > 0 {
			if err := json.Unmarshal(ev.Data, &cd); err != nil {
				return nil, fmt.Errorf("session: compaction payload: %w", err)
			}
		}
		if cd.Summary != "" {
			summaries[ev.Seq] = cd.Summary
		}
	}

	var out []domain.Message
	for _, ev := range events {
		if dropped[ev.Seq] {
			continue
		}
		switch ev.Type {
		case EvUserMessage:
			var d UserMessageData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return nil, fmt.Errorf("session: user/message seq %d: %w", ev.Seq, err)
			}
			out = append(out, domain.Message{
				Role: domain.RoleUser, Blocks: []domain.Block{domain.TextBlock{Text: d.Text}},
				Time: ev.Time, Turn: d.Turn,
			})
		case EvAssistantMsg:
			var d AssistantMessageData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return nil, fmt.Errorf("session: assistant/message seq %d: %w", ev.Seq, err)
			}
			var blocks []domain.Block
			if d.Reasoning != "" {
				blocks = append(blocks, domain.ReasoningBlock{Text: d.Reasoning})
			}
			if d.Text != "" {
				blocks = append(blocks, domain.TextBlock{Text: d.Text})
			}
			for _, tc := range d.ToolCalls {
				blocks = append(blocks, tc)
			}
			out = append(out, domain.Message{
				Role: domain.RoleAssistant, Blocks: blocks, Time: ev.Time, Turn: d.Turn, Step: d.Step,
			})
		case EvToolResult:
			var d ToolResultData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return nil, fmt.Errorf("session: tool/result seq %d: %w", ev.Seq, err)
			}
			out = append(out, domain.Message{
				Role: domain.RoleTool,
				Blocks: []domain.Block{domain.ToolResultBlock{
					CallID: d.CallID, Name: d.Name, Content: d.Content, IsError: d.IsError,
				}},
				Time: ev.Time, Turn: d.Turn, Step: d.Step,
			})
		case EvCompaction:
			// 摘要放在被替换区间的原位，而不是追加到末尾
			if s, ok := summaries[ev.Seq]; ok {
				out = append(out, domain.Message{
					Role:   domain.RoleSystem,
					Blocks: []domain.Block{domain.TextBlock{Text: "[已压缩的历史摘要] " + s}},
					Time:   ev.Time,
				})
			}
		case EvTurnStart, EvRequestHeader, EvTurnEnd:
			// 元数据事件不进对话视图
		default:
			// 未知事件类型：忽略但可审计（保持前向兼容）
		}
	}
	return out, nil
}
