package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zhichengtrang/go-harness/pkg/app"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	a, err := app.New(app.Config{
		DataDir: t.TempDir(), Tenant: "t1", Scenario: "aftersale",
		Model: "mock", MaxSteps: 6, UseMock: true, QuotaTokens: 100000,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	return httptest.NewServer(New(a).Handler())
}

func post(t *testing.T, url string, body any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode >= 300 {
		t.Fatalf("POST %s: status %d body %v", url, resp.StatusCode, out)
	}
	return out
}

func getJSON(t *testing.T, url string, out any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	json.NewDecoder(resp.Body).Decode(out)
	return resp.StatusCode
}

// waitFor 轮询直到条件满足。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(80 * time.Millisecond)
	}
	t.Fatalf("超时等待: %s", what)
}

type sessionView struct {
	ID          string           `json:"id"`
	Seq         int64            `json:"seq"`
	Busy        bool             `json:"busy"`
	PendingTurn bool             `json:"pendingTurn"`
	Approvals   []map[string]any `json:"approvals"`
	Events      []struct {
		Seq  int64          `json:"seq"`
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	} `json:"events"`
}

func TestRefundFlowWithApproval(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	created := post(t, ts.URL+"/api/sessions", map[string]any{"title": "退款场景"})
	id := created["id"].(string)

	post(t, fmt.Sprintf("%s/api/sessions/%s/messages", ts.URL, id), map[string]any{
		"text": "我的订单 A1001 一直没发货，我要退款",
	})

	var view sessionView
	waitFor(t, "出现待审批", func() bool {
		getJSON(t, fmt.Sprintf("%s/api/sessions/%s", ts.URL, id), &view)
		return len(view.Approvals) == 1 && !view.Busy
	})
	if view.Approvals[0]["tool"] != "order.refund" {
		t.Fatalf("approval = %+v", view.Approvals[0])
	}
	if !hasTurnEnd(view, "waiting-approval") {
		t.Fatalf("turn/end should be waiting-approval, events = %v", eventTypes(view))
	}

	// 批准 → 自动续跑
	aprID := view.Approvals[0]["id"].(string)
	post(t, fmt.Sprintf("%s/api/approvals/%s", ts.URL, aprID), map[string]any{
		"approved": true, "by": "alice", "reason": "核对无误",
	})

	waitFor(t, "续跑完成", func() bool {
		getJSON(t, fmt.Sprintf("%s/api/sessions/%s", ts.URL, id), &view)
		return hasTurnEnd(view, "completed") && len(view.Approvals) == 0
	})

	refundCount := 0
	var refundContent string
	for _, ev := range view.Events {
		if ev.Type == "tool/result" && ev.Data["name"] == "order.refund" && ev.Data["isError"] != true {
			refundCount++
			refundContent, _ = ev.Data["content"].(string)
		}
	}
	if refundCount != 1 || !strings.Contains(refundContent, "RF-A1001") {
		t.Fatalf("refund executed %d times, content = %q", refundCount, refundContent)
	}

	// 重复批准不应二次执行
	resp, err := http.Post(fmt.Sprintf("%s/api/approvals/%s", ts.URL, aprID), "application/json",
		bytes.NewReader([]byte(`{"approved":true,"by":"alice"}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second decision should conflict, got %d", resp.StatusCode)
	}
}

func TestRejectKeepsBusinessState(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	created := post(t, ts.URL+"/api/sessions", map[string]any{})
	id := created["id"].(string)
	post(t, fmt.Sprintf("%s/api/sessions/%s/messages", ts.URL, id), map[string]any{"text": "订单 A1002 我要退款"})

	var view sessionView
	waitFor(t, "待审批", func() bool {
		getJSON(t, fmt.Sprintf("%s/api/sessions/%s", ts.URL, id), &view)
		return len(view.Approvals) == 1 && !view.Busy
	})
	post(t, fmt.Sprintf("%s/api/approvals/%s", ts.URL, view.Approvals[0]["id"].(string)),
		map[string]any{"approved": false, "by": "bob", "reason": "金额不对"})

	waitFor(t, "驳回后 turn 继续跑完", func() bool {
		getJSON(t, fmt.Sprintf("%s/api/sessions/%s", ts.URL, id), &view)
		return hasTurnEnd(view, "completed")
	})
	for _, ev := range view.Events {
		if ev.Type == "tool/result" && ev.Data["name"] == "order.refund" {
			if ev.Data["isError"] != true || !strings.Contains(ev.Data["content"].(string), "驳回") {
				t.Fatalf("rejected refund should be an error result: %+v", ev.Data)
			}
		}
	}
}

func TestReadOnlyFlowNeedsNoApproval(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	created := post(t, ts.URL+"/api/sessions", map[string]any{})
	id := created["id"].(string)
	post(t, fmt.Sprintf("%s/api/sessions/%s/messages", ts.URL, id), map[string]any{"text": "SKU-1001 还有货吗？"})

	var view sessionView
	waitFor(t, "完成", func() bool {
		getJSON(t, fmt.Sprintf("%s/api/sessions/%s", ts.URL, id), &view)
		return hasTurnEnd(view, "completed")
	})
	if len(view.Approvals) != 0 {
		t.Fatalf("read-only flow should not need approval: %+v", view.Approvals)
	}
	found := false
	for _, ev := range view.Events {
		if ev.Type == "tool/result" && ev.Data["name"] == "inventory.check" {
			found = true
		}
	}
	if !found {
		t.Fatal("inventory.check not called")
	}
}

func TestSSEStreamsEventsAndDone(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	created := post(t, ts.URL+"/api/sessions", map[string]any{})
	id := created["id"].(string)
	post(t, fmt.Sprintf("%s/api/sessions/%s/messages", ts.URL, id), map[string]any{"text": "SKU-1001 有货吗"})

	resp, err := http.Get(fmt.Sprintf("%s/api/sessions/%s/stream?after=0", ts.URL, id))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 200000))
	text := string(body)
	if !strings.Contains(text, "event: user/message") || !strings.Contains(text, "event: assistant/message") {
		t.Fatalf("stream missing events:\n%s", text[:min(len(text), 800)])
	}
	if !strings.Contains(text, "event: done") {
		t.Fatalf("stream missing done:\n%s", text[max(0, len(text)-400):])
	}
}

func TestUsageAndActions(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	var actions struct {
		Plugins []string `json:"plugins"`
		Actions []struct {
			Name          string `json:"name"`
			Risk          string `json:"risk"`
			NeedsApproval bool   `json:"needsApproval"`
		} `json:"actions"`
	}
	if code := getJSON(t, ts.URL+"/api/actions", &actions); code != 200 {
		t.Fatalf("status = %d", code)
	}
	if len(actions.Plugins) != 3 {
		t.Fatalf("plugins = %v", actions.Plugins)
	}
	risks := map[string]string{}
	for _, a := range actions.Actions {
		risks[a.Name] = a.Risk
	}
	if risks["order.lookup"] != "read" || risks["order.refund"] != "money" || risks["ticket.create"] != "write-soft" {
		t.Fatalf("risks = %v", risks)
	}
	var usage map[string]any
	getJSON(t, ts.URL+"/api/usage", &usage)
	if usage["provider"] == "" {
		t.Fatalf("usage = %v", usage)
	}
}

func hasTurnEnd(v sessionView, reason string) bool {
	for _, ev := range v.Events {
		if ev.Type == "turn/end" {
			if r, _ := ev.Data["reason"].(string); r == reason {
				return true
			}
		}
	}
	return false
}

func eventTypes(v sessionView) []string {
	out := make([]string, 0, len(v.Events))
	for _, ev := range v.Events {
		out = append(out, ev.Type)
	}
	return out
}
