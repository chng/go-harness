package session

// EventStore 是会话存储的最小接口。
//
// 这是「后端业务系统」形态的第一道接缝：
//   - 本地 CLI 用 FileStore（events.jsonl，单写者）
//   - 服务端用 SQLStore（Postgres，事件表 + 租约）
//   - 测试用 MemoryStore
//
// 上层（agent）只依赖这个接口，不认识文件也不认识 SQL。
//
// 生产实现必须额外满足的两条语义（接口不表达，靠实现契约保证）：
//  1. 同一会话的 Append 必须是串行的（写者租约 + fencing token）；
//  2. Append 返回即表示已持久化（fsync / commit），上层可以据此重试。
type EventStore interface {
	Append(typ string, data any) (Event, error)
	AppendEvent(ev Event) (Event, error)
	Events() EventChain
	Seq() int64
	Close() error
}

// 编译期断言：文件实现满足接口。
var _ EventStore = (*Store)(nil)
