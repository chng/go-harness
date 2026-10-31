package kernel

import (
	"errors"
	"strings"
	"testing"
)

func TestOrderRespectsRequires(t *testing.T) {
	var loaded []string
	mk := func(name string, provides, requires []string) Plugin {
		return Plugin{
			Name:     name,
			Provides: provides,
			Requires: requires,
			Apply: func(c *Context) error {
				loaded = append(loaded, name)
				for _, s := range provides {
					c.Provide(s, name+"-svc")
				}
				return nil
			},
		}
	}
	k := New(nil)
	// 故意乱序声明：b 依赖 a 的服务
	err := k.Use(
		mk("b", []string{"svc-b"}, []string{"svc-a"}),
		mk("a", []string{"svc-a"}, nil),
	)
	if err != nil {
		t.Fatalf("Use: %v", err)
	}
	if got := strings.Join(loaded, ","); got != "a,b" {
		t.Fatalf("load order = %q, want a,b", got)
	}
	if _, err := Get[string](k, "svc-b"); err != nil {
		t.Fatalf("Get svc-b: %v", err)
	}
}

func TestMissingRequirementFailsFast(t *testing.T) {
	k := New(nil)
	err := k.Use(Plugin{
		Name:     "x",
		Requires: []string{"nope"},
		Apply:    func(c *Context) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "nobody provides") {
		t.Fatalf("want missing-dependency error, got %v", err)
	}
}

func TestCycleDetected(t *testing.T) {
	k := New(nil)
	noop := func(c *Context) error { return nil }
	err := k.Use(
		Plugin{Name: "a", Provides: []string{"sa"}, Requires: []string{"sb"}, Apply: noop},
		Plugin{Name: "b", Provides: []string{"sb"}, Requires: []string{"sa"}, Apply: noop},
	)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want cycle error, got %v", err)
	}
}

func TestCloseIsLIFOAndReportsFirstError(t *testing.T) {
	var closed []string
	boom := errors.New("boom")
	k := New(nil)
	k.Use(
		Plugin{Name: "a", Provides: []string{"sa"}, Apply: func(c *Context) error { c.Provide("sa", 1); return nil },
			Close: func() error { closed = append(closed, "a"); return nil }},
		Plugin{Name: "b", Provides: []string{"sb"}, Requires: []string{"sa"}, Apply: func(c *Context) error { c.Provide("sb", 2); return nil },
			Close: func() error { closed = append(closed, "b"); return boom }},
	)
	err := k.Close()
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("want wrapped boom, got %v", err)
	}
	if strings.Join(closed, ",") != "b,a" {
		t.Fatalf("close order = %q, want b,a", closed)
	}
	if err := k.Close(); err != nil {
		t.Fatalf("second Close should be no-op, got %v", err)
	}
}

func TestDuplicateServicePanics(t *testing.T) {
	k := New(nil)
	k.Provide("s", 1)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("want panic on duplicate Provide")
		}
	}()
	k.Provide("s", 2)
}

func TestGetTypeMismatch(t *testing.T) {
	k := New(nil)
	k.Provide("s", 1)
	if _, err := Get[string](k, "s"); err == nil {
		t.Fatal("want type mismatch error")
	}
	if _, err := Get[int](k, "missing"); err == nil {
		t.Fatal("want not-found error")
	}
}
