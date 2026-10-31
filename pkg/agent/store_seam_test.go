package agent

// 这个测试的意义：证明 agent 不认识「文件」。
// 后端换成 Postgres 实现时，这里不会产生任何改动。

import (
	"bytes"
	"context"
	"testing"

	"github.com/zhichengtrang/go-harness/pkg/domain"
	"github.com/zhichengtrang/go-harness/pkg/session"
	"github.com/zhichengtrang/go-harness/pkg/tools"
)

func TestAgentRunsOnAnyEventStore(t *testing.T) {
	stores := map[string]func() session.EventStore{
		"memory": func() session.EventStore { return session.NewMemoryStore() },
		"file": func() session.EventStore {
			s, err := session.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
	}
	for name, mk := range stores {
		t.Run(name, func(t *testing.T) {
			store := mk()
			defer store.Close()
			fa := &fakeAdapter{script: []scripted{textResponse("ok from " + name)}}
			reg := tools.NewRegistry()
			out := &bytes.Buffer{}
			ag := New(fa, store, reg, Config{Provider: "fake", Model: "m"}, nil, out)
			oc, err := ag.Run(context.Background(), "hi")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if oc.Reason != domain.EndCompleted || oc.Text != "ok from "+name {
				t.Fatalf("outcome = %+v", oc)
			}
			if store.Seq() == 0 {
				t.Fatal("no events written")
			}
			msgs, err := session.Fold(store.Events())
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 2 {
				t.Fatalf("msgs = %d", len(msgs))
			}
		})
	}
}
