# 轻量级服务器运维 Agent 软件需求说明书 (针对 AI 代码生成优化)

## `<Document_Context>`
* **Target Audience:** AI Code Generator / LLM.
* **System Name:** SLB-Ops-Agent (Soft Load Balancing Operations Agent).
* **Language & Stack:** 强烈建议使用 **Go (Golang)**。要求编译为无外部依赖的单一静态二进制文件。
* **Operating System:** Linux (CentOS/RedHat/Ubuntu)，基于 `systemd` 进行服务管理。
* **Core Objective:** 提供一个极度轻量、稳定、安全的常驻守护进程，用于接收控制台指令，管理宿主机自身及指定的软负载均衡生态软件（HAProxy、confd、node-exporter）。

## `<Architecture_&_Deployment_Constraints>`
1.  **Resource Limits:** 必须极度轻量。内存占用稳态 `< 30MB`，CPU 占用峰值 `< 5%`。不得影响核心业务（HAProxy）的流量转发。
2.  **Daemonization:** Agent 自身必须通过 `systemd` 托管，配置 `Restart=always` 和 `RestartSec=5`。
3.  **Dependency:** 不得依赖 Python、Java、Node.js 等解释器或运行环境。
4.  **Graceful Shutdown:** 捕获 `SIGTERM` 和 `SIGINT` 信号，确保正在执行的非中断类任务完成后再退出。


## `<Security_&_Authentication_Requirements>`
* **API Security:** 所有入站/出站通信必须采用 **mTLS (双向 TLS 认证)**。Agent 必须验证控制台下发的证书签发者（CA），拒绝任何未授权的客户端连接。
* **Network ACL:** 支持配置静态 IP 白名单，仅允许指定管理控制台网段调用接口。
* **Least Privilege:** Agent 进程尽量以非 root 用户运行。涉及特权操作（如 `systemctl reload haproxy`），通过 `/etc/sudoers` 精细化授权，禁止使用 `sudo su` 或无限制的 `sudo`。
* **Audit Logging:** 所有接收到的远程指令及执行结果，必须记录本地审计日志（包含时间戳、调用方 IP、指令类型、执行参数、返回码），并异步上报至控制台。

## `<Core_Feature_Specifications>`

### 1. 远程命令执行模块 (Command Executor)
* **Requirement:** 提供同步和异步两种执行模式。
* **Input:** 接收标准化的命令结构体（Command: string, Args: []string, Timeout: int, Env: map[string]string）。
* **Execution:** 必须使用 Go 的 `os/exec` 或类似机制。必须使用 `context.WithTimeout` 严格控制命令执行超时，防止僵尸进程。
* **Output:** 捕获并合并 `stdout` 和 `stderr`，记录 Exit Code。

### 2. 文件传输管理模块 (File Manager)
* **Requirement:** 支持大文件分块上传与下载。
* **Integrity:** 传输开始前和结束后，必须进行 SHA256 校验。若不一致，直接丢弃临时文件并返回错误。
* **Atomic Write:** 下载文件时，先写入 `.tmp` 后缀的临时文件，校验成功后，通过 `os.Rename` 原子性重命名为目标文件（如 HAProxy 配置文件），防止中途断电或中断导致配置文件损坏。
* **Permissions:** 支持在文件写入后，根据参数修改文件的所属用户/组及权限（Chown/Chmod）。

### 3. 系统服务管控模块 (Service Controller)
* **Requirement:** 封装对 `systemd` 的操作接口。
* **Actions:** 支持 `start`, `stop`, `restart`, `reload`, `status`, `enable`, `disable`。
* **Domain Isolation:** 限制只能操作白名单内的服务，硬编码或通过配置文件限制为：`haproxy.service`, `confd.service`, `node-exporter.service`, 以及 `slb-ops-agent.service`（自身）。

### 4. 健康检查与状态收集模块 (Health Probe)
* **Requirement:** 定时（如每隔 15 秒）在本地执行检查，并将状态缓存。
* **Targets:**
    * **Self:** 自身内存/CPU占用，Goroutine 数量，运行 uptime。
    * **HAProxy:** 进程存活状态，通过 Socket 尝试拉取 HAProxy stats（可选），配置语法检查（`haproxy -c -f /etc/haproxy/haproxy.cfg`）。
    * **node-exporter / confd:** 进程存活状态，监听端口是否被正常占用。
* **Export:** 提供 `/health` 或相应 gRPC 接口供控制台主动拉取（Pull）或主动上报（Push）。

### 5. 生命周期管理与自动回滚模块 (Upgrade & Watchdog) -> *Critical*

* **Requirement:** 提供 Agent 自身的平滑升级能力，并在失败时自动恢复（A/B 部署模式）。
* **State Machine Logic:**
    1.  **Download:** Agent 下载新版本二进制文件至 `/opt/slb-agent/bin/agent_v{new}`。
    2.  **Symlink Switch:** 将运行软链接 `/opt/slb-agent/bin/agent_current` 指向新版本。
    3.  **Watchdog Setup:** 记录当前旧版本路径到本地状态文件（如 `rollback_target.txt`）。启动一个独立的、轻量级的后台看门狗脚本（Bash 脚本或由 systemd timer 触发）。
    4.  **Restart:** Agent 执行 `systemctl restart slb-ops-agent` 重启自身。
    5.  **Validation:** 新版本启动后，必须在设定时间（如 180 秒）内成功与管理控制台完成一次 mTLS 心跳握手。
    6.  **Commit / Rollback:**
        * *Success:* 如果握手成功，Agent 删除 `rollback_target.txt`，看门狗脚本检测不到该文件，平滑退出。
        * *Fail:* 如果 180 秒内握手失败（或新 Agent 崩溃无法启动），看门狗脚本介入，强制将 `agent_current` 软链接改回 `rollback_target.txt` 中记录的旧版本，并再次执行 `systemctl restart slb-ops-agent`，恢复通信。

## `<API_Data_Structures_Example>` (用于指导 AI 生成接口契约)

```protobuf
// 强烈推荐使用 gRPC 定义接口，以下为伪代码示例
service AgentService {
  rpc ExecuteCommand(CommandRequest) returns (CommandResponse);
  rpc TransferFile(stream FileChunk) returns (TransferStatus);
  rpc ManageService(ServiceRequest) returns (ServiceResponse);
  rpc GetHealthStatus(HealthRequest) returns (HealthResponse);
  rpc UpgradeAgent(UpgradeRequest) returns (UpgradeResponse);
}

message CommandRequest {
  string command = 1;      // e.g., "haproxy"
  repeated string args = 2; // e.g., ["-c", "-f", "/etc/haproxy/haproxy.cfg"]
  int32 timeout_sec = 3;   // e.g., 30
}

message UpgradeRequest {
  string download_url = 1;
  string sha256_hash = 2;
  string target_version = 3;
}
```