#!/usr/bin/env bash
# 停止 trpc-agent-service（优雅退出，超时后强杀）。
set -euo pipefail
cd "$(dirname "$0")"

pid_file="data/trpc-service.pid"
if [[ ! -f "${pid_file}" ]]; then
  echo "trpc-agent-service is not running"
  exit 0
fi

pid="$(cat "${pid_file}")"
if kill -0 "${pid}" 2>/dev/null; then
  kill "${pid}"
  for _ in $(seq 1 40); do
    kill -0 "${pid}" 2>/dev/null || break
    sleep 0.5
  done
  if kill -0 "${pid}" 2>/dev/null; then
    kill -9 "${pid}" || true
  fi
  echo "trpc-agent-service stopped"
else
  echo "stale pid file removed"
fi
rm -f "${pid_file}"
