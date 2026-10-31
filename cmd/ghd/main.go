// Command ghd 是后端业务 agent 的服务端：HTTP + SSE + 异步执行 + 人工审批。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zhichengtrang/go-harness/pkg/app"
	"github.com/zhichengtrang/go-harness/pkg/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func builtinURLDefault() string { return "https://api.deepseek.com/v1" }

func run() error {
	addr := flag.String("addr", "127.0.0.1:8088", "监听地址")
	dataDir := flag.String("data-dir", "", "会话数据目录（默认 ~/.go-harness/app）")
	provider := flag.String("provider", "", "模型来源: mock | deepseek（默认：有 key 用 deepseek，否则 mock）")
	model := flag.String("model", "deepseek-chat", "模型名")
	baseURL := flag.String("base-url", builtinURLDefault(), "OpenAI 兼容端点")
	keyEnv := flag.String("api-key-env", "DEEPSEEK_API_KEY", "API key 所在环境变量")
	credFile := flag.String("cred-file", "", "可选：从这个文件里取 DEEPSEEK_API_KEY（一行 key: value 形式）")
	tenant := flag.String("tenant", "demo", "租户")
	scenario := flag.String("scenario", "aftersale", "场景")
	quotaTokens := flag.Int("quota-tokens", 300000, "每租户每日 token 预算，0 表示不限")
	maxSteps := flag.Int("max-steps", 8, "单轮最大步数")
	debug := flag.Bool("debug", false, "调试日志")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	dir := *dataDir
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".go-harness", "app")
	}

	key := os.Getenv(*keyEnv)
	if key == "" && *credFile != "" {
		var err error
		key, err = readKeyFromFile(*credFile, *keyEnv)
		if err != nil {
			return fmt.Errorf("读取 %s 失败: %w", *credFile, err)
		}
	}

	useMock := *provider == "mock" || (*provider == "" && key == "")
	if *provider == "deepseek" && key == "" {
		return fmt.Errorf("provider=deepseek 但没有找到 key：请设置 %s 或传 --cred-file", *keyEnv)
	}

	a, err := app.New(app.Config{
		DataDir: dir, Tenant: *tenant, User: "demo-user", Scenario: *scenario,
		Model: *model, MaxSteps: *maxSteps, QuotaTokens: *quotaTokens,
		UseMock: useMock, BaseURL: *baseURL, APIKey: key, Logger: logger,
	})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           server.New(a).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	fmt.Println("Go Harness 业务 Agent Demo")
	fmt.Println("  地址    :", "http://"+*addr)
	fmt.Println("  数据目录:", dir)
	if useMock {
		fmt.Println("  模型    : mock（确定性假模型，无需 API key）")
	} else {
		fmt.Println("  模型    :", *model, "@", *baseURL)
	}
	fmt.Println("  插件    :", strings.Join(a.PluginNames, ", "))
	fmt.Println("  动作    :", strings.Join(a.Cat.Names(), ", "))
	fmt.Println("  租户/场景:", *tenant+"/"+*scenario, "· 预算", fmt.Sprintf("%d tokens/天", *quotaTokens))
	fmt.Println()
	fmt.Println("打开上面的地址即可对话；涉钱动作会进入右侧审批队列。")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	fmt.Println("已退出")
	return nil
}

// readKeyFromFile 从简单的 key: value 文件里取 env 名对应的值（支持缩进）。
func readKeyFromFile(path, envName string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, envName+":") {
			return strings.TrimSpace(strings.TrimPrefix(s, envName+":")), nil
		}
	}
	return "", fmt.Errorf("文件里没有 %s", envName)
}
