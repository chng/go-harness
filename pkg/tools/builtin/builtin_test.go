package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhichengtrang/go-harness/pkg/llm"
	"github.com/zhichengtrang/go-harness/pkg/tools"
)

func newReg(t *testing.T) (*tools.Registry, string) {
	t.Helper()
	root := t.TempDir()
	r := tools.NewRegistry()
	if err := Register(r, Options{Root: root}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return r, root
}

func call(t *testing.T, r *tools.Registry, name, args string) tools.Result {
	t.Helper()
	return r.Execute(context.Background(), llm.ToolCall{ID: "c1", Name: name, Args: args})
}

func argsOf(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestWriteReadRoundTrip(t *testing.T) {
	r, root := newReg(t)
	res := call(t, r, "write", `{"path":"a/b.txt","content":"line1\nline2\nline3\n"}`)
	if res.IsError {
		t.Fatalf("write: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "a/b.txt")); err != nil {
		t.Fatalf("file not created: %v", err)
	}
	res = call(t, r, "read", `{"path":"a/b.txt"}`)
	if res.IsError {
		t.Fatalf("read: %+v", res)
	}
	want := "1\tline1\n2\tline2\n3\tline3\n"
	if res.Content != want {
		t.Fatalf("read = %q want %q", res.Content, want)
	}
	res = call(t, r, "read", `{"path":"a/b.txt","offset":2,"limit":1}`)
	if !strings.Contains(res.Content, "2\tline2") || strings.Contains(res.Content, "line1") {
		t.Fatalf("paging = %q", res.Content)
	}
	if !strings.Contains(res.Content, "可用 offset=3") {
		t.Fatalf("missing paging hint: %q", res.Content)
	}
}

func TestReadRejectsBinaryAndDirs(t *testing.T) {
	r, root := newReg(t)
	os.WriteFile(filepath.Join(root, "bin"), []byte{0x00, 0x01, 0x02}, 0o644)
	if res := call(t, r, "read", `{"path":"bin"}`); !res.IsError || !strings.Contains(res.Content, "二进制") {
		t.Fatalf("res = %+v", res)
	}
	os.Mkdir(filepath.Join(root, "d"), 0o755)
	if res := call(t, r, "read", `{"path":"d"}`); !res.IsError || !strings.Contains(res.Content, "目录") {
		t.Fatalf("res = %+v", res)
	}
}

func TestEditUniqueness(t *testing.T) {
	r, root := newReg(t)
	os.WriteFile(filepath.Join(root, "f.txt"), []byte("foo bar foo\n"), 0o644)
	res := call(t, r, "edit", argsOf(t, map[string]any{"path": "f.txt", "old": "foo", "new": "baz"}))
	if !res.IsError || !strings.Contains(res.Content, "2 次") {
		t.Fatalf("ambiguous edit should fail: %+v", res)
	}
	res = call(t, r, "edit", argsOf(t, map[string]any{"path": "f.txt", "old": "foo", "new": "baz", "replaceAll": true}))
	if res.IsError {
		t.Fatalf("replaceAll: %+v", res)
	}
	data, _ := os.ReadFile(filepath.Join(root, "f.txt"))
	if string(data) != "baz bar baz\n" {
		t.Fatalf("content = %q", data)
	}
	res = call(t, r, "edit", argsOf(t, map[string]any{"path": "f.txt", "old": "nope", "new": "x"}))
	if !res.IsError || !strings.Contains(res.Content, "没找到") {
		t.Fatalf("res = %+v", res)
	}
}

func TestGlobPatterns(t *testing.T) {
	r, root := newReg(t)
	os.MkdirAll(filepath.Join(root, "src/deep"), 0o755)
	os.MkdirAll(filepath.Join(root, "docs"), 0o755)
	for _, p := range []string{"src/a.go", "src/deep/b.go", "docs/c.md"} {
		os.WriteFile(filepath.Join(root, p), []byte("x"), 0o644)
	}
	res := call(t, r, "glob", argsOf(t, map[string]any{"pattern": "*.go"}))
	if res.IsError || !strings.Contains(res.Content, "src/deep/b.go") || strings.Contains(res.Content, "c.md") {
		t.Fatalf("glob *.go = %q", res.Content)
	}
	res = call(t, r, "glob", argsOf(t, map[string]any{"pattern": "**/*.go"}))
	if !strings.Contains(res.Content, "src/a.go") || !strings.Contains(res.Content, "src/deep/b.go") {
		t.Fatalf("glob **/*.go = %q", res.Content)
	}
	res = call(t, r, "glob", argsOf(t, map[string]any{"pattern": "*.md", "path": "docs"}))
	if !strings.Contains(res.Content, "c.md") {
		t.Fatalf("scoped glob = %q", res.Content)
	}
	res = call(t, r, "glob", argsOf(t, map[string]any{"pattern": "*.zzz"}))
	if res.Content != "没有匹配的文件" {
		t.Fatalf("res = %q", res.Content)
	}
}

func TestGrepIncludeAndLineNumbers(t *testing.T) {
	r, root := newReg(t)
	os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\nfunc Foo() {}\n"), 0o644)
	os.WriteFile(filepath.Join(root, "b.md"), []byte("Foo in markdown\n"), 0o644)
	res := call(t, r, "grep", argsOf(t, map[string]any{"pattern": "Foo"}))
	if res.IsError {
		t.Fatalf("grep: %+v", res)
	}
	if !strings.Contains(res.Content, "a.go:2:") || !strings.Contains(res.Content, "b.md:1:") {
		t.Fatalf("grep = %q", res.Content)
	}
	res = call(t, r, "grep", argsOf(t, map[string]any{"pattern": "Foo", "include": "*.go"}))
	if strings.Contains(res.Content, "b.md") || !strings.Contains(res.Content, "a.go:2:") {
		t.Fatalf("include filter = %q", res.Content)
	}
	res = call(t, r, "grep", argsOf(t, map[string]any{"pattern": "zzz"}))
	if res.Content != "没有匹配" {
		t.Fatalf("res = %q", res.Content)
	}
}

func TestBashTool(t *testing.T) {
	r, _ := newReg(t)
	res := call(t, r, "bash", argsOf(t, map[string]any{"command": "echo hello; echo err 1>&2"}))
	if res.IsError {
		t.Fatalf("bash: %+v", res)
	}
	if !strings.Contains(res.Content, "hello") || !strings.Contains(res.Content, "err") {
		t.Fatalf("bash out = %q", res.Content)
	}
	res = call(t, r, "bash", argsOf(t, map[string]any{"command": "exit 3"}))
	if res.IsError || !strings.Contains(res.Content, "退出码 3") {
		t.Fatalf("non-zero exit handling = %+v", res)
	}
	res = call(t, r, "bash", argsOf(t, map[string]any{"command": "sleep 5", "timeoutSec": 1}))
	if !res.IsError || !strings.Contains(res.Content, "超时") {
		t.Fatalf("timeout = %+v", res)
	}
}

func TestSandboxRejectsEscape(t *testing.T) {
	r, root := newReg(t)
	res := call(t, r, "read", `{"path":"../../../../etc/passwd"}`)
	if !res.IsError || !strings.Contains(res.Content, "超出工作区") {
		t.Fatalf("escape not blocked: %+v", res)
	}
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip("symlink unsupported")
	}
	res = call(t, r, "read", `{"path":"link/secret"}`)
	if !res.IsError {
		t.Fatalf("symlink escape not blocked: %+v", res)
	}
	res = call(t, r, "write", `{"path":"ok.txt","content":"ok"}`)
	if res.IsError {
		t.Fatalf("in-workspace write blocked: %+v", res)
	}
}
