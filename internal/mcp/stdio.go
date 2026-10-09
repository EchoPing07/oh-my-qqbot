package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file mirrors 参考/MaiBot/src/mcp_module/stdio_filter.py: MCP stdio 协议
// 规定 stdout 仅可承载 JSON-RPC 消息,但部分第三方服务器会把启动横幅打印到
// stdout 污染协议流。tolerantStdioTransport 与 go-sdk 的 CommandTransport 行为
// 一致,唯一差异是在解析层先按 JSON 起始符做廉价预筛,解析失败/非 JSON 的行只
// 记录告警后丢弃,不注入错误(合规服务器行为不变)。

// maxGarbagePreview mirrors stdio_filter._MAX_GARBAGE_PREVIEW。
const maxGarbagePreview = 200

// defaultTerminateDuration mirrors go-sdk 的 defaultTerminateDuration。
const defaultTerminateDuration = 5 * time.Second

// tolerantStdioTransport 是 stdio 传输的容错包装(对齐 tolerant_stdio_client)。
type tolerantStdioTransport struct {
	// Command 是待启动的子进程；StdoutPipe/StdinPipe 由本传输接管。
	Command *exec.Cmd
	// Stderr 接收子进程 stderr；nil 表示按行转发到 Logger。
	Stderr io.Writer
	// Logger 记录被丢弃的噪声行；nil 时使用默认 logger。
	Logger *slog.Logger
	// TerminateDuration 对齐 go-sdk CommandTransport.TerminateDuration。
	TerminateDuration time.Duration
}

// Connect implements mcp.Transport：启动子进程，并把「清洗后的 stdout +
// stdin」交给 go-sdk 的 ioConn（与 CommandTransport 的实现同构）。
func (t *tolerantStdioTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	cmd := t.Command
	if cmd == nil {
		return nil, errNilCommand
	}
	logger := loggerOrDefault(t.Logger)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if cmd.Stderr == nil {
		if t.Stderr != nil {
			cmd.Stderr = t.Stderr
		} else {
			// 对齐 stdio_filter：子进程 stderr 透传到宿主日志。
			cmd.Stderr = &lineLogWriter{logger: logger.With("component", "mcp_stdio_stderr")}
		}
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	terminate := t.TerminateDuration
	if terminate <= 0 {
		terminate = defaultTerminateDuration
	}
	streams := &stdioStreams{
		cmd:               cmd,
		stdout:            stdout,
		stdin:             stdin,
		terminateDuration: terminate,
		reader:            newTolerantReader(stdout, logger),
	}
	return (&mcp.IOTransport{Reader: streams, Writer: streams}).Connect(ctx)
}

// stdioStreams 同时充当清洗后的读取流与写往子进程 stdin 的写入流。
type stdioStreams struct {
	cmd               *exec.Cmd
	stdout            io.ReadCloser
	stdin             io.WriteCloser
	reader            *tolerantReader
	terminateDuration time.Duration
	closeOnce         sync.Once
	closeErr          error
}

func (s *stdioStreams) Read(p []byte) (int, error) { return s.reader.Read(p) }

func (s *stdioStreams) Write(p []byte) (int, error) { return s.stdin.Write(p) }

// Close mirrors go-sdk pipeRWC.Close + stdio_filter 的收尾流程:
// 关闭 stdin → 等待进程正常退出 → 超时后强杀 → 关闭 stdout。
func (s *stdioStreams) Close() error {
	s.closeOnce.Do(func() {
		if s.stdin != nil {
			_ = s.stdin.Close()
		}
		waitDone := make(chan error, 1)
		go func() { waitDone <- s.cmd.Wait() }()
		select {
		case <-waitDone:
		case <-time.After(s.terminateDuration):
			if s.cmd.Process != nil {
				_ = s.cmd.Process.Kill()
			}
			<-waitDone
		}
		if s.stdout != nil {
			_ = s.stdout.Close()
		}
		// 子进程退出码非零不作为连接错误上报（对齐 stdio_filter 的
		// ProcessLookupError 吞掉、CommandTransport 的 "closing stdin" 语义）。
	})
	return s.closeErr
}

// tolerantReader 丢弃非 JSON-RPC 行（stdout 协议流清洗）。
type tolerantReader struct {
	logger  *slog.Logger
	scanner *bufio.Scanner
	buffer  []byte
	source  io.Reader
}

func newTolerantReader(source io.Reader, logger *slog.Logger) *tolerantReader {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	return &tolerantReader{source: source, logger: logger, scanner: scanner}
}

func (r *tolerantReader) Read(p []byte) (int, error) {
	for len(r.buffer) == 0 {
		if !r.scanner.Scan() {
			if err := r.scanner.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		line := r.scanner.Bytes()
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			continue
		}
		// JSON-RPC 2.0 消息总是 JSON 对象('{')或批量数组('[');
		// 任何其他起始字符都可以无歧义判定为协议违规噪声。
		if trimmed[0] != '{' && trimmed[0] != '[' {
			r.logger.Warn(
				"已丢弃 MCP stdio 服务器的非 JSON 输出行（stdout 只能承载 JSON-RPC）",
				"line", previewText(trimmed))
			continue
		}
		// 起始符预筛通过后仍需校验 JSON（对齐 stdio_filter 的
		// model_validate_json 分支:格式错误同样只告警丢弃）。
		if !json.Valid(line) {
			r.logger.Warn(
				"已丢弃 MCP stdio 服务器的非法 JSON-RPC 行",
				"line", previewText(trimmed))
			continue
		}
		r.buffer = append(r.buffer[:0], line...)
		r.buffer = append(r.buffer, '\n')
	}
	n := copy(p, r.buffer)
	r.buffer = r.buffer[n:]
	return n, nil
}

func previewText(text string) string {
	if len(text) <= maxGarbagePreview {
		return text
	}
	return text[:maxGarbagePreview]
}

// lineLogWriter 把子进程 stderr 按行转发到 slog。
type lineLogWriter struct {
	logger *slog.Logger
	mu     sync.Mutex
	line   []byte
}

func (w *lineLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			w.flushLocked()
			continue
		}
		w.line = append(w.line, b)
		if len(w.line) > maxGarbagePreview*8 {
			w.flushLocked()
		}
	}
	return len(p), nil
}

func (w *lineLogWriter) flushLocked() {
	text := strings.TrimRight(string(w.line), "\r")
	w.line = w.line[:0]
	if strings.TrimSpace(text) != "" {
		w.logger.Debug(text)
	}
}
