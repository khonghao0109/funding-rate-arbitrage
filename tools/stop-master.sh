#!/usr/bin/env bash
# tools/stop-master.sh — Dừng Master Command Center (Port 8080) một cách an toàn

set -euo pipefail

PORT=8080

if ! lsof -i :"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "ℹ Master Command Center (Port $PORT) hiện không chạy."
    exit 0
fi

PID=$(lsof -ti :"$PORT" -sTCP:LISTEN | head -n 1)
echo "🛑 Đang dừng Master Command Center tại PID $PID (Port $PORT)..."
kill -TERM "$PID" || true

# Chờ giải phóng cổng
for i in {1..10}; do
    if ! lsof -i :"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
        echo "✔ Đã dừng Master Command Center và giải phóng cổng $PORT."
        exit 0
    fi
    sleep 0.5
done

# Buộc dừng nếu cần
if lsof -i :"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "⚠ Buộc dừng PID $PID..."
    kill -KILL "$PID" || true
    echo "✔ Đã buộc giải phóng cổng $PORT."
fi
