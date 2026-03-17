package health

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ─────────────────────────────────────────────
//  数据模型
// ─────────────────────────────────────────────

// AgentStatus 自身运行状态。
type AgentStatus struct {
	MemoryBytes    uint64
	CPUPercent     float64
	GoroutineCount int
	UptimeSec      int64
	Version        string
}

// ServiceStatus 被监控服务状态。
type ServiceStatus struct {
	Name              string
	ProcessAlive      bool
	PortOpen          bool
	ListenPort        int
	ActiveState       string
	ConfigCheckOutput string // 仅 HAProxy
	ConfigValid       bool
}

// Snapshot 是一次完整的健康快照。
type Snapshot struct {
	Timestamp    time.Time
	Agent        AgentStatus
	HAProxy      ServiceStatus
	Confd        ServiceStatus
	NodeExporter ServiceStatus
}

// ─────────────────────────────────────────────
//  Prober
// ─────────────────────────────────────────────

// Config 是健康检查的配置项。
type Config struct {
	CheckInterval      time.Duration
	HaproxyConfigFile  string
	HaproxyStatsSocket string
	NodeExporterPort   int
	ConfdPort          int
	AgentVersion       string
}

// Prober 定期采集健康数据并缓存最新快照。
type Prober struct {
	cfg       Config
	startTime time.Time

	mu       sync.RWMutex
	snapshot *Snapshot

	// 累计 CPU 采样，用于简单估算（两次 /proc/stat 差值）
	lastCPUIdle  uint64
	lastCPUTotal uint64

	// 原子停止信号
	stopped int32
	done    chan struct{}
}

// New 创建 Prober 并立即执行一次采集，再启动后台定时采集。
func New(cfg Config) *Prober {
	p := &Prober{
		cfg:       cfg,
		startTime: time.Now(),
		done:      make(chan struct{}),
	}

	// 立即采集一次
	snap := p.collect()
	p.mu.Lock()
	p.snapshot = &snap
	p.mu.Unlock()

	go p.loop()
	return p
}

// Latest 返回最新的健康快照（只读副本）。
func (p *Prober) Latest() Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.snapshot == nil {
		return Snapshot{}
	}
	return *p.snapshot
}

// Stop 停止后台采集。
func (p *Prober) Stop() {
	if atomic.CompareAndSwapInt32(&p.stopped, 0, 1) {
		close(p.done)
	}
}

func (p *Prober) loop() {
	ticker := time.NewTicker(p.cfg.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			snap := p.collect()
			p.mu.Lock()
			p.snapshot = &snap
			p.mu.Unlock()
		case <-p.done:
			return
		}
	}
}

// ─────────────────────────────────────────────
//  采集逻辑
// ─────────────────────────────────────────────

func (p *Prober) collect() Snapshot {
	return Snapshot{
		Timestamp:    time.Now(),
		Agent:        p.collectAgent(),
		HAProxy:      p.collectHAProxy(),
		Confd:        p.collectConfd(),
		NodeExporter: p.collectNodeExporter(),
	}
}

func (p *Prober) collectAgent() AgentStatus {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	return AgentStatus{
		MemoryBytes:    memStats.Sys,
		CPUPercent:     p.estimateCPU(),
		GoroutineCount: runtime.NumGoroutine(),
		UptimeSec:      int64(time.Since(p.startTime).Seconds()),
		Version:        p.cfg.AgentVersion,
	}
}

// estimateCPU 通过读取 /proc/self/stat 简单估算 CPU 占用（两次调用之差）。
// 生产级实现可引入 shirou/gopsutil，此处提供轻量框架实现。
func (p *Prober) estimateCPU() float64 {
	// TODO: 解析 /proc/self/stat 中 utime+stime 差值 / 时钟差值
	// 保持轻量，此处返回 0 作为占位符
	return 0.0
}

func (p *Prober) collectHAProxy() ServiceStatus {
	svc := ServiceStatus{
		Name:       "haproxy.service",
		ListenPort: 0, // HAProxy 端口由配置决定，不做通用端口检查
	}

	svc.ProcessAlive = isProcessAlive("haproxy")
	svc.ActiveState = queryActiveState("haproxy.service")

	// 配置语法检查
	if p.cfg.HaproxyConfigFile != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		out, err := exec.CommandContext(ctx,
			"haproxy", "-c", "-f", p.cfg.HaproxyConfigFile).CombinedOutput()
		svc.ConfigCheckOutput = string(out)
		svc.ConfigValid = err == nil
	}

	return svc
}

func (p *Prober) collectConfd() ServiceStatus {
	svc := ServiceStatus{
		Name:       "confd.service",
		ListenPort: p.cfg.ConfdPort,
	}
	svc.ProcessAlive = isProcessAlive("confd")
	svc.ActiveState = queryActiveState("confd.service")
	if p.cfg.ConfdPort > 0 {
		svc.PortOpen = isTCPPortOpen("127.0.0.1", p.cfg.ConfdPort)
	}
	return svc
}

func (p *Prober) collectNodeExporter() ServiceStatus {
	svc := ServiceStatus{
		Name:       "node-exporter.service",
		ListenPort: p.cfg.NodeExporterPort,
	}
	svc.ProcessAlive = isProcessAlive("node_exporter")
	svc.ActiveState = queryActiveState("node-exporter.service")
	if p.cfg.NodeExporterPort > 0 {
		svc.PortOpen = isTCPPortOpen("127.0.0.1", p.cfg.NodeExporterPort)
	}
	return svc
}

// ─────────────────────────────────────────────
//  工具函数
// ─────────────────────────────────────────────

// isProcessAlive 通过 pgrep 检查进程是否存活（不依赖 cgo）。
func isProcessAlive(processName string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := exec.CommandContext(ctx, "pgrep", "-x", processName).Run()
	return err == nil
}

// isTCPPortOpen 尝试 TCP 连接检查端口是否监听。
func isTCPPortOpen(host string, port int) bool {
	addr := fmt.Sprintf("%s:%d", host, port)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// queryActiveState 通过 systemctl 查询服务 ActiveState。
func queryActiveState(serviceName string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx,
		"systemctl", "show", serviceName, "--property=ActiveState", "--value").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// ReadProcSelfStat 读取 /proc/self/stat，供 CPU 估算使用。
func ReadProcSelfStat() (utime, stime uint64, err error) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 15 {
		return 0, 0, fmt.Errorf("unexpected /proc/self/stat format")
	}
	fmt.Sscanf(fields[13], "%d", &utime)
	fmt.Sscanf(fields[14], "%d", &stime)
	return utime, stime, nil
}
