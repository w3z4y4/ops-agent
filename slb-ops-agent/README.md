# SLB-Ops-Agent

轻量级服务器运维守护进程，用于管理宿主机上的 HAProxy / confd / node-exporter 生态。

## 架构总览

```
┌─────────────────────────────────────────────────────────────────┐
│                        Management Console                        │
│                    (gRPC client, mTLS cert)                      │
└──────────────────────────┬──────────────────────────────────────┘
                           │ mTLS gRPC :9443
                           ▼
┌─────────────────────────────────────────────────────────────────┐
│                       slb-ops-agent                              │
│                                                                  │
│  ┌─────────────┐  ┌─────────────┐  ┌──────────────────────┐   │
│  │  IP ACL     │  │ Audit Logger│  │  mTLS Interceptor    │   │
│  │ Interceptor │  │  (async)    │  │  (require client cert)│   │
│  └──────┬──────┘  └─────────────┘  └──────────────────────┘   │
│         │                                                        │
│  ┌──────▼──────────────────────────────────────────────────┐   │
│  │                    gRPC Handler                          │   │
│  └──┬──────────┬──────────────┬───────────────┬───────────┘   │
│     │          │              │               │                  │
│  ┌──▼──┐  ┌───▼────┐  ┌─────▼─────┐  ┌─────▼──────┐        │
│  │Exec │  │FileMgr │  │ServiceCtl │  │Health Probe│        │
│  │utor │  │(SHA256)│  │(allowlist)│  │(15s cache) │        │
│  └──┬──┘  └───┬────┘  └─────┬─────┘  └─────┬──────┘        │
│     │         │              │               │                  │
│  os/exec  atomic write  systemctl       pgrep/port            │
│           + chown/chmod  (sudo ACL)    /proc/self/stat        │
│                                                                  │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │              Upgrade Manager (A/B Deploy)                │   │
│  │  download → symlink switch → restart → heartbeat verify  │   │
│  │                      ↕ rollback_target.txt               │   │
│  └─────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
         │ sudo systemctl           │ watchdog.sh (bash)
         ▼                          ▼
  haproxy / confd /          rollback on timeout
  node-exporter /            (symlink → old version)
  slb-ops-agent
```

## 目录结构

```
slb-ops-agent/
├── api/
│   └── agent.proto               # gRPC 接口定义（唯一事实来源）
├── cmd/
│   └── agent/
│       └── main.go               # 入口：配置加载、模块组装、信号处理
├── internal/
│   ├── config/
│   │   └── config.go             # YAML 配置结构体 + 默认值 + 校验
│   ├── executor/
│   │   └── executor.go           # 同步/异步命令执行，context 超时防僵尸
│   ├── filemanager/
│   │   └── manager.go            # 分块上传/下载，SHA256 校验，原子写
│   ├── grpcserver/
│   │   ├── handler.go            # AgentServiceServer 实现（聚合所有子模块）
│   │   └── server.go             # gRPC Server 生命周期（mTLS + 拦截器装配）
│   ├── health/
│   │   └── prober.go             # 定时健康采集 + 快照缓存
│   ├── logger/
│   │   └── audit.go              # 异步审计日志（JSON per-line）
│   ├── security/
│   │   ├── tls.go                # mTLS 凭证构建工具
│   │   └── interceptor.go        # IP ACL + 审计 gRPC 拦截器
│   ├── servicectl/
│   │   └── controller.go         # systemd 封装 + 服务白名单
│   └── upgrade/
│       └── manager.go            # A/B 升级状态机 + 回滚状态文件管理
├── pkg/
│   └── proto/                    # protoc 生成产物（勿手动编辑）
│       ├── agent.pb.go
│       └── agent_grpc.pb.go
├── scripts/
│   └── watchdog.sh               # 升级看门狗脚本（Bash，独立于主进程）
├── deploy/
│   ├── systemd/
│   │   └── slb-ops-agent.service # systemd 单元文件
│   ├── sudoers/
│   │   └── slb-agent             # /etc/sudoers.d/ 精细化授权
│   └── config.example.yaml       # 配置文件模板
├── go.mod
└── Makefile
```

## 快速开始

### 1. 依赖工具

```bash
# Go 1.22+
go version

# protoc + 插件
apt install -y protobuf-compiler
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
```

### 2. 生成 Proto 代码并编译

