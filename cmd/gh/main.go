// Command gh 是 Go Harness 的 CLI 入口。
//
// 用法：
//
//	gh run "帮我看一下这个目录里有什么"
//	gh replay ~/.go-harness/sessions/20261030-131500
//	gh tools
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zhichengtrang/go-harness/pkg/agent"
	"github.com/zhichengtrang/go-harness/pkg/domain"
	"github.com/zhichengtrang/go-harness/pkg/kernel"
	"github.com/zhichengtrang/go-harness/pkg/llm"
	"github.com/zhichengtrang/go-harness/pkg/llm/adapters/openai"
	"github.com/zhichengtrang/go-harness/pkg/session"
	"github.com/zhichengtrang/go-harness/pkg/tools"
	"github.com/zhichengtrang/go-harness/pkg/tools/builtin"
)

const version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "replay":
		return cmdReplay(args[1:])
	case "tools":
		return cmdTools(args[1:])
	case "version", "-v", "--version":
		fmt.Println("gh", version)
		return nil
	case "help", "-h", "--help":
		return usage()
	}
	// 允许省略 run：gh "prompt"
	return cmdRun(args)
}

func usage() error {
	fmt.Fprint(os.Stderr, `gh —— Go Harness CLI

用法:
  gh run "你的任务"        跑一次任务（默认）
  gh replay <会话目录>     回放会话事件
  gh tools                列出可用工具
  gh version              版本

常用参数:
  --model string           模型名（默认 $GH_MODEL 或 deepseek-chat）
  --base-url string        OpenAI 兼容端点（默认 $GH_BASE_URL 或 https://api.deepseek.com/v1）
  --api-key-env string     从哪个环境变量取 key（默认依次试 GH_API_KEY / DEEPSEEK_API_KEY / OPENAI_API_KEY）
  --max-steps int          最多步数（默认 12）
  --show-reasoning         打印推理内容
  --workdir string         工具工作区根目录（默认当前目录）
  --session string         会话目录（默认 ~/.go-harness/sessions/<时间戳>）
  --debug                  打开调试日志
`)
	return errors.New("缺少子命令")
}

func envOr(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func defaultSessionDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".go-harness", "sessions", time.Now().Format("20060102-150405"))
}

func cmdTools(args []string) error {
	fs := flag.NewFlagSet("tools", flag.ContinueOnError)
	workdir := fs.String("workdir", "", "工作区根目录")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root := *workdir
	if root == "" {
		root, _ = os.Getwd()
	}
	reg := tools.NewRegistry()
	if err := builtin.Register(reg, builtin.Options{Root: root}); err != nil {
		return err
	}
	for _, spec := range reg.Specs() {
		tool, _ := reg.Lookup(spec.Name)
		kind := "写"
		if tool.ReadOnly {
			kind = "读"
		}
		fmt.Printf("%-8s [%s] %s\n", spec.Name, kind, spec.Description)
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, spec.Schema, "  ", "  "); err == nil {
			fmt.Println("  schema:", strings.ReplaceAll(pretty.String(), "\n", "\n  "))
		}
	}
	return nil
}

