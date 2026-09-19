#!/usr/bin/env bash
# Install CPA Carpool by building the current fork's source with Docker Compose.

set -euo pipefail

DEFAULT_DIR="/opt/cpa-carpool"
SOURCE_REPOSITORY="${CLIPROXY_SOURCE_REPOSITORY:-https://github.com/qlqqs/CLIProxyAPI}"
SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"

usage() {
  cat <<'USAGE'
用法：
  ./install.sh [选项]

选项：
  --dir DIR       安装目录（默认：/opt/cpa-carpool）
  --ref REF       远程源码引用（默认：main；仅从 curl 执行时使用）
  -h, --help      显示帮助

脚本始终从源码构建本地镜像，不会拉取 CPA Carpool 的远程应用镜像。
USAGE
}

INSTALL_DIR="$DEFAULT_DIR"
REF="main"
DOWNLOAD_ARCHIVE=""
DOWNLOAD_SOURCE_DIR=""
SOURCE_MANAGED=0

cleanup() {
  if [[ -n "$DOWNLOAD_ARCHIVE" && -f "$DOWNLOAD_ARCHIVE" ]]; then
    rm -f "$DOWNLOAD_ARCHIVE"
  fi
  if [[ -n "$DOWNLOAD_SOURCE_DIR" && -d "$DOWNLOAD_SOURCE_DIR" ]]; then
    rm -rf "$DOWNLOAD_SOURCE_DIR"
  fi
}
trap cleanup EXIT

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dir)
      [[ $# -ge 2 ]] || { echo "缺少 --dir 参数" >&2; exit 1; }
      INSTALL_DIR=$2
      shift 2
      ;;
    --ref)
      [[ $# -ge 2 ]] || { echo "缺少 --ref 参数" >&2; exit 1; }
      REF=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "未知参数：$1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

command -v docker >/dev/null 2>&1 || { echo "未找到 docker，请先安装 Docker。" >&2; exit 1; }
docker compose version >/dev/null 2>&1 || {
  echo "未找到 Docker Compose 插件，请先安装 Docker Compose v2。" >&2
  exit 1
}

if ! mkdir -p "$INSTALL_DIR" "$INSTALL_DIR/auths" "$INSTALL_DIR/logs" "$INSTALL_DIR/plugins" "$INSTALL_DIR/data"; then
  echo "无法创建安装目录 $INSTALL_DIR，请使用 sudo 运行，或通过 --dir 指定可写目录。" >&2
  exit 1
fi

fetch_file() {
  local name=$1
  local destination=$2
  if [[ -f "$SCRIPT_DIR/$name" && "$SCRIPT_DIR" != "/dev"* ]]; then
    cp "$SCRIPT_DIR/$name" "$destination"
  elif command -v curl >/dev/null 2>&1; then
    curl -fsSL "${SOURCE_REPOSITORY}/raw/${REF}/${name}" -o "$destination"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$destination" "${SOURCE_REPOSITORY}/raw/${REF}/${name}"
  else
    echo "需要 curl 或 wget 才能下载 ${name}。" >&2
    exit 1
  fi
}

prepare_source() {
  if [[ -f "$SCRIPT_DIR/Dockerfile" && -f "$SCRIPT_DIR/go.mod" ]]; then
    SOURCE_DIR="$SCRIPT_DIR"
    return
  fi

  SOURCE_MANAGED=1
  SOURCE_DIR="$INSTALL_DIR/source"

  command -v tar >/dev/null 2>&1 || { echo "未找到 tar，无法下载源码。" >&2; exit 1; }
  local archive staging_dir
  DOWNLOAD_ARCHIVE=$(mktemp)
  archive="$DOWNLOAD_ARCHIVE"
  DOWNLOAD_SOURCE_DIR=$(mktemp -d)
  staging_dir="$DOWNLOAD_SOURCE_DIR"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "${SOURCE_REPOSITORY}/archive/${REF}.tar.gz" -o "$archive"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$archive" "${SOURCE_REPOSITORY}/archive/${REF}.tar.gz"
  else
    echo "需要 curl 或 wget 才能下载源码。" >&2
    exit 1
  fi
  mkdir -p "$SOURCE_DIR"
  tar -xzf "$archive" --strip-components=1 -C "$SOURCE_DIR"
  rm -f "$archive"
  DOWNLOAD_ARCHIVE=""
DOWNLOAD_SOURCE_DIR=""
SOURCE_MANAGED=0
  [[ -f "$SOURCE_DIR/Dockerfile" && -f "$SOURCE_DIR/go.mod" ]] || {
    echo "下载的源码不完整，无法构建。" >&2
    exit 1
  }
}

prepare_source

# Keep local deployment files independent from the source directory.
if [[ ! -f "$INSTALL_DIR/docker-compose.yml" ]]; then
  fetch_file docker-compose.yml "$INSTALL_DIR/docker-compose.yml"
fi
if [[ ! -f "$INSTALL_DIR/config.yaml" ]]; then
  fetch_file config.example.yaml "$INSTALL_DIR/config.yaml"
fi
fetch_file update.sh "$INSTALL_DIR/update.sh"
chmod +x "$INSTALL_DIR/update.sh"
cat > "$INSTALL_DIR/docker-compose.cpa.yml" <<'COMPOSE'
name: cpa-carpool

services:
  cli-proxy-api:
    image: cpa-carpool:local
    container_name: cpa-carpool
COMPOSE

if [[ ! -f "$INSTALL_DIR/.env" ]]; then
  printf 'COMPOSE_PROJECT_NAME=cpa-carpool\nCLI_PROXY_IMAGE=cpa-carpool:local\nCLI_PROXY_BUILD_CONTEXT=%s\nCLI_PROXY_SOURCE_MANAGED=%s\nCLI_PROXY_SOURCE_REPOSITORY=%s\nCLI_PROXY_SOURCE_REF=%s\n' "$SOURCE_DIR" "$SOURCE_MANAGED" "$SOURCE_REPOSITORY" "$REF" > "$INSTALL_DIR/.env"
else
  tmp_file=$(mktemp)
  awk -v context="$SOURCE_DIR" -v managed="$SOURCE_MANAGED" -v repository="$SOURCE_REPOSITORY" -v ref="$REF" '
    BEGIN { project_found = 0; image_found = 0; context_found = 0; managed_found = 0; repository_found = 0; ref_found = 0 }
    /^COMPOSE_PROJECT_NAME=/ { print "COMPOSE_PROJECT_NAME=cpa-carpool"; project_found = 1; next }
    /^CLI_PROXY_IMAGE=/ { print "CLI_PROXY_IMAGE=cpa-carpool:local"; image_found = 1; next }
    /^CLI_PROXY_BUILD_CONTEXT=/ { print "CLI_PROXY_BUILD_CONTEXT=" context; context_found = 1; next }
    /^CLI_PROXY_SOURCE_MANAGED=/ { print "CLI_PROXY_SOURCE_MANAGED=" managed; managed_found = 1; next }
    /^CLI_PROXY_SOURCE_REPOSITORY=/ { print "CLI_PROXY_SOURCE_REPOSITORY=" repository; repository_found = 1; next }
    /^CLI_PROXY_SOURCE_REF=/ { print "CLI_PROXY_SOURCE_REF=" ref; ref_found = 1; next }
    { print }
    END {
      if (!project_found) print "COMPOSE_PROJECT_NAME=cpa-carpool"
      if (!image_found) print "CLI_PROXY_IMAGE=cpa-carpool:local"
      if (!context_found) print "CLI_PROXY_BUILD_CONTEXT=" context
      if (!managed_found) print "CLI_PROXY_SOURCE_MANAGED=" managed
      if (!repository_found) print "CLI_PROXY_SOURCE_REPOSITORY=" repository
      if (!ref_found) print "CLI_PROXY_SOURCE_REF=" ref
    }
  ' "$INSTALL_DIR/.env" > "$tmp_file"
  mv "$tmp_file" "$INSTALL_DIR/.env"
fi

printf '安装目录：%s\n源码目录：%s\n' "$INSTALL_DIR" "$SOURCE_DIR"
printf '正在编译本地镜像并启动服务...\n'
docker compose --project-directory "$INSTALL_DIR" -f "$INSTALL_DIR/docker-compose.yml" -f "$INSTALL_DIR/docker-compose.cpa.yml" build

# Replace legacy or foreign containers that would conflict with this deployment.
for container_name in cli-proxy-api cpa-carpool; do
  existing_container=$(docker ps -aq --filter "name=^/${container_name}$")
  container_workdir=""
  if [[ -n "$existing_container" ]]; then
    container_workdir=$(docker inspect --format '{{ index .Config.Labels "com.docker.compose.project.working_dir" }}' "$existing_container" 2>/dev/null || true)
  fi
  if [[ -n "$existing_container" && ( "$container_name" == "cli-proxy-api" || "$container_workdir" != "$INSTALL_DIR" ) ]]; then
    printf '检测到冲突容器 %s，正在替换...\n' "$container_name"
    docker rm -f "$existing_container" >/dev/null
  fi
done

printf '正在初始化默认管理员账号...\n'
docker compose --project-directory "$INSTALL_DIR" -f "$INSTALL_DIR/docker-compose.yml" -f "$INSTALL_DIR/docker-compose.cpa.yml" stop cli-proxy-api >/dev/null 2>&1 || true
docker compose --project-directory "$INSTALL_DIR" -f "$INSTALL_DIR/docker-compose.yml" -f "$INSTALL_DIR/docker-compose.cpa.yml" run --rm --no-deps cli-proxy-api \
  ./CLIProxyAPI --config /CLIProxyAPI/config.yaml --carpool-bootstrap-random-admin admin

docker compose --project-directory "$INSTALL_DIR" -f "$INSTALL_DIR/docker-compose.yml" -f "$INSTALL_DIR/docker-compose.cpa.yml" up -d --remove-orphans --pull never
printf '\n安装完成。\n配置文件：%s/config.yaml\n查看日志：cd %q && docker compose -f docker-compose.yml -f docker-compose.cpa.yml logs -f\n' "$INSTALL_DIR" "$INSTALL_DIR"
