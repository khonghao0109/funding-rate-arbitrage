#!/usr/bin/env bash
# tools/start-master.sh — Khởi chạy Master Command Center (Port 8080)
# Tích hợp toàn diện: Scanner (8085), Động cơ 1, Động cơ 2 (Bybit & Binance), Paper Ledger (8086)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

PORT=8080
LOG_DIR="$REPO_ROOT/.logs"
LOG_FILE="$LOG_DIR/master-portal.log"

mkdir -p "$LOG_DIR"

echo "============================================================"
echo "⚡ MASTER COMMAND CENTER — HỆ THỐNG GIAO DỊCH TRỌNG TÀI TỔNG LỰC"
echo "============================================================"

# 1. Kiểm tra Scanner (Port 8085)
if lsof -i :8085 -sTCP:LISTEN >/dev/null 2>&1; then
    echo "✔ Scanner (Port 8085): Đang hoạt động"
else
    echo "⚠ CẢNH BÁO: Scanner (Port 8085) chưa chạy. Các dữ liệu funding realtime có thể bị hạn chế."
fi

# 2. Kiểm tra Paper Ledger (Port 8086)
if lsof -i :8086 -sTCP:LISTEN >/dev/null 2>&1; then
    echo "✔ Paper Ledger (Port 8086): Đang hoạt động"
else
    echo "⚠ CẢNH BÁO: Paper Ledger (Port 8086) chưa chạy."
fi

# 3. Kiểm tra nếu Master Portal (Port 8080) đã chạy
if lsof -i :"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    EXISTING_PID=$(lsof -ti :"$PORT" -sTCP:LISTEN | head -n 1)
    echo "ℹ Master Command Center đã đang chạy tại PID $EXISTING_PID trên cổng $PORT."
    echo "🔗 Mở trình duyệt tại: http://localhost:$PORT"
    exit 0
fi

# 4. Đồng bộ các tệp ý định vị thế mở từ .paper/exec sang .paper/exec-master
mkdir -p "$REPO_ROOT/.paper/exec-master"
if [ -d "$REPO_ROOT/.paper/exec" ]; then
    cp -n "$REPO_ROOT/.paper/exec"/*.json "$REPO_ROOT/.paper/exec-master/" 2>/dev/null || true
fi

# 5. Khởi chạy Master Command Center
echo "🚀 Đang khởi chạy Master Command Center trên cổng $PORT..."
nohup go run ./cmd/execportal -master -port "$PORT" > "$LOG_FILE" 2>&1 &
MASTER_PID=$!
echo "Đã cấp tiến trình PID: $MASTER_PID (Log: $LOG_FILE)"

# 5. Chờ sẵn sàng
echo -n "Đang kết nối cổng $PORT"
SUCCESS=0
for i in {1..30}; do
    if curl -s -f -H "X-Execportal-Action: read" "http://127.0.0.1:$PORT/api/status" >/dev/null 2>&1; then
        SUCCESS=1
        break
    fi
    echo -n "."
    sleep 0.5
done
echo ""

if [ "$SUCCESS" -eq 1 ]; then
    echo "============================================================"
    echo "🎉 MASTER COMMAND CENTER ĐÃ SẴN SÀNG!"
    echo "🔗 TRUY CẬP NGAY: http://localhost:$PORT"
    echo "============================================================"
else
    echo "❌ Không thể khởi động Master Command Center sau 15 giây. Xem log tại: $LOG_FILE"
    exit 1
fi
