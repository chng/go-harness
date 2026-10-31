package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zhichengtrang/go-harness/pkg/llm"
)

type echoArgs struct {
	Text  string   `json:"text"`
	Count int      `json:"count,omitempty"`
	Flags []string `json:"flags,omitempty"`
}

func TestSchemaForGeneratesRequiredAndProps(t *testing.T) {
	raw, err := SchemaFor(mustType(t, echoArgs{}))
	if err != nil {
		t.Fatal(err)
	}
	var sch map[string]any
	if err := json.Unmarshal(raw, &sch); err != nil {
		t.Fatal(err)
	}
	if sch["type"] != "object" {
		t.Fatalf("type = %v", sch["type"])
	}
	props := sch["properties"].(map[string]any)
	if len(props) != 3 {
		t.Fatalf("props = %v", props)
	}
	req := sch["required"].([]any)
	if len(req) != 1 || req[0] != "text" {
		t.Fatalf("required = %v", req)
	}
	if props["count"].(map[string]any)["type"] != "integer" {
		t.Fatalf("count schema = %v", props["count"])
	}
	if props["flags"].(map[string]any)["type"] != "array" {
		t.Fatalf("flags schema = %v", props["flags"])
	}
	if sch["additionalProperties"] != false {
		t.Fatal("additionalProperties should be false")
	}
}

func TestDefineRejectsUnknownFields(t *testing.T) {
	tool := Define("echo", "echo back", func(ctx context.Context, a echoArgs) (string, error) {
		return a.Text, nil
	})
	res := execute(t, tool, `{"text":"hi","nope":1}`)
	if !res.IsError || !strings.Contains(res.Content, "参数无效") {
		t.Fatalf("res = %+v", res)
	}
	res = execute(t, tool, `{"text":"hi"}`)
	if res.IsError || res.Content != "hi" {
		t.Fatalf("res = %+v", res)
	}
}

func TestRegistryRejectsDuplicatesAndBadSchema(t *testing.T) {
	r := NewRegistry()
	ok := Define("a", "d", func(ctx context.Context, a echoArgs) (string, error) { return "", nil })
	if err := r.Register(ok); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(ok); err == nil {
		t.Fatal("want duplicate error")
	}
	if err := r.Register(Tool{Name: "b", Execute: func(context.Context, json.RawMessage) (string, error) { return "", nil }}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(Tool{Name: "c", Schema: json.RawMessage("{bad")}); err == nil {
		t.Fatal("want invalid schema error")
	}
}

func TestRegistryUnknownToolAndBadArgs(t *testing.T) {
	r := NewRegistry()
	res := r.Execute(context.Background(), llm.ToolCall{Name: "nope", Args: "{}"})
	if !res.IsError || !strings.Contains(res.Content, "unknown tool") {
		t.Fatalf("res = %+v", res)
	}
}

func TestRegistryTruncatesOutput(t *testing.T) {
	r := NewRegistry()
	long := strings.Repeat("字", 10000)
	tool := Define("big", "big output", func(ctx context.Context, a echoArgs) (string, error) { return long, nil })
	tool.MaxOutput = 100
	r.Register(tool)
	res := r.Execute(context.Background(), llm.ToolCall{Name: "big", Args: "{}"})
	if !res.Truncated {
		t.Fatal("want truncated")
	}
	if !strings.Contains(res.Content, "输出被截断") {
		t.Fatalf("res = %q", res.Content)
	}
	// 截断不能产生非法 UTF-8
	if !utf8.ValidString(res.Content) {
		t.Fatal("truncation produced invalid UTF-8")
	}
}

func TestRegistryGuardBlocks(t *testing.T) {
	r := NewRegistry()
	r.Register(Define("t", "d", func(ctx context.Context, a echoArgs) (string, error) { return "ran", nil }))
	r.UseGuard(GuardFunc(func(ctx context.Context, name string, args json.RawMessage) error {
		return errors.New("需要审批")
	}))
	res := r.Execute(context.Background(), llm.ToolCall{Name: "t", Args: "{}"})
	if !res.IsError || res.Content != "需要审批" {
		t.Fatalf("res = %+v", res)
	}
}

func TestRegistryTimeout(t *testing.T) {
	r := NewRegistry()
	tool := Define("slow", "slow", func(ctx context.Context, a echoArgs) (string, error) {
		select {
		case <-time.After(2 * time.Second):
			return "late", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	tool.Timeout = 50 * time.Millisecond
	r.Register(tool)
	start := time.Now()
	res := r.Execute(context.Background(), llm.ToolCall{Name: "slow", Args: "{}"})
	if !res.IsError || !strings.Contains(res.Content, "timed out") {
		t.Fatalf("res = %+v", res)
	}
	if time.Since(start) > time.Second {
		t.Fatal("timeout not enforced promptly")
	}
}

func TestSpecsAreStableAndSorted(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"c", "a", "b"} {
		name := n
		r.Register(Define(name, "d", func(ctx context.Context, a echoArgs) (string, error) { return "", nil }))
	}
	specs := r.Specs()
	if len(specs) != 3 || specs[0].Name != "a" || specs[2].Name != "c" {
		t.Fatalf("specs = %+v", specs)
	}
	for _, s := range specs {
		if len(s.Schema) == 0 || !json.Valid(s.Schema) {
			t.Fatalf("bad schema for %s", s.Name)
		}
	}
}

func execute(t *testing.T, tool Tool, args string) Result {
	t.Helper()
	r := NewRegistry()
	if err := r.Register(tool); err != nil {
		t.Fatal(err)
	}
	return r.Execute(context.Background(), llm.ToolCall{Name: tool.Name, Args: args, ID: "c1"})
}