```bash
make proto    # 生成 pkg/proto/*.go
make build    # 输出 dist/slb-ops-agent_<version>
```

编译结果为无 CGO 静态二进制：
```
-rwxr-xr-x 1 root root 12M dist/slb-ops-agent_v1.0.0
```

### 3. 初始化运行环境

```bash
# 创建专用用户（无登录 shell）
useradd -r -s /sbin/nologin slb-agent

# 目录结构
install -d -o slb-agent -g slb-agent /opt/slb-agent/{bin,data}
install -d -o slb-agent -g slb-agent /var/log/slb-agent
install -d -o root      -g slb-agent /etc/slb-agent/tls

# 开发环境自签证书（生产环境替换为真实 PKI）
make gen-dev-certs
cp deploy/tls/ca.crt    /etc/slb-agent/tls/
cp deploy/tls/agent.crt /etc/slb-agent/tls/
cp deploy/tls/agent.key /etc/slb-agent/tls/
chmod 640 /etc/slb-agent/tls/agent.key
chown slb-agent:slb-agent /etc/slb-agent/tls/agent.key

# 配置文件
cp deploy/config.example.yaml /etc/slb-agent/config.yaml
# 编辑 /etc/slb-agent/config.yaml 填写实际参数
```

### 4. 安装并启动

```bash
# 安装二进制 + 软链接
make install

# 安装看门狗脚本
make install-watchdog

# 安装 sudoers
sudo install -m 0440 deploy/sudoers/slb-agent /etc/sudoers.d/slb-agent
sudo visudo -c  # 验证语法

# 安装 systemd 单元
sudo cp deploy/systemd/slb-ops-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now slb-ops-agent

# 验证
sudo systemctl status slb-ops-agent
journalctl -u slb-ops-agent -f
```

## 安全设计要点

| 层面 | 措施 |
|------|------|
| 传输安全 | mTLS 1.3，双向证书验证，Agent 拒绝无效 CA 签发的客户端 |
| 网络准入 | 静态 IP/CIDR 白名单（gRPC Unary + Stream 双重拦截）|
| 运行权限 | `slb-agent` 非 root 账户；`/etc/sudoers.d/slb-agent` 精细授权单条命令 |
| 文件操作 | SHA256 端到端校验；`os.Rename` 原子写防止半写损坏 |
| 命令执行 | `context.WithTimeout` 硬超时；白名单内服务名校验 |
| 审计追踪 | 每条远程指令记录 caller IP / 方法 / 参数摘要 / 耗时 / 结果 |

## 升级与回滚流程

```
Console                         Agent (old)            watchdog.sh
  │                                │                        │
  │──UpgradeAgent(url,sha256)──►  │                        │
  │                          download & verify              │
  │                          write rollback_target.txt      │
  │                          switch symlink                 │
  │                          start watchdog.sh ────────►   │（后台启动，轮询 rollback_target.txt）
  │                          systemctl restart self         │
  │                                │ (进程终止)             │
  │                                                         │ 每 5s 检查文件是否存在
  │            Agent (new) 启动                             │
  │◄──────────Heartbeat()──────── │                        │
  │──────────HeartbeatOK──────►  │                        │
  │                          OnHeartbeatSuccess()           │
  │                          delete rollback_target.txt     │
  │                                                    检测到文件消失 → 退出 ✓
  │
  │ --- 若新版本 180s 内未心跳 ---
  │                                                    超时 → 改回旧 symlink
  │                                                    systemctl restart → 旧版本恢复
```

## 开发注意事项

1. **Proto 生成代码**：`pkg/proto/` 目录由 `make proto` 生成，不要手动编辑。
2. **`upgrade/manager.go`**：`execCommandContext` 变量支持在测试中替换为 mock，避免真实调用 systemctl。
3. **`filemanager/manager.go`**：`hash.Hash` 字段在最终代码中应使用 `import "hash"` 并改为 `hash.Hash` 类型，框架注释中已说明。
4. **`security/interceptor.go`**：`summarizeRequest` 函数需根据各 proto 消息类型补充字段摘取逻辑（避免将大 payload 写入审计日志）。
5. **CPU 估算**：`health/prober.go` 中 `estimateCPU()` 为占位实现，建议解析 `/proc/self/stat` 两次差值或引入 `github.com/shirou/gopsutil`（注意 CGO 依赖问题）。
