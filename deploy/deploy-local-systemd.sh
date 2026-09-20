#!/usr/bin/env bash
# 本机 systemd 部署 sub2api 的唯一 sanctioned 路径(2026-09-20 定规)。
#
# 背景:2026-09-19 一次手工 `go build ./cmd/server` 部署漏掉 -tags=embed,
# API 一切正常而管理面板全 404,直到 operator 打开 http://192.168.18.30:8080/
# 才发现。backend/Makefile 的 build-embed 早就写着这条警告,但文档拦不住裸命令。
# 本脚本把三道结构防线固化:
#   1. 只走 make build-embed(缺前端产物直接拒绝);
#   2. 部署后健康门:服务 active + 面板在本机与 LAN 地址都 200(embed 门)
#      + (可选)API 容量端点 200;
#   3. 任一门失败:自动回滚上一个二进制、重启、复核,非零退出并大声报错。
#
# 用法: sudo deploy/deploy-local-systemd.sh [repo_dir] [--api-probe <pool_key>]
set -euo pipefail

REPO="${1:-/root/sub2api}"
shift || true
API_KEY=""
if [ "${1:-}" = "--api-probe" ]; then API_KEY="${2:?--api-probe needs a key}"; shift 2 || true; fi

BIN_DIR="/opt/sub2api"
SERVICE="sub2api"
STAMP="$(date +%Y%m%d-%H%M%S)"

[ "$(id -u)" -eq 0 ] || { echo "must run as root" >&2; exit 2; }
[ -d "$REPO/backend" ] || { echo "not a sub2api checkout: $REPO" >&2; exit 2; }

lan_ip() { ip -4 addr show scope global 2>/dev/null | awk '/inet /{print $2}' | head -1 | cut -d/ -f1; }
LAN_IP="$(lan_ip)"

health_gates() {
  local failures=0
  systemctl is-active --quiet "$SERVICE" || { echo "  GATE FAIL: service not active" >&2; failures=1; }
  for url in "http://127.0.0.1:8080/" ${LAN_IP:+"http://${LAN_IP}:8080/"}; do
    code="$(curl -s --max-time 8 -o /dev/null -w '%{http_code}' "$url" || echo 000)"
    if [ "$code" != "200" ]; then
      echo "  GATE FAIL: panel $url -> $code (embed miss returns 404 while API stays up)" >&2
      failures=1
    fi
  done
  if [ -n "$API_KEY" ]; then
    code="$(curl -s --max-time 8 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $API_KEY" \
      http://127.0.0.1:8080/v1/sub2api/pool-capacity || echo 000)"
    if [ "$code" != "200" ]; then
      echo "  GATE FAIL: pool-capacity -> $code" >&2; failures=1
    fi
  fi
  return $failures
}

echo "[1/5] build (make build-embed — refuses a panel-less binary)"
make -C "$REPO/backend" build-embed
[ -s "$REPO/backend/bin/server" ] || { echo "built binary missing" >&2; exit 2; }

echo "[2/5] backup current binary"
cp -a "$BIN_DIR/sub2api" "$BIN_DIR/sub2api.bak-$STAMP"

echo "[3/5] install + restart"
install -m 0755 "$REPO/backend/bin/server" "$BIN_DIR/sub2api"
systemctl restart "$SERVICE"
sleep 3

echo "[4/5] health gates"
if health_gates; then
  echo "[5/5] deployed OK; backup at $BIN_DIR/sub2api.bak-$STAMP"
  exit 0
fi

echo "HEALTH GATES FAILED — rolling back to $BIN_DIR/sub2api.bak-$STAMP" >&2
install -m 0755 "$BIN_DIR/sub2api.bak-$STAMP" "$BIN_DIR/sub2api"
systemctl restart "$SERVICE"
sleep 3
if health_gates; then
  echo "rollback restored the previous binary; deploy REJECTED — inspect build above" >&2
else
  echo "ROLLBACK ALSO UNHEALTHY — service needs manual attention now" >&2
fi
exit 1
