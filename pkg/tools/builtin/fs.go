// Package builtin 提供第一批内置工具：文件与进程。
package builtin

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/zhichengtrang/go-harness/pkg/tools"
)

// Options 是内置工具的配置。
type Options struct {
	Root string // 工作区根目录；空表示不限制
}

// resolve 把用户给的相对路径解析到工作区内，并阻止逃逸。
func resolve(root, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("路径不能为空")
	}
	abs := p
	if !filepath.IsAbs(abs) {
		if root == "" {
			var err error
			abs, err = filepath.Abs(p)
			if err != nil {
				return "", err
			}
		} else {
			abs = filepath.Join(root, p)
		}
	}
	abs = filepath.Clean(abs)
	if root == "" {
		return abs, nil
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = filepath.Clean(root)
	}
	real, err := evalExisting(abs)
	if err != nil {
		return "", err
	}
	if real != realRoot && !strings.HasPrefix(real, realRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("路径 %q 超出工作区 %q（sandbox 拒绝）", p, realRoot)
	}
	return abs, nil
}

// evalExisting 解析路径中已存在的部分（防止用符号链接逃逸）。
func evalExisting(abs string) (string, error) {
	cur := abs
	var rest []string
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				real = filepath.Join(real, rest[i])
			}
			return filepath.Clean(real), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Clean(abs), nil
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

// ReadArgs 是 read 工具参数。
type ReadArgs struct {
	Path   string `json:"path" jsonschema:"文件路径（相对工作区或绝对）"`
	Offset int    `json:"offset,omitempty" jsonschema:"起始行号（1 基）"`
	Limit  int    `json:"limit,omitempty" jsonschema:"最多读取行数，默认 2000"`
}

