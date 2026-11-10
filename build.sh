#!/usr/bin/env bash
# Shell Go 打包脚本：交叉编译 + 生成发布包
# 用法：
#   ./build.sh              打包 linux/amd64 与 linux/arm64
#   ./build.sh amd64        只打包 linux/amd64
#   ./build.sh local        只编译本机版本（用于调试）
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

APP="shell-go"
VERSION="$(cat VERSION 2>/dev/null || echo '1.0.0')"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo 'dev')"
BUILD_TIME="$(date '+%Y-%m-%dT%H:%M:%S')"
DIST="$ROOT/dist"
LDFLAGS="-s -w -X main.buildVersion=${VERSION} -X main.buildCommit=${COMMIT} -X 'main.buildTime=${BUILD_TIME}'"

c_ok()   { printf '\033[1;32m%s\033[0m\n' "$*"; }
c_info() { printf '\033[1;34m%s\033[0m\n' "$*"; }
c_warn() { printf '\033[1;33m%s\033[0m\n' "$*"; }
c_err()  { printf '\033[1;31m%s\033[0m\n' "$*" >&2; }

check_env() {
  command -v go >/dev/null 2>&1 || { c_err "未找到 go，请先安装 Go 1.22+"; exit 1; }
  c_info "Go: $(go version)"
}

# 生成发布目录
stage() {
  local target="$1" goarch="$2"
  local dir="$DIST/${APP}-${VERSION}-${target}"
  rm -rf "$dir"
  mkdir -p "$dir"

  c_info "编译 linux/${goarch} …"
  CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$dir/$APP" .

  cp config.example.yaml "$dir/config.example.yaml"
  cp README.md "$dir/README.md"
  cp CHANGELOG.md "$dir/CHANGELOG.md" 2>/dev/null || true
  cp deploy/install.sh "$dir/install.sh"
  cp deploy/shell-go.service "$dir/shell-go.service"
  cp -R docs "$dir/docs"
  chmod +x "$dir/install.sh" "$dir/$APP"
  echo "$VERSION ($COMMIT, $BUILD_TIME, linux/$goarch)" > "$dir/VERSION"

  # COPYFILE_DISABLE/--no-xattrs：macOS 打包会带上 com.apple.provenance 等扩展属性头，
  # Linux 上解包虽然能用，但会刷一屏 "Ignoring unknown extended header keyword" 警告。
  ( cd "$DIST" && COPYFILE_DISABLE=1 tar --no-xattrs -czf "${APP}-${VERSION}-${target}.tar.gz" "${APP}-${VERSION}-${target}" )
  rm -rf "$dir"
  c_ok "  → dist/${APP}-${VERSION}-${target}.tar.gz  ($(du -h "$DIST/${APP}-${VERSION}-${target}.tar.gz" | cut -f1))"
}

build_local() {
  c_info "编译本机版本 …"
  go build -trimpath -ldflags "$LDFLAGS" -o "$ROOT/$APP" .
  c_ok "  → ./$APP ($(du -h "$ROOT/$APP" | cut -f1))"
}

main() {
  local target="${1:-all}"
  check_env
  mkdir -p "$DIST"

  case "$target" in
    local)
      build_local
      ;;
    amd64)
      stage linux-amd64 amd64
      ;;
    arm64)
      stage linux-arm64 arm64
      ;;
    all)
      go vet ./... || c_warn "go vet 有告警，请留意"
      stage linux-amd64 amd64
      stage linux-arm64 arm64
      build_local
      ;;
    *)
      c_err "未知参数：$target（可选 local / amd64 / arm64 / all）"
      exit 1
      ;;
  esac

  echo
  c_ok "完成。产物目录：$DIST"
}

main "$@"
