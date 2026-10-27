#!/bin/bash
# 重启日志追踪系统（先 stop 再 start，只作用于本目录启动的实例）
# 用法: ./restart.sh [配置文件]，默认 config.yaml
cd "$(dirname "$0")" || exit 1

CONFIG=${1:-config.yaml}

# stop.sh 在 pid 归属校验不通过时会返回非 0，此时不能继续启动
if ! ./stop.sh; then
  echo "停止失败，已中止重启"
  exit 1
fi

./start.sh "$CONFIG"
