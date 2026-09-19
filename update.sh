#!/usr/bin/env bash
# Rebuild and update a local CPA Carpool Docker Compose installation.

set -euo pipefail

INSTALL_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
ENV_FILE="$INSTALL_DIR/.env"
DOWNLOAD_ARCHIVE=""
DOWNLOAD_SOURCE_DIR=""

cleanup() {
  if [[ -n "$DOWNLOAD_ARCHIVE" && -f "$DOWNLOAD_ARCHIVE" ]]; then
    rm -f "$DOWNLOAD_ARCHIVE"
  fi
  if [[ -n "$DOWNLOAD_SOURCE_DIR" && -d "$DOWNLOAD_SOURCE_DIR" ]]; then
    rm -rf "$DOWNLOAD_SOURCE_DIR"
  fi
}
trap cleanup EXIT

usage() {
  cat <<'USAGE'
用法：
  ./update.sh

下载最新的托管源码（如适用），重新编译并重建 CPA Carpool 容器。
USAGE
}

case "${1:-}" in
  '') ;;
  -h|--help) usage; exit 0 ;;
  *) echo "不支持参数：$1" >&2; usage >&2; exit 1 ;;
esac

command -v docker >/dev/null 2>&1 || { echo "未找到 docker，请先安装 Docker。" >&2; exit 1; }
docker compose version >/dev/null 2>&1 || {
  echo "未找到 Docker Compose 插件，请先安装 Docker Compose v2。" >&2
  exit 1
}
[[ -f "$INSTALL_DIR/docker-compose.yml" && -f "$INSTALL_DIR/docker-compose.cpa.yml" ]] || { echo "不是有效的 CPA Carpool 安装目录：$INSTALL_DIR" >&2; exit 1; }
[[ -f "$ENV_FILE" ]] || { echo "缺少 .env，请重新执行 install.sh。" >&2; exit 1; }

read_env() {
  awk -F= -v key="$1" '$1 == key { print substr($0, index($0, "=") + 1); exit }' "$ENV_FILE"
}

BUILD_CONTEXT=$(read_env CLI_PROXY_BUILD_CONTEXT)
SOURCE_MANAGED=$(read_env CLI_PROXY_SOURCE_MANAGED)
SOURCE_REPOSITORY=$(read_env CLI_PROXY_SOURCE_REPOSITORY)
SOURCE_REF=$(read_env CLI_PROXY_SOURCE_REF)

if [[ "$SOURCE_MANAGED" == "1" ]]; then
  [[ -n "$BUILD_CONTEXT" && "$BUILD_CONTEXT" != "/" ]] || { echo "源码目录配置无效。" >&2; exit 1; }
  SOURCE_REPOSITORY="${SOURCE_REPOSITORY:-https://github.com/qlqqs/CLIProxyAPI}"
  SOURCE_REF="${SOURCE_REF:-main}"
  command -v tar >/dev/null 2>&1 || { echo "未找到 tar，无法更新源码。" >&2; exit 1; }
  DOWNLOAD_ARCHIVE=$(mktemp)
  DOWNLOAD_SOURCE_DIR=$(mktemp -d)
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "${SOURCE_REPOSITORY}/archive/${SOURCE_REF}.tar.gz" -o "$DOWNLOAD_ARCHIVE"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$DOWNLOAD_ARCHIVE" "${SOURCE_REPOSITORY}/archive/${SOURCE_REF}.tar.gz"
  else
    echo "需要 curl 或 wget 才能更新源码。" >&2
    exit 1
  fi
  tar -xzf "$DOWNLOAD_ARCHIVE" --strip-components=1 -C "$DOWNLOAD_SOURCE_DIR"
  [[ -f "$DOWNLOAD_SOURCE_DIR/Dockerfile" && -f "$DOWNLOAD_SOURCE_DIR/go.mod" ]] || {
    echo "下载的源码不完整，无法更新。" >&2
    exit 1
  }
  rm -rf "$BUILD_CONTEXT"
  mv "$DOWNLOAD_SOURCE_DIR" "$BUILD_CONTEXT"
  DOWNLOAD_SOURCE_DIR=""
  rm -f "$DOWNLOAD_ARCHIVE"
  DOWNLOAD_ARCHIVE=""
fi

[[ -f "$BUILD_CONTEXT/Dockerfile" && -f "$BUILD_CONTEXT/go.mod" ]] || {
  echo "源码目录不存在或不完整：$BUILD_CONTEXT" >&2
  exit 1
}

printf '源码目录：%s\n正在重新编译并更新服务...\n' "$BUILD_CONTEXT"
docker compose --project-directory "$INSTALL_DIR" -f "$INSTALL_DIR/docker-compose.yml" -f "$INSTALL_DIR/docker-compose.cpa.yml" build

for container_name in cli-proxy-api cpa-carpool; do
  existing_container=$(docker ps -aq --filter "name=^/${container_name}$")
  container_workdir=""
  if [[ -n "$existing_container" ]]; then
    container_workdir=$(docker inspect --format '{{ index .Config.Labels "com.docker.compose.project.working_dir" }}' "$existing_container" 2>/dev/null || true)
  fi
  if [[ -n "$existing_container" && ( "$container_name" == "cli-proxy-api" || "$container_workdir" != "$INSTALL_DIR" ) ]]; then
    docker rm -f "$existing_container" >/dev/null
  fi
done

docker compose --project-directory "$INSTALL_DIR" -f "$INSTALL_DIR/docker-compose.yml" -f "$INSTALL_DIR/docker-compose.cpa.yml" up -d --remove-orphans --pull never
printf '更新完成。\n'
