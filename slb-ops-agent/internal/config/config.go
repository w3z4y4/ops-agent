package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是 Agent 全局配置，从 YAML 文件加载。
type Config struct {
	Agent   AgentConfig   `yaml:"agent"`
	GRPC    GRPCConfig    `yaml:"grpc"`
	TLS     TLSConfig     `yaml:"tls"`
	ACL     ACLConfig     `yaml:"acl"`
	Logging LoggingConfig `yaml:"logging"`
	Health  HealthConfig  `yaml:"health"`
	Upgrade UpgradeConfig `yaml:"upgrade"`
	Services ServicesConfig `yaml:"services"`
}

type AgentConfig struct {
	// Agent 版本，由编译时注入
	Version string `yaml:"-"`
	// 数据/状态文件根目录
	DataDir string `yaml:"data_dir"` // default: /opt/slb-agent/data
	// 二进制文件目录
	BinDir  string `yaml:"bin_dir"`  // default: /opt/slb-agent/bin
}

type GRPCConfig struct {
	// 监听地址，如 0.0.0.0:9443
	ListenAddr string `yaml:"listen_addr"`
	// 单次请求最大消息体（字节），默认 64MB
	MaxRecvMsgSize int `yaml:"max_recv_msg_size"`
	MaxSendMsgSize int `yaml:"max_send_msg_size"`
}

type TLSConfig struct {
	// Agent 自身证书（服务端 + 客户端双向）
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// 用于验证控制台证书的 CA
	CACertFile string `yaml:"ca_cert_file"`
}

type ACLConfig struct {
	// 允许连接的控制台 IP/CIDR 列表，留空则不做 IP 限制（仅依赖 mTLS）
	AllowedCIDRs []string `yaml:"allowed_cidrs"`
}

type LoggingConfig struct {
	// 日志级别: debug / info / warn / error
	Level string `yaml:"level"`
	// 审计日志文件路径
	AuditFile string `yaml:"audit_file"` // default: /var/log/slb-agent/audit.log
	// 运行日志文件路径（若为空则输出到 stdout，由 systemd 接管）
	AppFile string `yaml:"app_file"`
}

type HealthConfig struct {
	// 本地健康检查间隔
	CheckInterval time.Duration `yaml:"check_interval"` // default: 15s
	// HAProxy 配置文件路径（用于 -c 语法检查）
	HaproxyConfigFile string `yaml:"haproxy_config_file"` // default: /etc/haproxy/haproxy.cfg
	// HAProxy stats socket
	HaproxyStatsSocket string `yaml:"haproxy_stats_socket"` // default: /var/run/haproxy/admin.sock
	// node-exporter 监听端口
	NodeExporterPort int `yaml:"node_exporter_port"` // default: 9100
	// confd 监听端口（若无 HTTP 端口可设为 0 仅检查进程）
	ConfdPort int `yaml:"confd_port"`
}

type UpgradeConfig struct {
	// 升级后等待心跳握手超时
	ValidateTimeout time.Duration `yaml:"validate_timeout"` // default: 180s
	// 回滚状态文件路径
	RollbackStateFile string `yaml:"rollback_state_file"` // default: /opt/slb-agent/data/rollback_target.txt
	// 看门狗脚本路径
	WatchdogScript string `yaml:"watchdog_script"` // default: /opt/slb-agent/bin/watchdog.sh
}

// ServicesConfig 定义可操控的服务白名单及相关参数
type ServicesConfig struct {
	// 白名单，硬编码默认值见 DefaultAllowedServices
	AllowedServices []string `yaml:"allowed_services"`
}

// DefaultAllowedServices 硬编码白名单兜底值
var DefaultAllowedServices = []string{
	"haproxy.service",
	"confd.service",
	"node-exporter.service",
	"slb-ops-agent.service",
}

// Load 从指定路径加载 YAML 配置文件，并填充默认值。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config file: %w", err)
	}

	applyDefaults(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

func applyDefaults(c *Config) {
	if c.Agent.DataDir == "" {
		c.Agent.DataDir = "/opt/slb-agent/data"
	}
	if c.Agent.BinDir == "" {
		c.Agent.BinDir = "/opt/slb-agent/bin"
	}
	if c.GRPC.ListenAddr == "" {
		c.GRPC.ListenAddr = "0.0.0.0:9443"
	}
	if c.GRPC.MaxRecvMsgSize == 0 {
		c.GRPC.MaxRecvMsgSize = 64 * 1024 * 1024 // 64MB
	}
	if c.GRPC.MaxSendMsgSize == 0 {
		c.GRPC.MaxSendMsgSize = 64 * 1024 * 1024
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.AuditFile == "" {
		c.Logging.AuditFile = "/var/log/slb-agent/audit.log"
	}
	if c.Health.CheckInterval == 0 {
		c.Health.CheckInterval = 15 * time.Second
	}
	if c.Health.HaproxyConfigFile == "" {
		c.Health.HaproxyConfigFile = "/etc/haproxy/haproxy.cfg"
	}
	if c.Health.HaproxyStatsSocket == "" {
		c.Health.HaproxyStatsSocket = "/var/run/haproxy/admin.sock"
	}
	if c.Health.NodeExporterPort == 0 {
		c.Health.NodeExporterPort = 9100
	}
	if c.Upgrade.ValidateTimeout == 0 {
		c.Upgrade.ValidateTimeout = 180 * time.Second
	}
	if c.Upgrade.RollbackStateFile == "" {
		c.Upgrade.RollbackStateFile = "/opt/slb-agent/data/rollback_target.txt"
	}
	if c.Upgrade.WatchdogScript == "" {
		c.Upgrade.WatchdogScript = "/opt/slb-agent/bin/watchdog.sh"
	}
	if len(c.Services.AllowedServices) == 0 {
		c.Services.AllowedServices = DefaultAllowedServices
	}
}

func validate(c *Config) error {
	if c.TLS.CertFile == "" || c.TLS.KeyFile == "" || c.TLS.CACertFile == "" {
		return fmt.Errorf("tls.cert_file, tls.key_file and tls.ca_cert_file are required")
	}
	return nil
}
