#!/usr/bin/env bash
# WorkBuddy Manager 一键部署脚本（在服务器上执行）
#   用法:  sudo bash deploy/install.sh
# 前置:  已存在 /opt/workbuddy2api（workbuddy2api 反代）并监听 7863
set -euo pipefail

APP_DIR="${APP_DIR:-/opt/workbuddy-manager}"
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PY="${PY:-/usr/bin/python3}"

echo "==> 部署目录: ${APP_DIR}"
mkdir -p "${APP_DIR}/data"

if [ "${SRC_DIR}" != "${APP_DIR}" ]; then
  echo "==> 同步代码到 ${APP_DIR}"
  for item in server deploy .env.example; do
    cp -r "${SRC_DIR}/${item}" "${APP_DIR}/" 2>/dev/null || true
  done
  mkdir -p "${APP_DIR}/web"
  if [ -d "${SRC_DIR}/web/out" ]; then
    cp -r "${SRC_DIR}/web/out" "${APP_DIR}/web/"
  else
    echo "!! 未找到 web/out，请先在本地执行:  cd web && npm install && npm run build:export"
  fi
fi

echo "==> 安装 Python 依赖"
"${PY}" -m pip install --break-system-packages -q -r "${APP_DIR}/server/requirements.txt"

echo "==> 注册 systemd 服务"
cp "${APP_DIR}/deploy/workbuddy-web.service" /etc/systemd/system/workbuddy-web.service
systemctl daemon-reload
systemctl enable --now workbuddy-web
sleep 2
systemctl status workbuddy-web --no-pager || true

echo
echo "==> 完成。首次启动的随机管理员密码见:"
echo "    journalctl -u workbuddy-web | grep -A3 '初始管理员'"
echo "    或设置 WB_ADMIN_PASSWORD 后重启服务。"
echo
echo "==> 访问 http://127.0.0.1:7864  （公网需在 1Panel 建反代并开启 HTTPS）"
