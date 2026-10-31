package session

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// MemoryStore 是纯内存实现，用于测试与无数据库试跑。
//
// 注意：它不是生产实现——没有持久化、没有跨进程串行化。
type MemoryStore struct {
	mu     sync.Mutex
	events EventChain
	seq    int64
	closed bool
}

// NewMemoryStore 创建内存存储。
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

func (m *MemoryStore) append(ev Event) (Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Event{}, fmt.Errorf("session: store closed")
	}
	ev.Seq = m.seq + 1
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	m.seq = ev.Seq
	m.events = append(m.events, ev)
	return ev, nil
}

// Append 追加事件。
func (m *MemoryStore) Append(typ string, data any) (Event, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, fmt.Errorf("session: marshal %s: %w", typ, err)
	}
	return m.append(Event{Type: typ, Data: raw})
}

// AppendEvent 追加原始事件（带 surfaceOp）。
func (m *MemoryStore) AppendEvent(ev Event) (Event, error) { return m.append(ev) }

// Events 返回事件副本。
func (m *MemoryStore) Events() EventChain {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(EventChain, len(m.events))
	copy(out, m.events)
	return out
}

// Seq 返回最新 seq。
func (m *MemoryStore) Seq() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seq
}

// Close 关闭存储。
func (m *MemoryStore) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

var _ EventStore = (*MemoryStore)(nil)