func cmdReplay(args []string) error {
	if len(args) == 0 {
		return errors.New("用法: gh replay <会话目录>")
	}
	dir := args[0]
	if strings.HasPrefix(dir, "~") {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}
	store, err := session.Open(dir)
	if err != nil {
		return err
	}
	defer store.Close()
	events := store.Events()
	if len(events) == 0 {
		return fmt.Errorf("%s 中没有事件", dir)
	}
	fmt.Printf("会话 %s：%d 个事件\n\n", dir, len(events))
	for _, ev := range events {
		line := fmt.Sprintf("%3d %-18s", ev.Seq, ev.Type)
		switch ev.Type {
		case session.EvUserMessage:
			var d session.UserMessageData
			json.Unmarshal(ev.Data, &d)
			line += " " + oneLine(d.Text, 100)
		case session.EvAssistantMsg:
			var d session.AssistantMessageData
			json.Unmarshal(ev.Data, &d)
			line += fmt.Sprintf(" step=%d finish=%s text=%q", d.Step, d.Finish, oneLine(d.Text, 60))
			if len(d.ToolCalls) > 0 {
				var names []string
				for _, tc := range d.ToolCalls {
					names = append(names, tc.Name)
				}
				line += " calls=[" + strings.Join(names, ",") + "]"
			}
		case session.EvToolResult:
			var d session.ToolResultData
			json.Unmarshal(ev.Data, &d)
			line += fmt.Sprintf(" %s(%s) err=%v %q", d.Name, d.CallID, d.IsError, oneLine(d.Content, 60))
		case session.EvTurnEnd:
			var d session.TurnEndData
			json.Unmarshal(ev.Data, &d)
			line += fmt.Sprintf(" reason=%s steps=%d %s", d.Reason, d.Steps, d.Duration)
		case session.EvRequestHeader:
			var d session.RequestHeaderData
			json.Unmarshal(ev.Data, &d)
			line += fmt.Sprintf(" %s/%s msgs=%d", d.Provider, d.Model, d.MessageCount)
		}
		fmt.Println(line)
	}
	msgs, err := session.Fold(events)
	if err != nil {
		return err
	}
	fmt.Printf("\n折叠后的对话视图：%d 条消息\n", len(msgs))
	for i, m := range msgs {
		fmt.Printf("  [%d] %-9s %s\n", i+1, m.Role, oneLine(m.Text(), 90))
	}
	return nil
}

