package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/yourorg/slb-ops-agent/internal/config"
)

// State 表示升级状态机的当前状态。
type State int32

const (
	StateIdle       State = iota // 无升级进行
	StateDownloading             // 正在下载新版本
	StateValidating              // 等待心跳握手验证
	StateCommitted               // 升级成功已提交
	StateRolledBack              // 已回滚
)

// Manager 负责 Agent 自身的平滑升级与 A/B 回滚。
type Manager struct {
	cfg     *config.AgentConfig
	ugCfg   *config.UpgradeConfig
	state   atomic.Int32 // State 枚举
	version string       // 当前运行版本

	// 升级成功后关闭此 channel，告知 Watchdog 可以退出
	commitCh chan struct{}
}

// New 创建升级管理器。
func New(agentCfg *config.AgentConfig, ugCfg *config.UpgradeConfig, currentVersion string) *Manager {
	m := &Manager{
		cfg:      agentCfg,
		ugCfg:    ugCfg,
		version:  currentVersion,
		commitCh: make(chan struct{}),
	}
	m.state.Store(int32(StateIdle))
	return m
}

// ─────────────────────────────────────────────
//  升级状态机入口
// ─────────────────────────────────────────────

// StartUpgrade 启动升级流程（异步），立即返回。
// 调用方在 UpgradeResponse.Accepted=true 后，等待 Agent 自动重启。
func (m *Manager) StartUpgrade(req UpgradeRequest) error {
	if !m.state.CompareAndSwap(int32(StateIdle), int32(StateDownloading)) {
		return fmt.Errorf("upgrade already in progress (state=%d)", m.state.Load())
	}

	go m.runUpgrade(req)
	return nil
}

// OnHeartbeatSuccess 在新版本成功与控制台完成心跳握手后调用，提交升级。
func (m *Manager) OnHeartbeatSuccess() {
	if m.state.CompareAndSwap(int32(StateValidating), int32(StateCommitted)) {
		// 删除回滚状态文件，看门狗脚本检测不到文件后自动退出
		os.Remove(m.ugCfg.RollbackStateFile)
		close(m.commitCh)
	}
}

// ─────────────────────────────────────────────
//  升级流程
// ─────────────────────────────────────────────

type UpgradeRequest struct {
	DownloadURL         string
	SHA256Hash          string
	TargetVersion       string
	ValidateTimeoutSec  int
}

func (m *Manager) runUpgrade(req UpgradeRequest) {
	// Step 1: 下载新版本二进制
	newBinPath := filepath.Join(m.cfg.BinDir, fmt.Sprintf("agent_v%s", req.TargetVersion))
	if err := m.downloadAndVerify(req.DownloadURL, req.SHA256Hash, newBinPath); err != nil {
		m.state.Store(int32(StateIdle))
		logError("upgrade download failed: %v", err)
		return
	}

	// Step 2: 记录当前版本路径到回滚状态文件
	currentSymlink := filepath.Join(m.cfg.BinDir, "agent_current")
	currentTarget, err := os.Readlink(currentSymlink)
	if err != nil {
		m.state.Store(int32(StateIdle))
		logError("readlink current: %v", err)
		return
	}

	if err := os.WriteFile(m.ugCfg.RollbackStateFile, []byte(currentTarget), 0640); err != nil {
		m.state.Store(int32(StateIdle))
		logError("write rollback state: %v", err)
		return
	}

	// Step 3: 切换软链接到新版本
	tmpLink := currentSymlink + ".new"
	os.Remove(tmpLink)
	if err := os.Symlink(newBinPath, tmpLink); err != nil {
		m.state.Store(int32(StateIdle))
		logError("create symlink: %v", err)
		return
	}
	if err := os.Rename(tmpLink, currentSymlink); err != nil {
		m.state.Store(int32(StateIdle))
		logError("rename symlink: %v", err)
		return
	}

	// Step 4: 进入 Validating 状态
	m.state.Store(int32(StateValidating))

	// Step 5: 重启自身（由 systemd 重新拉起新版本）
	// Agent 执行此命令后，当前进程会被终止，新进程接管后调用 OnHeartbeatSuccess
	logInfo("upgrade: restarting agent service to activate version %s", req.TargetVersion)
	if err := restartSelf(); err != nil {
		logError("restart self: %v", err)
	}
	// 注意：正常情况下此行及以后代码不会被执行（进程已重启）
}

// ─────────────────────────────────────────────
//  下载与验证
// ─────────────────────────────────────────────

func (m *Manager) downloadAndVerify(url, expectedSHA256, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	tmpPath := destPath + ".downloading"
	defer os.Remove(tmpPath) // 失败时清理

	// 带超时的 HTTP GET
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hasher), resp.Body); err != nil {
		f.Close()
		return fmt.Errorf("write: %w", err)
	}
	f.Close()

	// 校验 SHA256
	actual := hex.EncodeToString(hasher.Sum(nil))
	if actual != expectedSHA256 {
		return fmt.Errorf("sha256 mismatch: want %s got %s", expectedSHA256, actual)
	}

	// 原子移动
	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}

// ─────────────────────────────────────────────
//  辅助
// ─────────────────────────────────────────────

func restartSelf() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 通过已授权的 sudo 重启自身（sudoers 中允许 slb-agent 用户 restart slb-ops-agent.service）
	cmd := execCommandContext(ctx, "sudo", "systemctl", "restart", "slb-ops-agent.service")
	return cmd.Run()
}

// logInfo / logError 为占位符，正式实现中替换为结构化日志库
func logInfo(format string, args ...any)  { fmt.Printf("[upgrade] INFO  "+format+"\n", args...) }
func logError(format string, args ...any) { fmt.Printf("[upgrade] ERROR "+format+"\n", args...) }

// execCommandContext 是 exec.CommandContext 的别名，方便测试时 mock。
var execCommandContext = func(ctx context.Context, name string, arg ...string) interface {
	Run() error
} {
	import_exec_cmd, _ := context.WithCancel(ctx) // placeholder
	_ = import_exec_cmd
	// 实际使用 exec.CommandContext(ctx, name, arg...)
	return nil
}
