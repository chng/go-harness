package llm

import (
	"io"
	"strings"
	"testing"
)

func collect(t *testing.T, r io.Reader) []SSEvent {
	t.Helper()
	var out []SSEvent
	if err := ScanSSE(r, func(ev SSEvent) error {
		out = append(out, ev)
		return nil
	}); err != nil {
		t.Fatalf("ScanSSE: %v", err)
	}
	return out
}

func TestSSEBasic(t *testing.T) {
	evs := collect(t, strings.NewReader("data: one\n\ndata: two\n\n"))
	if len(evs) != 2 || evs[0].Data != "one" || evs[1].Data != "two" {
		t.Fatalf("got %+v", evs)
	}
}

func TestSSECRLF(t *testing.T) {
	evs := collect(t, strings.NewReader("data: a\r\n\r\ndata: b\r\n\r\n"))
	if len(evs) != 2 || evs[0].Data != "a" || evs[1].Data != "b" {
		t.Fatalf("got %+v", evs)
	}
}

func TestSSECommentsAndFields(t *testing.T) {
	in := ": heartbeat\nretry: 1000\nevent: ping\ndata: x\n\n"
	evs := collect(t, strings.NewReader(in))
	if len(evs) != 1 || evs[0].Event != "ping" || evs[0].Data != "x" {
		t.Fatalf("got %+v", evs)
	}
}

func TestSSEMultilineData(t *testing.T) {
	evs := collect(t, strings.NewReader("data: l1\ndata: l2\n\n"))
	if len(evs) != 1 || evs[0].Data != "l1\nl2" {
		t.Fatalf("got %+v", evs)
	}
}

func TestSSENoSpaceAfterColon(t *testing.T) {
	evs := collect(t, strings.NewReader("data:{\"a\":1}\n\n"))
	if len(evs) != 1 || evs[0].Data != "{\"a\":1}" {
		t.Fatalf("got %+v", evs)
	}
}

// oneByteReader 模拟分包边界：每次只给一个字节。
type oneByteReader struct {
	s string
	i int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	p[0] = r.s[r.i]
	r.i++
	return 1, nil
}

func TestSSEByteAtATime(t *testing.T) {
	in := "data: hello world\r\n\r\ndata: [DONE]\r\n\r\n"
	evs := collect(t, &oneByteReader{s: in})
	if len(evs) != 2 || evs[0].Data != "hello world" || evs[1].Data != "[DONE]" {
		t.Fatalf("got %+v", evs)
	}
}

func TestSSEFlushAtEOF(t *testing.T) {
	// 缺少结尾空行：EOF 时必须补发
	evs := collect(t, strings.NewReader("data: tail"))
	if len(evs) != 1 || evs[0].Data != "tail" {
		t.Fatalf("got %+v", evs)
	}
}

func TestAccumulatorSplitsArgs(t *testing.T) {
	acc := NewToolCallAccumulator()
	acc.Add(0, "call_1", "read", "")
	acc.Add(0, "", "", "{\"pa")
	acc.Add(0, "", "", "th\": \"a.tx")
	acc.Add(0, "", "", "t\"}")
	calls := acc.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Name != "read" || calls[0].Args != "{\"path\": \"a.txt\"}" {
		t.Fatalf("call = %+v", calls[0])
	}
}

func TestAccumulatorMultipleAndEmptyArgs(t *testing.T) {
	acc := NewToolCallAccumulator()
	acc.Add(1, "b", "write", "{\"x\":1}")
	acc.Add(0, "a", "list", "")
	calls := acc.Calls()
	if len(calls) != 2 || calls[0].ID != "b" || calls[1].ID != "a" {
		t.Fatalf("order not by index: %+v", calls)
	}
	if calls[1].Args != "{}" {
		t.Fatalf("empty args should normalize to {}, got %q", calls[1].Args)
	}
}

func TestClassifyHTTP(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   Code
	}{
		{401, "bad key", CodeAuth},
		{403, "forbidden", CodeAuth},
		{402, "payment", CodeQuota},
		{429, "slow down", CodeRateLimit},
		{400, "This model's maximum context length is 65536 tokens", CodeContextOverflow},
		{400, "invalid tools", CodeInvalidRequest},
		{500, "boom", CodeUnavailable},
		{503, "unavailable", CodeUnavailable},
	}
	for _, c := range cases {
		e := ClassifyHTTP(c.status, c.body, map[string][]string{})
		if e.Code != c.want {
			t.Errorf("status %d body %q: got %s want %s", c.status, c.body, e.Code, c.want)
		}
	}
}

func TestRetryPolicyDelay(t *testing.T) {
	p := DefaultRetryPolicy()
	if d := p.Delay(1, nil); d != 500*1000*1000 {
		t.Fatalf("delay1 = %v", d)
	}
	if d := p.Delay(2, nil); d != 1000*1000*1000 {
		t.Fatalf("delay2 = %v", d)
	}
	if d := p.Delay(10, nil); d != p.MaxDelay {
		t.Fatalf("delay10 = %v want %v", d, p.MaxDelay)
	}
	e := &Error{Code: CodeRateLimit, RetryAfter: 3 * 1000 * 1000 * 1000}
	if d := p.Delay(1, e); d != 3*1000*1000*1000 {
		t.Fatalf("retry-after not honored: %v", d)
	}
	if !IsRetryable(&Error{Code: CodeRateLimit}) || IsRetryable(&Error{Code: CodeInvalidRequest}) {
		t.Fatal("retry whitelist wrong")
	}
}
