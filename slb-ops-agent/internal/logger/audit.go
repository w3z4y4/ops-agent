package logger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditEntry 是单条审计记录。
type AuditEntry struct {
	Timestamp   time.Time         `json:"timestamp"`
	CallerIP    string            `json:"caller_ip"`
	Method      string            `json:"method"`      // gRPC 方法名
	Params      map[string]any    `json:"params"`      // 关键入参摘要
	ExitCode    int               `json:"exit_code"`   // 命令退出码，非命令请求设为 -1
	Result      string            `json:"result"`      // "success" / "error"
	ErrorDetail string            `json:"error,omitempty"`
	DurationMS  int64             `json:"duration_ms"`
}

// AuditLogger 异步写本地审计日志。
type AuditLogger struct {
	mu      sync.Mutex
	file    *os.File
	encoder *json.Encoder
	queue   chan AuditEntry
	wg      sync.WaitGroup
	done    chan struct{}
}

// NewAuditLogger 创建并启动审计日志后台 goroutine。
func NewAuditLogger(path string) (*AuditLogger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create audit log dir: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}

	al := &AuditLogger{
		file:    f,
		encoder: json.NewEncoder(f),
		queue:   make(chan AuditEntry, 1024),
		done:    make(chan struct{}),
	}
	al.encoder.SetEscapeHTML(false)

	al.wg.Add(1)
	go al.drain()

	return al, nil
}

// Log 将审计记录投入异步队列（非阻塞，队满则丢弃并打印警告）。
func (a *AuditLogger) Log(entry AuditEntry) {
	select {
	case a.queue <- entry:
	default:
		// 队列满，打印警告但不阻塞调用方
		fmt.Fprintf(os.Stderr, "[audit] queue full, dropping entry: %s\n", entry.Method)
	}
}

// Close 等待队列清空后关闭文件。
func (a *AuditLogger) Close() {
	close(a.done)
	a.wg.Wait()
	a.file.Close()
}

func (a *AuditLogger) drain() {
	defer a.wg.Done()
	for {
		select {
		case entry := <-a.queue:
			a.write(entry)
		case <-a.done:
			// 清空剩余队列后退出
			for {
				select {
				case entry := <-a.queue:
					a.write(entry)
				default:
					return
				}
			}
		}
	}
}

func (a *AuditLogger) write(entry AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.encoder.Encode(entry); err != nil {
		fmt.Fprintf(os.Stderr, "[audit] write error: %v\n", err)
	}
}
