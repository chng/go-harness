// Package session 是 L1 会话存储：append-only 事件日志 + 视图折叠。
//
// 存储格式：JSONL，一行一个事件。写入策略：单写者 + O_APPEND + 立即 fsync。
// 崩溃恢复：打开时扫描最后一行，若无法解析则截断到上一个换行（残行丢弃）。
package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 事件类型（闭集，新增类型要同时更新 fold.go）。
const (
	EvTurnStart     = "turn/start"
	EvUserMessage   = "user/message"
	EvRequestHeader = "request/header"
	EvAssistantMsg  = "assistant/message"
	EvToolResult    = "tool/result"
	EvCompaction    = "compaction"
	EvTurnEnd       = "turn/end"
)

// SurfaceOp 描述视图表面操作，目前只支持 replace（压缩用）。
type SurfaceOp struct {
	Op    string `json:"op"`    // replace
	Start int64  `json:"start"` // 起始 seq
	End   int64  `json:"end"`   // 结束 seq
}

// Event 是一条会话事件。
type Event struct {
	Type      string          `json:"type"`
	Seq       int64           `json:"seq"`
	Time      time.Time       `json:"time"`
	Data      json.RawMessage `json:"data,omitempty"`
	SurfaceOp *SurfaceOp      `json:"surfaceOp,omitempty"`
}

// EventChain 表示 seq 单调递增的追加链。
type EventChain []Event

// Store 是单会话的事件存储。
type Store struct {
	mu     sync.Mutex
	dir    string
	f      *os.File
	seq    int64
	events EventChain
}

// Open 打开（或创建）会话存储。dir 不存在则创建。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "events.jsonl")
	s := &Store{dir: dir}
	if err := s.recover(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("session: open %s: %w", path, err)
	}
	s.f = f
	return s, nil
}

// recover 读取已有事件、丢弃尾部残行、恢复 seq。
func (s *Store) recover(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("session: open for recover: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var good int64 // 已确认完整的字节数
	var offset int64
	var lastSeq int64
	for sc.Scan() {
		line := sc.Bytes()
		offset += int64(len(line)) + 1
		if len(line) == 0 {
			good = offset
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			break // 残行：从 good 处截断
		}
		s.events = append(s.events, ev)
		lastSeq = ev.Seq
		good = offset
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("session: scan %s: %w", path, err)
	}
	s.seq = lastSeq
	info, statErr := os.Stat(path)
	if statErr == nil && info.Size() > good {
		if err := os.Truncate(path, good); err != nil {
			return fmt.Errorf("session: truncate tail of %s: %w", path, err)
		}
	}
	return nil
}

// Append 追加一个事件：分配 seq、写一行、fsync。返回写入后的事件。
func (s *Store) Append(typ string, data any) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.f == nil {
		return Event{}, fmt.Errorf("session: store closed")
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, fmt.Errorf("session: marshal %s: %w", typ, err)
	}
	ev := Event{Type: typ, Seq: s.seq + 1, Time: time.Now().UTC(), Data: raw}
	line, err := json.Marshal(ev)
	if err != nil {
		return Event{}, fmt.Errorf("session: marshal event: %w", err)
	}
	line = append(line, '\n')
	if _, err := s.f.Write(line); err != nil {
		return Event{}, fmt.Errorf("session: write event: %w", err)
	}
	if err := s.f.Sync(); err != nil {
		return Event{}, fmt.Errorf("session: sync: %w", err)
	}
	s.seq = ev.Seq
	s.events = append(s.events, ev)
	return ev, nil
}

// AppendEvent 追加一个「带 surfaceOp 的原始事件」（压缩用）。
func (s *Store) AppendEvent(ev Event) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return Event{}, fmt.Errorf("session: store closed")
	}
	ev.Seq = s.seq + 1
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return Event{}, fmt.Errorf("session: marshal event: %w", err)
	}
	line = append(line, '\n')
	if _, err := s.f.Write(line); err != nil {
		return Event{}, fmt.Errorf("session: write event: %w", err)
	}
	if err := s.f.Sync(); err != nil {
		return Event{}, fmt.Errorf("session: sync: %w", err)
	}
	s.seq = ev.Seq
	s.events = append(s.events, ev)
	return ev, nil
}

// Events 返回全部事件（副本）。
func (s *Store) Events() EventChain {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(EventChain, len(s.events))
	copy(out, s.events)
	return out
}

// Seq 返回最新 seq。
func (s *Store) Seq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// Dir 返回会话目录。
func (s *Store) Dir() string { return s.dir }

// Close 关闭存储。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}
