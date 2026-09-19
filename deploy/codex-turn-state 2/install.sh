#!/usr/bin/env bash
set -euo pipefail

PACKAGE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CPA_DIR="$(cd "${1:-$PACKAGE_DIR/..}" && pwd)"
CPA_CONTAINER="${CPA_CONTAINER:-cli-proxy-api}"
PLUGIN_DIR="${CLI_PROXY_PLUGIN_PATH:-$CPA_DIR/plugins}"
GO_IMAGE="${GO_IMAGE:-golang:1.26-bookworm}"

fail() { printf '%s\n' "$*" >&2; exit 1; }
command -v docker >/dev/null || fail '需要 Docker。'
docker info >/dev/null 2>&1 || fail 'Docker 不可用，请先启动 Docker。'

CONTAINER_RUNNING=false
if [[ "$(docker inspect --format '{{.State.Running}}' "$CPA_CONTAINER" 2>/dev/null || true)" == true ]]; then
  CONTAINER_RUNNING=true
  MACHINE="$(docker exec "$CPA_CONTAINER" uname -m)"
  MOUNT_DIR="$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/CLIProxyAPI/plugins"}}{{.Source}}{{end}}{{end}}' "$CPA_CONTAINER")"
  [[ -n "$MOUNT_DIR" ]] || fail '请先在 CPA 的 Compose volumes 中添加 ./plugins:/CLIProxyAPI/plugins，再执行 docker compose up -d。'
  PLUGIN_DIR="$MOUNT_DIR"
else
  [[ "$(uname -s)" == Linux ]] || fail '请在 Linux 服务器运行，或先启动 cli-proxy-api 容器。'
  MACHINE="$(uname -m)"
fi

case "$MACHINE" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) fail "不支持的服务器架构：$MACHINE" ;;
esac

case "$PLUGIN_DIR" in
  /*) ;;
  *) PLUGIN_DIR="$CPA_DIR/$PLUGIN_DIR" ;;
esac

mkdir -p "$PACKAGE_DIR/build/linux/$ARCH" "$PACKAGE_DIR/.cache/build" "$PACKAGE_DIR/.cache/mod"
printf '正在编译 linux/%s 插件…\n' "$ARCH"
docker run --rm --platform "linux/$ARCH" \
  --user "$(id -u):$(id -g)" \
  -v "$PACKAGE_DIR:/work" -w /work/go \
  -e CGO_ENABLED=1 -e GOFLAGS='-buildvcs=false -mod=readonly' \
  -e GOCACHE=/work/.cache/build -e GOMODCACHE=/work/.cache/mod \
  "$GO_IMAGE" \
  sh -ec "go test ./...; go build -buildmode=c-shared -o /work/build/linux/$ARCH/codex-turn-state.so ."

# Resolve libc dependencies using the exact running CPA image before installation.
if [[ "$CONTAINER_RUNNING" == true ]]; then
  CPA_IMAGE="$(docker inspect --format '{{.Image}}' "$CPA_CONTAINER")"
  if ! LDD_OUTPUT="$(docker run --rm --platform "linux/$ARCH" \
    -v "$PACKAGE_DIR/build/linux/$ARCH:/check:ro" --entrypoint ldd \
    "$CPA_IMAGE" /check/codex-turn-state.so 2>&1)"; then
    fail "插件与 CPA 镜像不兼容：$LDD_OUTPUT"
  fi
  [[ "$LDD_OUTPUT" != *'not found'* ]] || fail "插件依赖缺失：$LDD_OUTPUT"
fi

TARGET_DIR="$PLUGIN_DIR/linux/$ARCH"
STORE_DIR="$PLUGIN_DIR/codex-turn-state-store"
mkdir -p "$TARGET_DIR" "$STORE_DIR"
chmod 700 "$STORE_DIR"

# The stock CPA container runs as root; also support a non-root container user.
if [[ "$CONTAINER_RUNNING" == true ]]; then
  CONTAINER_UID="$(docker exec "$CPA_CONTAINER" id -u)"
  CONTAINER_GID="$(docker exec "$CPA_CONTAINER" id -g)"
  if [[ "$CONTAINER_UID" != 0 && "$CONTAINER_UID" != "$(id -u)" ]]; then
    chown "$CONTAINER_UID:$CONTAINER_GID" "$STORE_DIR" || fail '目录权限设置失败，请使用 sudo bash codex-turn-state/install.sh。'
  fi
fi

# Rename old libraries instead of overwriting an inode a live process may map.
BACKUP_SUFFIX="bak-$(date +%Y%m%d-%H%M%S)-$$"
shopt -s nullglob
for SEARCH_DIR in "$PLUGIN_DIR" "$PLUGIN_DIR/linux" "$TARGET_DIR"; do
  for OLD_LIBRARY in "$SEARCH_DIR"/codex-turn-state.so "$SEARCH_DIR"/codex-turn-state-v*.so; do
    [[ -f "$OLD_LIBRARY" ]] || continue
    mv "$OLD_LIBRARY" "$OLD_LIBRARY.$BACKUP_SUFFIX"
  done
done
install -m 755 "$PACKAGE_DIR/build/linux/$ARCH/codex-turn-state.so" "$TARGET_DIR/codex-turn-state.so.pending"
mv "$TARGET_DIR/codex-turn-state.so.pending" "$TARGET_DIR/codex-turn-state.so"

printf '\n已安装：%s\n' "$TARGET_DIR/codex-turn-state.so"
printf '下一步：合并 config.snippet.yaml 到 CPA 的 config.yaml，填写两把密钥，然后执行 docker compose restart cli-proxy-api。\n'
