package llm

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// SSEvent 是一条 Server-Sent Event。
type SSEvent struct {
	Event string
	Data  string
	ID    string
}

// ScanSSE 解析 SSE 流。
//
// 处理要点（都是线上真实踩过的坑）：
//   - CRLF 与 LF 混合；
//   - 以 ':' 开头的注释行（心跳）直接忽略；
//   - 多行 data 用 '\n' 拼接；
//   - 空行才派发；EOF 时若还有未派发内容要补发；
//   - 分包边界不落在语义上（按行处理天然满足）。
func ScanSSE(r io.Reader, fn func(SSEvent) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var ev SSEvent
	var data []string
	flush := func() error {
		if len(data) == 0 && ev.Event == "" && ev.ID == "" {
			return nil
		}
		out := ev
		out.Data = strings.Join(data, "\n")
		data = data[:0]
		ev.Event = ""
		ev.ID = ""
		return fn(out)
	}
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			switch {
			case line == "":
				if ferr := flush(); ferr != nil {
					return ferr
				}
			case strings.HasPrefix(line, ":"):
				// 注释 / 心跳
			default:
				field, value, found := strings.Cut(line, ":")
				if found {
					value = strings.TrimPrefix(value, " ")
				}
				switch field {
				case "data":
					data = append(data, value)
				case "event":
					ev.Event = value
				case "id":
					ev.ID = value
				case "retry":
					// 忽略：重试由本地策略控制
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return flush()
			}
			return err
		}
	}
}
