package servicectl

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Action 枚举对应 proto ServiceAction。
type Action string

const (
	ActionStart   Action = "start"
	ActionStop    Action = "stop"
	ActionRestart Action = "restart"
	ActionReload  Action = "reload"
	ActionStatus  Action = "status"
	ActionEnable  Action = "enable"
	ActionDisable Action = "disable"
)

// Result 是 systemctl 操作结果。
type Result struct {
	ServiceName string
	ActiveState string // active / inactive / failed / unknown
	SubState    string // running / dead / exited / ...
	Output      string
	Error       string
	Success     bool
}

// Controller 封装 systemctl 命令，并强制白名单检查。
type Controller struct {
	allowedServices map[string]struct{}
}

// New 创建 Controller，allowedList 为允许操作的服务名称（含 .service 后缀）。
func New(allowedList []string) *Controller {
	m := make(map[string]struct{}, len(allowedList))
	for _, s := range allowedList {
		m[s] = struct{}{}
	}
	return &Controller{allowedServices: m}
}

// Execute 执行对 serviceName 的 action 操作。
func (c *Controller) Execute(serviceName string, action Action) Result {
	// ── 白名单检查 ──────────────────────────────
	if _, ok := c.allowedServices[serviceName]; !ok {
		return Result{
			ServiceName: serviceName,
			Error:       fmt.Sprintf("service %q is not in the allowed list", serviceName),
		}
	}

	// ── 执行 systemctl ──────────────────────────
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	switch action {
	case ActionStatus:
		cmd = exec.CommandContext(ctx, "systemctl", "status", "--no-pager", serviceName)
	case ActionStart, ActionStop, ActionRestart, ActionReload, ActionEnable, ActionDisable:
		// 通过 sudo 调用，权限由 /etc/sudoers 控制
		cmd = exec.CommandContext(ctx, "sudo", "systemctl", string(action), serviceName)
	default:
		return Result{
			ServiceName: serviceName,
			Error:       fmt.Sprintf("unknown action: %s", action),
		}
	}

	var outBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf

	err := cmd.Run()
	output := outBuf.String()

	result := Result{
		ServiceName: serviceName,
		Output:      output,
		Success:     err == nil,
	}
	if err != nil {
		result.Error = err.Error()
	}

	// ── 无论操作成功与否，查询最新状态 ──────────
	active, sub := queryServiceState(serviceName)
	result.ActiveState = active
	result.SubState = sub

	return result
}

// queryServiceState 查询服务的 ActiveState 和 SubState。
func queryServiceState(serviceName string) (activeState, subState string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// ActiveState
	out, err := exec.CommandContext(ctx,
		"systemctl", "show", serviceName, "--property=ActiveState", "--value").Output()
	if err == nil {
		activeState = strings.TrimSpace(string(out))
	} else {
		activeState = "unknown"
	}

	// SubState
	out, err = exec.CommandContext(ctx,
		"systemctl", "show", serviceName, "--property=SubState", "--value").Output()
	if err == nil {
		subState = strings.TrimSpace(string(out))
	} else {
		subState = "unknown"
	}

	return
}

// IsAllowed 供外部模块检查服务名是否在白名单内。
func (c *Controller) IsAllowed(serviceName string) bool {
	_, ok := c.allowedServices[serviceName]
	return ok
}
