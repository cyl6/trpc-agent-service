#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
solution_root="$(cd "${script_dir}/.." && pwd)"
smoke_port="${SMOKE_PORT:-18080}"
smoke_admin_token="smoke-placeholder-admin-token"
tmp_dir="$(mktemp -d -t trpc-service-smoke-XXXXXX)"
service_pid=""

cleanup() {
  if [[ -n "${service_pid}" ]] && kill -0 "${service_pid}" 2>/dev/null; then
    kill "${service_pid}" 2>/dev/null || true
    wait "${service_pid}" 2>/dev/null || true
  fi
  rm -rf "${tmp_dir}"
}
trap cleanup EXIT INT TERM

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "required command not found: $1" >&2
    exit 1
  fi
}

wait_for_http() {
  local url="$1"
  local attempts="${2:-80}"
  local attempt
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    if curl --silent --show-error --fail --max-time 2 "${url}" >/dev/null 2>&1; then
      return 0
    fi
    if [[ -n "${service_pid}" ]] && ! kill -0 "${service_pid}" 2>/dev/null; then
      echo "service exited before becoming ready" >&2
      sed -n '1,240p' "${tmp_dir}/service.log" >&2
      return 1
    fi
    sleep 0.25
  done
  echo "timed out waiting for ${url}" >&2
  sed -n '1,240p' "${tmp_dir}/service.log" >&2
  return 1
}

require_command go
require_command curl

echo "==> unit tests"
(
  cd "${solution_root}"
  go test ./...
)

echo "==> static analysis"
(
  cd "${solution_root}"
  go vet ./...
  CGO_ENABLED=0 go build -trimpath -o "${tmp_dir}/trpc-service" ./cmd/trpc-service
)

echo "==> deployment syntax"
if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
  docker compose \
    -f "${solution_root}/deploy/compose.yaml" \
    config --quiet
else
  echo "docker compose not installed; compose rendering skipped"
fi

if command -v kubectl >/dev/null 2>&1; then
  kubectl kustomize "${solution_root}/deploy/k8s" >"${tmp_dir}/kubernetes.yaml"
  test -s "${tmp_dir}/kubernetes.yaml"
else
  echo "kubectl not installed; kustomize rendering skipped"
fi

if command -v promtool >/dev/null 2>&1; then
  promtool check config "${solution_root}/observability/prometheus.yaml"
else
  echo "promtool not installed; Prometheus semantic validation skipped"
fi

echo "==> local HTTP smoke test"
sed \
  "s/address: \":8080\"/address: \"127.0.0.1:${smoke_port}\"/" \
  "${solution_root}/config/example.yaml" \
  >"${tmp_dir}/config.yaml"

ADMIN_TOKEN="${smoke_admin_token}" \
  "${tmp_dir}/trpc-service" -config "${tmp_dir}/config.yaml" \
  >"${tmp_dir}/service.log" 2>&1 &
service_pid="$!"

base_url="http://127.0.0.1:${smoke_port}"
wait_for_http "${base_url}/readyz"

curl --silent --show-error --fail "${base_url}/healthz" \
  | grep -q '"status":"ok"'
curl --silent --show-error --fail "${base_url}/readyz" \
  | grep -q '"status":"ready"'
curl --silent --show-error --fail \
  -H "Authorization: Bearer ${smoke_admin_token}" \
  "${base_url}/admin/v1/tenants" \
  | grep -q '"tenant_id":"acme"'

request_body='{"message_id":"smoke-message-1","user_id":"smoke-user","conversation_id":"smoke-user","scope":"direct","text":"hello from smoke test"}'
first_response="$(curl --silent --show-error --fail \
  -H "Authorization: Bearer ${smoke_admin_token}" \
  -H 'Content-Type: application/json' \
  --data "${request_body}" \
  "${base_url}/v1/chat/acme")"
grep -q '"duplicate":false' <<<"${first_response}"
grep -q '"session_id":"' <<<"${first_response}"

duplicate_response="$(curl --silent --show-error --fail \
  -H "Authorization: Bearer ${smoke_admin_token}" \
  -H 'Content-Type: application/json' \
  --data "${request_body}" \
  "${base_url}/v1/chat/acme")"
grep -q '"duplicate":true' <<<"${duplicate_response}"

curl --silent --show-error --fail "${base_url}/metrics" \
  | grep -q '^agent_requests_total'

if grep -Fq "${smoke_admin_token}" "${tmp_dir}/service.log"; then
  echo "smoke placeholder secret leaked into application logs" >&2
  exit 1
fi

echo "smoke test passed"