func oneLine(s string, max int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", "⏎"), "\t", " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// splitFlags 把 flag 与位置参数分开，使 gh run "prompt" --model x 与
// gh run --model x "prompt" 两种写法都能用（标准 flag 包遇到位置参数就停止解析）。
func splitFlags(args []string) (flags, positional []string) {
	boolFlags := map[string]bool{
		"show-reasoning": true, "debug": true, "dry-run": true, "help": true, "h": true,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(a) > 1 && a[0] == '-' {
			name := a
			if strings.HasPrefix(name, "--") {
				name = name[2:]
			} else {
				name = name[1:]
			}
			if idx := strings.IndexByte(name, '='); idx >= 0 {
				flags = append(flags, a)
				continue
			}
			flags = append(flags, a)
			if boolFlags[name] {
				continue
			}
			if i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		positional = append(positional, a)
	}
	return flags, positional
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	model := fs.String("model", envOr("GH_MODEL"), "模型名")
	baseURL := fs.String("base-url", envOr("GH_BASE_URL"), "OpenAI 兼容端点")
	keyEnv := fs.String("api-key-env", "", "密钥所在环境变量名")
	system := fs.String("system", "", "额外的系统提示")
	maxSteps := fs.Int("max-steps", 12, "最大步数")
	maxTokens := fs.Int("max-tokens", 0, "单次输出 token 上限")
	effort := fs.String("reasoning-effort", envOr("GH_REASONING_EFFORT"), "推理强度（供应商支持时）")
	temperature := fs.Float64("temperature", -1, "温度，负数表示用默认")
	showReasoning := fs.Bool("show-reasoning", false, "打印推理内容")
	workdir := fs.String("workdir", "", "工具工作区根目录")
	sessionDir := fs.String("session", "", "会话目录")
	debug := fs.Bool("debug", false, "调试日志")
	dryRun := fs.Bool("dry-run", false, "不调用模型，只检查配置")
	flagArgs, positional := splitFlags(args)
	if err := fs.Parse(append(flagArgs, positional...)); err != nil {
		return err
	}
	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		return errors.New("请给出任务描述，例如: gh run \"列出当前目录的文件\"")
	}

	level := slog.LevelWarn
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	root := *workdir
	if root == "" {
		root, _ = os.Getwd()
	}
	root, _ = filepath.Abs(root)

	apiKey := ""
	if *keyEnv != "" {
		apiKey = os.Getenv(*keyEnv)
		if apiKey == "" {
			return fmt.Errorf("环境变量 %s 为空", *keyEnv)
		}
	} else {
		apiKey = envOr("GH_API_KEY", "DEEPSEEK_API_KEY", "OPENAI_API_KEY")
	}
	if *model == "" {
		*model = "deepseek-chat"
	}
	if *baseURL == "" {
		*baseURL = "https://api.deepseek.com/v1"
	}

	if *dryRun {
		fmt.Printf("配置检查通过\n  provider=openai\n  baseURL=%s\n  model=%s\n  workdir=%s\n  apiKey=%s\n  maxSteps=%d\n",
			*baseURL, *model, root, maskKey(apiKey), *maxSteps)
		return nil
	}
	if apiKey == "" {
		return errors.New("没有找到 API key：请设置 GH_API_KEY / DEEPSEEK_API_KEY / OPENAI_API_KEY，或用 --api-key-env 指定")
	}

	dir := *sessionDir
	if dir == "" {
		dir = defaultSessionDir()
	}
	store, err := session.Open(dir)
	if err != nil {
		return err
	}

	// ---- L0 内核：把各层能力装成插件 ----
	k := kernel.New(logger)
	reg := tools.NewRegistry()
	adapter := openai.New(openai.Config{ProviderName: "openai-compat", BaseURL: *baseURL, APIKey: apiKey})
	var ag *agent.Agent

	err = k.Use(
		kernel.Plugin{
			Name: "session-store", Provides: []string{"session.store"},
			Apply: func(c *kernel.Context) error { c.Provide("session.store", store); return nil },
			Close: func() error { return store.Close() },
		},
		kernel.Plugin{
			Name: "tools-builtin", Provides: []string{"tools.registry"}, Requires: []string{"session.store"},
			Apply: func(c *kernel.Context) error {
				if err := builtin.Register(reg, builtin.Options{Root: root}); err != nil {
					return err
				}
				c.Provide("tools.registry", reg)
				return nil
			},
		},
		kernel.Plugin{
			Name: "llm-openai", Provides: []string{"llm.adapter"}, Requires: []string{"tools.registry"},
			Apply: func(c *kernel.Context) error { c.Provide("llm.adapter", llm.Adapter(adapter)); return nil },
		},
		kernel.Plugin{
			Name: "agent", Provides: []string{"agent"}, Requires: []string{"llm.adapter", "tools.registry", "session.store"},
			Apply: func(c *kernel.Context) error {
				cfg := agent.Config{
					Provider: adapter.Provider(), Model: *model, System: baseSystem, SystemExtra: *system,
					MaxSteps: *maxSteps, MaxTokens: *maxTokens, ReasoningEffort: *effort,
				}
				if *temperature >= 0 {
					cfg.Temperature = temperature
				}
				ag = agent.New(adapter, store, reg, cfg, logger, os.Stdout)
				ag.ShowReasoning(*showReasoning)
				c.Provide("agent", ag)
				return nil
			},
		},
	)
	if err != nil {
		store.Close()
		return err
	}
	defer k.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stderr, "→ %s · 会话 %s\n", *model, dir)
	oc, err := ag.Run(ctx, prompt)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\n← %s · %d 步 · %d tokens(prompt %d / completion %d) · %s\n",
		oc.Reason, oc.Steps, oc.Usage.TotalTokens, oc.Usage.PromptTokens, oc.Usage.CompletionTokens, oc.Duration.Round(time.Millisecond))
	if oc.Detail != "" {
		fmt.Fprintf(os.Stderr, "  说明: %s\n", oc.Detail)
	}
	if oc.Reason != domain.EndCompleted {
		return fmt.Errorf("turn 以 %s 结束", oc.Reason)
	}
	return nil
}

func maskKey(k string) string {
	if k == "" {
		return "(未设置)"
	}
	if len(k) <= 8 {
		return "***"
	}
	return k[:4] + "..." + k[len(k)-4:]
}

const baseSystem = `你是 Go Harness 的编码助手，运行在用户本机。
工具调用规则：
- 修改文件前先 read 确认内容；edit 要求 old 文本唯一匹配。
- 需要一次性执行多条命令时用 bash，但不要用它做本可以用 read/glob/grep 完成的事。
- 不确定的事实不要猜，先用工具查证。
- 最终回答用中文，简洁说明结论与依据。`
