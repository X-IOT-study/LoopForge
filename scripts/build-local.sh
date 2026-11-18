#!/usr/bin/env bash
# Build the single-user local edition from this checkout.
#
# Local mode runs on the host's own CLI logins and configuration and has no
# execution machines, so it needs the server (with the web UI embedded) and the
# stdio MCP server only — the runner and gate MCP binaries are platform pieces.
#
# Usage:
#   build-local.sh [output-dir]        default: .local/bin
#   SKIP_FRONTEND=1 build-local.sh     reuse the existing web/native-chat bundle
#   FORCE_FRONTEND=1 build-local.sh    rebuild the bundle even when it is fresh
#   GO=/path/to/go build-local.sh      use that toolchain
set -euo pipefail
cd "$(dirname "$0")/.."
output="${1:-$PWD/.local/bin}"
mkdir -p "$output"
output="$(cd "$output" && pwd)"

fail() { printf '\n✗ %s\n' "$1" >&2; exit 1; }

# go.mod pins a toolchain newer than some distributions ship, and Go refuses a
# module that asks for more than it is, so pick one that satisfies the pin.
required="$(awk '$1 == "go" { print $2; exit }' go.mod)"
[[ -n $required ]] || fail 'go.mod does not declare a Go version.'

version_ok() { # version_ok HAVE NEED, both dotted numbers
  local have="$1" need="$2" i a b
  local IFS=.
  local -a have_parts=($have) need_parts=($need)
  for i in 0 1 2; do
    a="${have_parts[i]:-0}"
    b="${need_parts[i]:-0}"
    [[ $a =~ ^[0-9]+$ ]] || a=0
    [[ $b =~ ^[0-9]+$ ]] || b=0
    if ((10#$a > 10#$b)); then return 0; fi
    if ((10#$a < 10#$b)); then return 1; fi
  done
  return 0
}

go_bin=''
seen=''
for candidate in "${GO:-}" "$(command -v go || true)" /usr/local/go/bin/go \
  "$HOME"/tools/go*/bin/go "$HOME"/sdk/go*/bin/go "$HOME"/.local/go/bin/go; do
  [[ -n $candidate && -x $candidate ]] || continue
  version="$("$candidate" version 2>/dev/null | awk '{ print $3 }')"
  [[ -n $version ]] || continue
  seen+="${version#go} (${candidate}) "
  if version_ok "${version#go}" "$required"; then
    go_bin="$candidate"
    break
  fi
done
[[ -n $go_bin ]] || fail "需要 Go ${required} 或更新的版本；已找到: ${seen:-无}。用 GO=/path/to/go 指定工具链。"

need_frontend() {
  [[ ${SKIP_FRONTEND:-0} == 1 ]] && return 1
  [[ ${FORCE_FRONTEND:-0} == 1 ]] && return 0
  [[ -f web/native-chat/index.html ]] || return 0
  # The bundle is only stale when the sources changed after it.
  [[ -n "$(find chatbot/src chatbot/index.html chatbot/package.json chatbot/package-lock.json \
    -newer web/native-chat/index.html -print -quit 2>/dev/null)" ]]
}

if need_frontend; then
  node -e 'if (+process.versions.node.split(".")[0] < 22) { console.error("Chatbot build requires Node >= 22"); process.exit(1) }' ||
    fail '需要 Node >= 22 才能构建前端。'
  if [[ ! -d chatbot/node_modules ]] || [[ chatbot/package-lock.json -nt chatbot/node_modules ]]; then
    printf 'Installing frontend dependencies…\n'
    npm --prefix chatbot ci --include=dev --no-audit --no-fund
  fi
  printf 'Building the web UI into web/native-chat…\n'
  npm --prefix chatbot run build
fi
[[ -f web/native-chat/index.html ]] || fail '缺少 web/native-chat；先不带 SKIP_FRONTEND 重新运行。'

flags=(-trimpath -ldflags=-s\ -w)
export CGO_ENABLED=0
printf 'Building with %s…\n' "$("$go_bin" version)"
"$go_bin" build "${flags[@]}" -o "$output/agentflow" ./cmd/agentflow
"$go_bin" build "${flags[@]}" -o "$output/agentflow-mcp" ./cmd/agentflow-mcp

printf '\nBuilt the local edition:\n'
for name in agentflow agentflow-mcp; do
  printf '  %s (%s)\n' "$output/$name" "$(du -h "$output/$name" | cut -f1)"
done
printf '\nNext: bash scripts/start-local.sh\n'
