#!/usr/bin/env bash
# 后台启动 trpc-agent-service；PID 与日志写入 data/。
# 可用环境变量：ADMIN_TOKEN（必须或使用默认演示值）、CONFIG、SMOKE 之外的同名端口变量见 config。
set -euo pipefail
cd "$(dirname "$0")"

mkdir -p data
pid_file="data/trpc-service.pid"
log_file="data/trpc-service.log"

if [[ -f "${pid_file}" ]] && kill -0 "$(cat "${pid_file}")" 2>/dev/null; then
  echo "trpc-agent-service already running (pid $(cat "${pid_file}"))"
  exit 0
fi

if [[ ! -x bin/trpc-service ]]; then
  ./build.sh
fi

ADMIN_TOKEN="${ADMIN_TOKEN:-local-admin}" \
  nohup bin/trpc-service -config "${CONFIG:-config/example.yaml}" \
  >>"${log_file}" 2>&1 &
echo $! >"${pid_file}"
echo "trpc-agent-service started (pid $(cat "${pid_file}"), log ${log_file})"
