#!/bin/bash
# 停止日志查看系统（只停当前目录启动的实例）
cd "$(dirname "$0")" || exit 1

PID=""
[ -f logviewer.pid ] && PID=$(cat logviewer.pid)

if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
  HERE=$(pwd -P)
  CWD=$(readlink -f "/proc/$PID/cwd" 2>/dev/null)
  if [ "$CWD" != "$HERE" ]; then
    echo "PID $PID 的工作目录是 $CWD，不是当前目录($HERE)，拒绝操作"
    exit 1
  fi
  kill "$PID" 2>/dev/null
  sleep 1
  if kill -0 "$PID" 2>/dev/null; then
    echo "进程未响应，强制终止"
    kill -9 "$PID" 2>/dev/null
  fi
  rm -f logviewer.pid
  echo "logviewer 已停止 (PID: $PID)"
else
  rm -f logviewer.pid
  echo "logviewer 未在运行"
fi
