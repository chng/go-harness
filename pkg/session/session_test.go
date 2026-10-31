package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhichengtrang/go-harness/pkg/domain"
)

func TestAppendAndFold(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Append(EvTurnStart, TurnStartData{Turn: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(EvUserMessage, UserMessageData{Text: "hi", Turn: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(EvAssistantMsg, AssistantMessageData{
		Turn: 1, Step: 1, Text: "hello",
		ToolCalls: []domain.ToolCallBlock{{ID: "c1", Name: "read", Args: "{}"}},
		Finish:    domain.FinishToolCalls,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(EvToolResult, ToolResultData{Turn: 1, Step: 1, CallID: "c1", Name: "read", Content: "ok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(EvTurnEnd, TurnEndData{Turn: 1, Reason: domain.EndCompleted, Steps: 1}); err != nil {
		t.Fatal(err)
	}
	if got := s.Seq(); got != 5 {
		t.Fatalf("seq = %d, want 5", got)
	}
	msgs, err := Fold(s.Events())
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("msgs = %d, want 3", len(msgs))
	}
	if msgs[0].Role != domain.RoleUser || msgs[0].Text() != "hi" {
		t.Fatalf("msg0 = %+v", msgs[0])
	}
	if msgs[1].Role != domain.RoleAssistant || len(msgs[1].ToolCalls()) != 1 {
		t.Fatalf("msg1 = %+v", msgs[1])
	}
	if msgs[2].Role != domain.RoleTool {
		t.Fatalf("msg2 = %+v", msgs[2])
	}
	s.Close()

	// 重开后 seq 与事件都要恢复
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if s2.Seq() != 5 {
		t.Fatalf("recovered seq = %d, want 5", s2.Seq())
	}
}

func TestRecoverTruncatesPartialTail(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Append(EvUserMessage, UserMessageData{Text: "kept", Turn: 1})
	s.Close()

	path := filepath.Join(dir, "events.jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟崩溃：写入半行
	if _, err := f.WriteString("{\"type\":\"user/message\",\"seq\":2,\"da"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	defer s2.Close()
	if s2.Seq() != 1 {
		t.Fatalf("seq after recover = %d, want 1", s2.Seq())
	}
	msgs, err := Fold(s2.Events())
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Text() != "kept" {
		t.Fatalf("msgs = %+v", msgs)
	}
	// 尾部残行应被物理截断
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "\n") != 1 {
		t.Fatalf("file still has partial tail: %q", string(raw))
	}
	// 截断后还能继续追加
	if _, err := s2.Append(EvUserMessage, UserMessageData{Text: "after", Turn: 1}); err != nil {
		t.Fatalf("append after recover: %v", err)
	}
	if s2.Seq() != 2 {
		t.Fatalf("seq = %d, want 2", s2.Seq())
	}
}

func TestFoldAppliesSurfaceReplace(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	defer s.Close()
	s.Append(EvUserMessage, UserMessageData{Text: "old1", Turn: 1})
	s.Append(EvAssistantMsg, AssistantMessageData{Turn: 1, Step: 1, Text: "old2"})
	if _, err := s.AppendEvent(Event{
		Type:      EvCompaction,
		SurfaceOp: &SurfaceOp{Op: "replace", Start: 1, End: 2},
		Data:      mustJSON(t, CompactionData{Summary: "早前聊过 old1/old2", ReplacedStart: 1, ReplacedEnd: 2}),
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	s.Append(EvUserMessage, UserMessageData{Text: "new", Turn: 2})

	msgs, err := Fold(s.Events())
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("msgs = %d, want 2 (%+v)", len(msgs), msgs)
	}
	if msgs[1].Text() != "new" {
		t.Fatalf("last msg = %q", msgs[1].Text())
	}
	found := false
	for _, m := range msgs {
		if strings.Contains(m.Text(), "已压缩的历史摘要") {
			found = true
		}
	}
	if !found {
		t.Fatal("compaction summary not injected into view")
	}
}
