#!/bin/bash
# 启动日志查看系统（使用 pidfile，避免与其他目录的同名进程混淆）
cd "$(dirname "$0")" || exit 1

if [ -f logviewer.pid ] && kill -0 "$(cat logviewer.pid)" 2>/dev/null; then
  echo "logviewer 已在运行中 (PID: $(cat logviewer.pid))"
  exit 1
fi

BIN=./logviewer
[ -x "$BIN" ] || { echo "找不到可执行文件: $BIN"; exit 1; }

CONFIG=${1:-config.yaml}
[ -f "$CONFIG" ] || { echo "找不到配置文件: $CONFIG"; exit 1; }

nohup "$BIN" -config "$CONFIG" > logviewer.log 2>&1 &
echo $! > logviewer.pid
sleep 0.6

if kill -0 "$(cat logviewer.pid)" 2>/dev/null; then
  PORT=$(awk '/^port:/{print $2}' "$CONFIG")
  echo "logviewer 已启动，PID: $(cat logviewer.pid)"
  echo "配置: $CONFIG"
  echo "访问地址: http://$(hostname -I | awk '{print $1}'):${PORT:-23344}"
else
  echo "启动失败，最近日志："
  tail -20 logviewer.log
  rm -f logviewer.pid
  exit 1
fi
