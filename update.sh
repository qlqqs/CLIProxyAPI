#!/usr/bin/env bash
# Rebuild and update a local CPA Carpool Docker Compose installation.

set -euo pipefail

INSTALL_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
ENV_FILE="$INSTALL_DIR/.env"

usage() {
  cat <<'USAGE'
用法：
  ./update.sh

重新编译源码并重建 CPA Carpool 容器，不会拉取远程 CLIProxyAPI 应用镜像。
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

BUILD_CONTEXT=$(awk -F= '$1 == "CLI_PROXY_BUILD_CONTEXT" { print substr($0, index($0, "=") + 1); exit }' "$ENV_FILE")
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
