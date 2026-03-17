package executor

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/google/uuid"
)

const defaultTimeout = 30 * time.Second

// ─────────────────────────────────────────────
//  数据结构
// ─────────────────────────────────────────────

// Request 描述一次命令执行请求。
type Request struct {
	Command    string
	Args       []string
	TimeoutSec int
	Env        map[string]string
}

// Result 是命令执行结果。
type Result struct {
	TaskID     string
	Output     string // stdout + stderr 合并
	ExitCode   int
	Error      string
	DurationMS int64
}

// ─────────────────────────────────────────────
//  Executor
// ─────────────────────────────────────────────

// Executor 负责同步/异步执行系统命令。
type Executor struct {
	mu    sync.RWMutex
	tasks map[string]*asyncTask // taskID → result（完成后存储）
}

type asyncTask struct {
	done   chan struct{}
	result Result
}

// New 创建 Executor 实例。
func New() *Executor {
	return &Executor{
		tasks: make(map[string]*asyncTask),
	}
}

// Execute 同步执行命令，阻塞直到命令完成或超时。
func (e *Executor) Execute(req Request) Result {
	return e.run(uuid.New().String(), req)
}

// ExecuteAsync 异步执行命令，立即返回 taskID，
// 调用方可通过 GetResult 轮询结果。
func (e *Executor) ExecuteAsync(req Request) string {
	taskID := uuid.New().String()
	task := &asyncTask{done: make(chan struct{})}

	e.mu.Lock()
	e.tasks[taskID] = task
	e.mu.Unlock()

	go func() {
		result := e.run(taskID, req)
		task.result = result
		close(task.done)
	}()

	return taskID
}

// GetResult 返回异步任务结果。
// 若任务未完成，ok=false；若 taskID 不存在，返回错误 Result。
func (e *Executor) GetResult(taskID string) (Result, bool) {
	e.mu.RLock()
	task, exists := e.tasks[taskID]
	e.mu.RUnlock()

	if !exists {
		return Result{
			TaskID: taskID,
			Error:  "task not found",
		}, false
	}

	select {
	case <-task.done:
		// 任务完成后从 map 中清除（防内存泄漏）
		e.mu.Lock()
		delete(e.tasks, taskID)
		e.mu.Unlock()
		return task.result, true
	default:
		return Result{TaskID: taskID}, false
	}
}

// ─────────────────────────────────────────────
//  核心执行逻辑
// ─────────────────────────────────────────────

func (e *Executor) run(taskID string, req Request) Result {
	start := time.Now()

	timeout := defaultTimeout
	if req.TimeoutSec > 0 {
		timeout = time.Duration(req.TimeoutSec) * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, req.Command, req.Args...)

	// 合并 stdout 和 stderr
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	// 注入额外环境变量（继承当前进程环境）
	if len(req.Env) > 0 {
		env := cmd.Environ()
		for k, v := range req.Env {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
		cmd.Env = env
	}

	err := cmd.Run()
	duration := time.Since(start).Milliseconds()

	result := Result{
		TaskID:     taskID,
		Output:     buf.String(),
		ExitCode:   0,
		DurationMS: duration,
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else if ctx.Err() == context.DeadlineExceeded {
			result.ExitCode = -1
			result.Error = fmt.Sprintf("command timed out after %v", timeout)
		} else {
			result.ExitCode = -1
			result.Error = err.Error()
		}
	}

	return result
}