// Read 读取文本文件并带行号返回。
func Read(opt Options) tools.Tool {
	t := tools.Define("read", "读取 UTF-8 文本文件，返回带行号的内容。支持 offset/limit 分页。",
		func(ctx context.Context, a ReadArgs) (string, error) {
			path, err := resolve(opt.Root, a.Path)
			if err != nil {
				return "", err
			}
			f, err := os.Open(path)
			if err != nil {
				return "", err
			}
			defer f.Close()
			info, err := f.Stat()
			if err != nil {
				return "", err
			}
			if info.IsDir() {
				return "", fmt.Errorf("%s 是目录，请用 glob 列出内容", a.Path)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			if isBinary(data) {
				return "", fmt.Errorf("%s 看起来是二进制文件（前 512 字节含 NUL），拒绝按文本读取", a.Path)
			}
			lines := strings.Split(string(data), "\n")
			if len(lines) > 0 && lines[len(lines)-1] == "" {
				lines = lines[:len(lines)-1]
			}
			offset := a.Offset
			if offset <= 0 {
				offset = 1
			}
			limit := a.Limit
			if limit <= 0 {
				limit = 2000
			}
			start := min(offset-1, len(lines))
			end := min(start+limit, len(lines))
			var b strings.Builder
			for i := start; i < end; i++ {
				fmt.Fprintf(&b, "%d\t%s\n", i+1, lines[i])
			}
			if end < len(lines) {
				fmt.Fprintf(&b, "... 共 %d 行，已显示 %d-%d，可用 offset=%d 继续\n", len(lines), start+1, end, end+1)
			}
			return b.String(), nil
		})
	t.ReadOnly = true
	return t
}

// WriteArgs 是 write 工具参数。
type WriteArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Write 原子写文件（临时文件 + rename），父目录自动创建。
func Write(opt Options) tools.Tool {
	t := tools.Define("write", "把内容完整写入文件（覆盖）。使用临时文件 + rename，保证不会写出半个文件。",
		func(ctx context.Context, a WriteArgs) (string, error) {
			path, err := resolve(opt.Root, a.Path)
			if err != nil {
				return "", err
			}
			if err := atomicWriteFile(path, []byte(a.Content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("已写入 %s（%d 字节）", a.Path, len(a.Content)), nil
		})
	t.Serial = true
	return t
}

// EditArgs 是 edit 工具参数。
type EditArgs struct {
	Path       string `json:"path"`
	Old        string `json:"old" jsonschema:"要被替换的字面文本"`
	New        string `json:"new" jsonschema:"替换成的文本"`
	ReplaceAll bool   `json:"replaceAll,omitempty" jsonschema:"是否替换全部匹配"`
}

// Edit 做字面文本替换（默认要求唯一匹配，避免误改）。
func Edit(opt Options) tools.Tool {
	t := tools.Define("edit", "对已有文件做字面文本替换。默认 old 必须唯一出现，否则报错。",
		func(ctx context.Context, a EditArgs) (string, error) {
			path, err := resolve(opt.Root, a.Path)
			if err != nil {
				return "", err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			if a.Old == "" {
				return "", fmt.Errorf("old 不能为空")
			}
			n := strings.Count(string(data), a.Old)
			switch {
			case n == 0:
				return "", fmt.Errorf("在 %s 中没找到要替换的文本", a.Path)
			case n > 1 && !a.ReplaceAll:
				return "", fmt.Errorf("要替换的文本在 %s 中出现 %d 次，请提供更长的上下文或设置 replaceAll", a.Path, n)
			}
			out := string(data)
			if a.ReplaceAll {
				out = strings.ReplaceAll(out, a.Old, a.New)
			} else {
				out = strings.Replace(out, a.Old, a.New, 1)
			}
			if err := atomicWriteFile(path, []byte(out), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("已替换 %d 处（%s）", n, a.Path), nil
		})
	t.Serial = true
	return t
}

// GlobArgs 是 glob 工具参数。
type GlobArgs struct {
	Pattern string `json:"pattern" jsonschema:"glob 模式；不含 / 时按文件名匹配任意深度，** 匹配任意层级"`
	Path    string `json:"path,omitempty" jsonschema:"搜索起点，默认工作区根"`
}

// Glob 按模式找文件。
func Glob(opt Options) tools.Tool {
	t := tools.Define("glob", "按 glob 模式查找文件路径（只返回文件，不返回目录）。", GlobRun(opt))
	t.ReadOnly = true
	return t
}

// GlobRun 是 glob 的实现（导出以便测试）。
func GlobRun(opt Options) func(context.Context, GlobArgs) (string, error) {
	return func(ctx context.Context, a GlobArgs) (string, error) {
		root := opt.Root
		if a.Path != "" {
			r, err := resolve(opt.Root, a.Path)
			if err != nil {
				return "", err
			}
			root = r
		}
		if root == "" {
			root = "."
		}
		re, err := globToRegexp(a.Pattern)
		if err != nil {
			return "", err
		}
		var matches []string
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if p == root {
				return nil
			}
			if d.IsDir() {
				if d.Name() == ".git" || d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			rel, relErr := filepath.Rel(root, p)
			if relErr != nil {
				return nil
			}
			target := rel
			if !strings.Contains(a.Pattern, "/") {
				target = filepath.Base(p)
			}
			if re.MatchString(filepath.ToSlash(target)) {
				matches = append(matches, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		sort.Strings(matches)
		if len(matches) == 0 {
			return "没有匹配的文件", nil
		}
		limited := matches
		extra := ""
		if len(limited) > 100 {
			limited = limited[:100]
			extra = fmt.Sprintf("\n... 共 %d 个匹配，只显示前 100 个", len(matches))
		}
		return strings.Join(limited, "\n") + extra, nil
	}
}

// GrepArgs 是 grep 工具参数。
type GrepArgs struct {
	Pattern string `json:"pattern" jsonschema:"ripgrep 风格的正则表达式"`
	Path    string `json:"path,omitempty" jsonschema:"搜索起点，默认工作区根"`
	Include string `json:"include,omitempty" jsonschema:"只搜索匹配此 glob 的文件名，例如 *.go"`
}

// Grep 在文件内容里搜索。
func Grep(opt Options) tools.Tool {
	t := tools.Define("grep", "用正则搜索文件内容，返回 文件:行号: 内容。",
		func(ctx context.Context, a GrepArgs) (string, error) {
			re, err := regexp.Compile(a.Pattern)
			if err != nil {
				return "", fmt.Errorf("正则无效: %w", err)
			}
			root := opt.Root
			if a.Path != "" {
				r, rerr := resolve(opt.Root, a.Path)
				if rerr != nil {
					return "", rerr
				}
				root = r
			}
			if root == "" {
				root = "."
			}
			var includeRe *regexp.Regexp
			if a.Include != "" {
				includeRe, err = globToRegexp(a.Include)
				if err != nil {
					return "", err
				}
			}
			const maxMatches = 250
			var b strings.Builder
			count := 0
			hitCap := false
			err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err != nil || hitCap {
					return nil
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if d.IsDir() {
					if d.Name() == ".git" || d.Name() == "node_modules" {
						return filepath.SkipDir
					}
					return nil
				}
				if includeRe != nil && !includeRe.MatchString(filepath.Base(p)) {
					return nil
				}
				data, rerr := os.ReadFile(p)
				if rerr != nil || isBinary(data) || len(data) > 4*1024*1024 {
					return nil
				}
				rel, rerr := filepath.Rel(root, p)
				if rerr != nil {
					rel = p
				}
				for i, line := range strings.Split(string(data), "\n") {
					if count >= maxMatches {
						hitCap = true
						break
					}
					if re.MatchString(line) {
						fmt.Fprintf(&b, "%s:%d: %s\n", filepath.ToSlash(rel), i+1, strings.TrimRight(line, "\r"))
						count++
					}
				}
				return nil
			})
			if err != nil {
				return "", err
			}
			if count == 0 {
				return "没有匹配", nil
			}
			if hitCap {
				fmt.Fprintf(&b, "... 命中数超过 %d，已截断\n", maxMatches)
			}
			return b.String(), nil
		})
	t.ReadOnly = true
	return t
}

func isBinary(data []byte) bool {
	n := min(len(data), 512)
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

// atomicWriteFile 用「临时文件 + rename」保证原子性。
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// globToRegexp 把 glob 转正则：** 跨层级，* 单层，? 单字符。
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '[', ']', '{', '}', '^', '$', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
