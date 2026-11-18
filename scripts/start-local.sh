#!/usr/bin/env bash
# Start, stop and inspect the single-user local edition from this checkout.
#
# The local edition (AGENTFLOW_LOCAL_MODE=1) has no accounts, no cloud
# services and no execution machines: it runs the CLI logins this machine
# already has, on a database that stays on this machine.
#
# Usage:
#   start-local.sh [start|stop|restart|status|logs] [--foreground]
#
# Environment:
#   AGENTFLOW_MYSQL_DSN   use this database and skip the Compose MySQL entirely
#   AGENTFLOW_HTTP_PORT   listen port, default 8090 (loopback only)
#   LOOPFORGE_DATA_DIR    data root, default .data (state lands in <root>/local)
#   LOOPFORGE_MYSQL_PORT  published MySQL port for the Compose database,
#                         default 33315 (from .env when configured)
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/docker-access.sh

fail() { printf '\n✗ %s\n' "$1" >&2; exit 1; }

command_name=start
foreground=0
for argument in "$@"; do
  case "$argument" in
    start|stop|restart|status|logs) command_name="$argument" ;;
    --foreground|-f) foreground=1 ;;
    -h|--help) sed -n '3,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) fail "未知参数: $argument" ;;
  esac
done

# A database named by the caller wins: it is how a local edition reuses a
# database it already has instead of starting another one.
external_dsn="${AGENTFLOW_MYSQL_DSN:-}"
[[ -f .env ]] || bash scripts/configure.sh
set -a
# shellcheck disable=SC1091
source .env
set +a
if [[ -n $external_dsn ]]; then AGENTFLOW_MYSQL_DSN="$external_dsn"; fi

port="${AGENTFLOW_HTTP_PORT:-8090}"
data="${LOOPFORGE_DATA_DIR:-$PWD/.data}/local"
workspaces="$data/workspaces"
log="$data/agentflow.log"
pidfile="$data/agentflow.pid"
binary=.local/bin/agentflow
url="http://127.0.0.1:$port"
mkdir -p "$data" "$workspaces"

running_pid() {
  local pid
  [[ -f $pidfile ]] || return 1
  pid="$(cat "$pidfile" 2>/dev/null || true)"
  [[ -n $pid ]] && kill -0 "$pid" 2>/dev/null || return 1
  printf '%s' "$pid"
}

# wait_for_http polls the one endpoint that answers before anyone signs in, so a
# slow first migration does not look like a failed start.
wait_for_http() {
  local attempt
  for attempt in $(seq 1 90); do
    if curl -fsS "$url/api/auth/setup" >/dev/null 2>&1; then return 0; fi
    if [[ -n ${1:-} ]] && ! kill -0 "$1" 2>/dev/null; then return 1; fi
    sleep 1
  done
  return 1
}

ensure_database() {
  # The caller's DSN means "this database exists, use it". A DSN that came from
  # .env points at the Compose MySQL this script is about to start, so it is not
  # the same thing.
  if [[ -n $external_dsn ]]; then
    printf 'Database: %s (given, the Compose MySQL stays untouched)\n' "$(printf '%s' "$external_dsn" | sed 's/:[^:@]*@/:***@/')"
    return 0
  fi
  command -v docker >/dev/null || fail '需要 Docker 运行数据库，或用 AGENTFLOW_MYSQL_DSN 指向一个已有的 MySQL。'
  select_docker_access || fail 'Docker 没有运行，或当前用户无法访问 Docker。'
  printf 'Starting the Compose MySQL…\n'
  docker_access compose up -d --wait mysql
  if [[ -z ${AGENTFLOW_MYSQL_DSN:-} ]]; then
    AGENTFLOW_MYSQL_DSN="agentflow:${LOOPFORGE_DB_PASSWORD}@tcp(127.0.0.1:${LOOPFORGE_MYSQL_PORT:-33315})/agentflow"
  fi
  export AGENTFLOW_MYSQL_DSN
}

