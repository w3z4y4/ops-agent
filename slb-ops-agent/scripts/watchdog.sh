#!/usr/bin/env bash
# =============================================================================
# watchdog.sh — SLB-Ops-Agent 升级看门狗脚本
#
# 工作原理：
#   升级流程写入 ROLLBACK_STATE_FILE（内含旧版本路径）后重启 Agent。
#   本脚本在后台持续轮询：
#     - 若 ROLLBACK_STATE_FILE 消失 → 升级已由新 Agent 主动提交，脚本退出。
#     - 若超过 VALIDATE_TIMEOUT 秒后文件仍存在 → 新版本验证失败，执行回滚。
#
# 部署方式：
#   推荐通过 systemd timer 或 nohup 在升级前由 Agent 启动本脚本。
#   升级成功后 Agent 会删除 ROLLBACK_STATE_FILE，本脚本自动退出。
#
# 依赖：bash, systemctl, ln, sleep（均为 Linux 基础命令）
# =============================================================================

set -euo pipefail

# ── 配置（可通过环境变量覆盖）────────────────────────────────────────────────
ROLLBACK_STATE_FILE="${ROLLBACK_STATE_FILE:-/opt/slb-agent/data/rollback_target.txt}"
SYMLINK_PATH="${SYMLINK_PATH:-/opt/slb-agent/bin/agent_current}"
SERVICE_NAME="${SERVICE_NAME:-slb-ops-agent.service}"
VALIDATE_TIMEOUT="${VALIDATE_TIMEOUT:-180}"   # 秒
POLL_INTERVAL="${POLL_INTERVAL:-5}"           # 轮询间隔（秒）
LOG_FILE="${LOG_FILE:-/var/log/slb-agent/watchdog.log}"

# ── 日志函数 ─────────────────────────────────────────────────────────────────
log() {
  local level="$1"; shift
  echo "$(date '+%Y-%m-%dT%H:%M:%S%z') [watchdog] [$level] $*" | tee -a "$LOG_FILE" >&2
}

# ── 前置检查 ─────────────────────────────────────────────────────────────────
if [[ ! -f "$ROLLBACK_STATE_FILE" ]]; then
  log INFO "rollback state file not found, nothing to watch. exiting."
  exit 0
fi

log INFO "watchdog started. validate_timeout=${VALIDATE_TIMEOUT}s state_file=${ROLLBACK_STATE_FILE}"

# ── 主轮询循环 ────────────────────────────────────────────────────────────────
elapsed=0

while (( elapsed < VALIDATE_TIMEOUT )); do
  sleep "$POLL_INTERVAL"
  elapsed=$(( elapsed + POLL_INTERVAL ))

  if [[ ! -f "$ROLLBACK_STATE_FILE" ]]; then
    log INFO "rollback state file removed by new agent — upgrade committed. watchdog exiting."
    exit 0
  fi

  log DEBUG "waiting for new agent to commit... (${elapsed}/${VALIDATE_TIMEOUT}s)"
done

# ── 超时，执行回滚 ────────────────────────────────────────────────────────────
log WARN "validate timeout reached (${VALIDATE_TIMEOUT}s). rolling back..."

ROLLBACK_TARGET="$(cat "$ROLLBACK_STATE_FILE")"

if [[ -z "$ROLLBACK_TARGET" ]]; then
  log ERROR "rollback target is empty! cannot rollback. manual intervention required."
  exit 1
fi

if [[ ! -x "$ROLLBACK_TARGET" ]]; then
  log ERROR "rollback target '$ROLLBACK_TARGET' is not executable. manual intervention required."
  exit 1
fi

log INFO "switching symlink: $SYMLINK_PATH -> $ROLLBACK_TARGET"

# 原子切换软链接（ln -sfn 是原子操作）
ln -sfn "$ROLLBACK_TARGET" "${SYMLINK_PATH}.rollback" \
  && mv -f "${SYMLINK_PATH}.rollback" "$SYMLINK_PATH"

log INFO "restarting service: $SERVICE_NAME"
sudo systemctl restart "$SERVICE_NAME"

# 等待服务重新启动
sleep 5
if systemctl is-active --quiet "$SERVICE_NAME"; then
  log INFO "rollback successful. service $SERVICE_NAME is active."
  # 清除状态文件，避免下次误判
  rm -f "$ROLLBACK_STATE_FILE"
  exit 0
else
  log ERROR "rollback failed: service $SERVICE_NAME is not active after restart."
  exit 2
fi
