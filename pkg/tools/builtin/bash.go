package builtin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/zhichengtrang/go-harness/pkg/tools"
)

// BashArgs 是 bash 工具参数。
type BashArgs struct {
	Command     string `json:"command" jsonschema:"要执行的 shell 命令（bash -lc 执行）"`
	Workdir     string `json:"workdir,omitempty" jsonschema:"工作目录，默认工作区根"`
	TimeoutSec  int    `json:"timeoutSec,omitempty" jsonschema:"超时秒数，默认 120"`
	Description string `json:"description,omitempty" jsonschema:"这条命令做什么（给自己看的说明）"`
}

// Bash 执行 shell 命令，返回退出码与合并输出。
func Bash(opt Options) tools.Tool {
	t := tools.Define("bash", "在 bash -lc 中执行命令，返回退出码与 stdout/stderr。非 0 退出码以 isError 形式返回。",
		func(ctx context.Context, a BashArgs) (string, error) {
			if strings.TrimSpace(a.Command) == "" {
				return "", fmt.Errorf("command 不能为空")
			}
			timeout := time.Duration(a.TimeoutSec) * time.Second
			if timeout <= 0 {
				timeout = 120 * time.Second
			}
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			cmd := exec.CommandContext(cctx, "bash", "-lc", a.Command)
			dir := opt.Root
			if a.Workdir != "" {
				r, err := resolve(opt.Root, a.Workdir)
				if err != nil {
					return "", err
				}
				dir = r
			}
			cmd.Dir = dir
			var buf bytes.Buffer
			cmd.Stdout = &buf
			cmd.Stderr = &buf
			err := cmd.Run()
			out := buf.String()
			if cctx.Err() == context.DeadlineExceeded {
				msg := fmt.Sprintf("命令超时（%s）", timeout)
				if out != "" {
					msg += "\n" + out
				}
				return "", errors.New(msg)
			}
			if err != nil {
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					// 不是「命令跑完但返回非 0」，而是根本没跑起来
					return "", fmt.Errorf("无法执行命令: %w", err)
				}
				// 非 0 退出码是正常结果的一部分：把码和输出一起交给模型
				if out == "" {
					return fmt.Sprintf("退出码 %d，无输出", ee.ExitCode()), nil
				}
				return fmt.Sprintf("退出码 %d\n%s", ee.ExitCode(), out), nil
			}
			if out == "" {
				return "（无输出，退出码 0）", nil
			}
			return out, nil
		})
	t.Serial = true
	return t
}

// Register 注册第一批内置工具。
func Register(r *tools.Registry, opt Options) error {
	for _, t := range []tools.Tool{
		Read(opt), Write(opt), Edit(opt), Glob(opt), Grep(opt), Bash(opt),
	} {
		if err := r.Register(t); err != nil {
			return err
		}
	}
	return nil
}