# The tables come from the server's own migration on first start, so the starter
# catalog can only be applied once it is up. Repeating it is cheap and only adds
# what is missing; without it the project and SOP lists start empty.
ensure_catalog() {
  if [[ -n $external_dsn ]]; then
    printf 'Starter catalog: apply migrations/seed/*.sql to that database yourself (docs/local-mode.md).\n'
    return 0
  fi
  printf 'Applying the starter catalog…\n'
  if bash scripts/install-schema-extensions.sh >"$data/schema.log" 2>&1; then
    printf 'Starter catalog ready.\n'
  else
    printf '⚠ 起始目录没有应用（不影响已能使用的功能），详见 %s\n' "$data/schema.log"
  fi
}

print_ready() {
  local setup
  setup="$(curl -fsS "$url/api/auth/setup" 2>/dev/null || true)"
  printf '\nLoopForge 本地版已就绪：%s\n' "$url"
  case "$setup" in
    *'"needsSetup":true'*)
      printf '首次使用：打开上面的地址设置本机账号密码（只会建这一个账号，没有云端账号体系）。\n' ;;
    *'"needsSetup":false'*)
      printf '打开上面的地址登录；本机账号已存在。\n' ;;
    *)
      printf '打开上面的地址开始使用。\n' ;;
  esac
  printf '数据与工作区: %s\n日志: %s\n停止: bash scripts/start-local.sh stop\n' "$data" "$log"
}

start() {
  local pid
  if pid="$(running_pid)"; then
    printf '已经在运行 (pid %s)：%s\n' "$pid" "$url"
    return 0
  fi
  ensure_database
  [[ -x $binary ]] || bash scripts/build-local.sh
  # Something already answering on this port is not ours (there is no pid file),
  # and starting anyway would report that other instance as this one.
  if curl -fsS "$url/api/auth/setup" >/dev/null 2>&1; then
    fail "$url 已被另一个进程占用（不是本脚本启动的）。先停掉它，或用 AGENTFLOW_HTTP_PORT 换端口。"
  fi
  export AGENTFLOW_LOCAL_MODE=1
  export AGENTFLOW_DATA_DIR="$data"
  if [[ $foreground == 1 ]]; then
    exec "$binary" -addr "127.0.0.1:$port" -workspaces "$workspaces"
  fi
  printf 'Starting the local edition…\n'
  # setsid keeps it alive past this shell; nohup keeps it alive past the
  # terminal. The pid file is written by this shell, so stop can find it.
  setsid nohup "$binary" -addr "127.0.0.1:$port" -workspaces "$workspaces" >"$log" 2>&1 </dev/null &
  pid=$!
  printf '%s\n' "$pid" >"$pidfile"
  if ! wait_for_http "$pid"; then
    printf '\n✗ 启动失败，日志尾部：\n' >&2
    tail -20 "$log" >&2 || true
    rm -f "$pidfile"
    exit 1
  fi
  ensure_catalog
  print_ready
}

stop() {
  local pid
  if ! pid="$(running_pid)"; then
    rm -f "$pidfile"
    printf '没有在运行的本地版。\n'
    return 0
  fi
  kill "$pid" 2>/dev/null || true
  for _ in $(seq 1 20); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.5
  done
  kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null || true
  rm -f "$pidfile"
  printf '已停止 (pid %s)。\n' "$pid"
}

status() {
  local pid
  if pid="$(running_pid)"; then
    printf '运行中 (pid %s)  %s\n' "$pid" "$url"
  else
    printf '未运行。\n'
    return 1
  fi
  if curl -fsS "$url/api/auth/setup" >/dev/null 2>&1; then
    printf '接口正常应答。\n'
  else
    printf '接口尚未应答（可能仍在启动）。\n'
  fi
}

case "$command_name" in
  start) start ;;
  stop) stop ;;
  restart) stop; start ;;
  status) status ;;
  logs) tail -n 50 -f "$log" ;;
esac
