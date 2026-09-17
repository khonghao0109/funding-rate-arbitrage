# KẾ HOẠCH KẾT NỐI VÀ CẤU HÌNH BYBIT BROKER (BYBIT V5 UTA BROKER PLAN)

> **Cập nhật:** 2026-09-17  
> **Áp dụng:** Bước 4.5i — Bybit Testnet (`api-testnet.bybit.com`) & Bybit Demo Trading (`api-demo.bybit.com`)  
> **Mục tiêu:** Xây dựng adapter thực thi lệnh có ký chuẩn Bybit V5 Unified Trading Account (UTA), phục vụ chiến lược Arbitrage chéo sàn (Cross-Exchange Cash & Carry Funding Arbitrage) giữa Binance và Bybit.

---

## 1. TỔNG QUAN & BỐI CẢNH

### 1.1. Vì sao chọn Bybit làm sàn thứ 2?
1. **Thanh khoản phái sinh Top 2 toàn cầu:** Khối lượng giao dịch và độ sâu sổ lệnh USDT-Perpetual của Bybit bám sát Binance, đảm bảo khớp lệnh quy mô lớn mà trượt giá (slippage) cực thấp.
2. **Độ lệch Funding Rate hấp dẫn:** Thống kê từ dữ liệu 3 năm (`scanner.db`) cho thấy tỷ lệ lệch Funding Rate giữa Binance và Bybit thường xuyên đạt từ **15% đến 45% APR**, tạo ra các cơ hội Arbitrage chéo sàn có tỷ suất sinh lời vượt trội so với chỉ đánh nội bộ một sàn.
3. **Kiến trúc Bybit V5 hiện đại:** API V5 hợp nhất cả Spot, Linear Perpetual (USDT/USDC) và Inverse vào chung một cơ chế quản lý vốn Unified Trading Account (UTA).

### 1.2. Hai môi trường thử nghiệm của Bybit
Bybit cung cấp 2 giải pháp thử nghiệm an toàn:
* **Môi trường 1: Bybit Testnet độc lập (`https://api-testnet.bybit.com`):**
  - Đăng ký tài khoản tại [testnet.bybit.com](https://testnet.bybit.com) (không cần KYC).
  - Nhận ngay **50,000 USDT ảo** qua nút Faucet trên giao diện web.
  - Phục vụ kiểm thử logic kết nối, xử lý lỗi mạng, đặt/hủy lệnh.
* **Môi trường 2: Bybit Demo Trading trên Mainnet (`https://api-demo.bybit.com`):**
  - Chạy trực tiếp từ tài khoản Bybit chính thức (chuyển sang chế độ "Demo Trading").
  - Được cấp **50,000 USDT demo**, nhưng sử dụng **100% SỔ LỆNH THẬT và FUNDING RATE THẬT của Mainnet**.
  - Nhận tiền funding ròng thực tế vào số dư ví demo tại các mốc 00:00, 08:00, 16:00 UTC.

---

## 2. KIẾN TRÚC KẾT NỐI VÀ XÁC THỰC API V5

### 2.1. Host Pinning & Bảo vệ an toàn (`internal/broker/hosts.go`)
Nhằm tuân thủ nguyên tắc số 5 (không bao giờ chạm Mainnet thật trước Bước 4.6), danh sách host hợp lệ của Bybit được ghim cứng:
```go
const (
    BybitFuturesTestnetHost = "api-testnet.bybit.com"
    BybitFuturesDemoHost    = "api-demo.bybit.com"
)
```
Mọi lời gọi ra ngoài 2 domain trên đều bị từ chối ngay lập tức tại `broker.NewClient` (`broker.ErrHostNotPinned`).

### 2.2. Chuẩn ký HMAC-SHA256 Header-based
Khác với Binance (truyền signature trong query/body), Bybit V5 truyền toàn bộ thông tin xác thực qua HTTP Headers:
* `X-BAPI-API-KEY`: Bybit API Key
* `X-BAPI-TIMESTAMP`: Thời gian Unix tính bằng mili-giây (`time.Now().UnixMilli() + clockOffsetMs`)
* `X-BAPI-RECV-WINDOW`: Cửa sổ nhận lệnh (mặc định `5000` ms)
* `X-BAPI-SIGN`: Chữ ký HMAC-SHA256:
  $$\text{Sign} = \text{HMAC\_SHA256}(\text{timestamp} + \text{apiKey} + \text{recvWindow} + \text{payload}, \text{apiSecret})$$
  - Đối với request `GET`: `payload` là query string (ví dụ: `category=linear&symbol=BTCUSDT`).
  - Đối với request `POST`: `payload` là chuỗi JSON body (ví dụ: `{"category":"linear","symbol":"BTCUSDT",...}`).

### 2.3. Đồng bộ đồng hồ sàn (Clock Synchronization)
- Endpoint: `GET /v5/market/time` (không cần ký).
- Cơ chế: Đo thời gian gửi và nhận, tính `offset = serverTime - (sendTime + recvTime)/2`.
- Kiểm soát an toàn: Nếu `|offset| >= recvWindow`, bot từ chối ký lệnh để tránh lỗi `10002 Request expired`.

---

## 3. ĐẶC TẢ CẤU HÌNH (CONFIGURATION)

### 3.1. Biến môi trường (`.env`)
```bash
# Bybit Broker Credentials
BYBIT_API_KEY=your_bybit_testnet_api_key_here
BYBIT_API_SECRET=your_bybit_testnet_api_secret_here
BYBIT_TESTNET=true
BYBIT_MODE=testnet # "testnet" (api-testnet.bybit.com) hoặc "demo" (api-demo.bybit.com)
```

### 3.2. Quản trị Secret an toàn
- Sử dụng struct `broker.Secret` đã kiểm chứng tại Bước 4.1:
  - Tự động in `[redacted]` khi format `%v`, `%s`, `%+v`.
  - Từ chối serialization JSON (`MarshalJSON` trả về error).
  - Không bao giờ log key hoặc secret ra console / file log.

---

## 4. BỘ ENDPOINT BYBIT V5 CẦN TRIỂN KHAI

| Thao tác | HTTP Method & Path | Ý nghĩa trong hệ thống |
| :--- | :--- | :--- |
| **Đồng bộ giờ** | `GET /v5/market/time` | Lấy timestamp chính xác của sàn |
| **Đọc số dư** | `GET /v5/account/wallet-balance?accountType=UNIFIED` | Đọc số dư USDT khả dụng của tài khoản UTA |
| **Đọc vị thế** | `GET /v5/position/list?category=linear&symbol={symbol}` | Lấy size Short/Long, entry price, liquidation price |
| **Đặt lệnh Market** | `POST /v5/order/create` | Mở/đóng chân Perp với `timeInForce: "IOC"` |
| **Hủy lệnh** | `POST /v5/order/cancel` | Hủy khẩn cấp khi gặp sự cố trượt giá |
| **Quy chuẩn mã** | `GET /v5/market/instruments-info?category=linear` | Lấy `stepSize`, `tickSize`, `minOrderQty` |

---

## 5. CÔNG CỤ XÁC MINH: `cmd/bybitcheck`

Xây dựng công cụ CLI độc lập `cmd/bybitcheck/main.go` (tương tự `cmd/brokercheck` của Binance) để kiểm tra kết nối:
1. Gửi request đọc giờ server $\rightarrow$ đo độ lệch (ping ms, offset ms).
2. Gửi request có ký đọc số dư ví Unified $\rightarrow$ in ra danh sách tài sản (USDT, USDC, BTC...).
3. Kiểm tra quyền của API Key:
   - ✅ `canTrade: true` (Đạt).
   - ⚠️ `canWithdraw: true` (Cảnh báo: Yêu cầu người vận hành tắt quyền rút tiền).
4. **Không bao giờ in secret hoặc số dư thực tế lên màn hình** (chỉ in mã HTTP, latency và tên tài sản).

---

## 6. THỰC THI CHÉO SÀN DELTA-NEUTRAL (CROSS-EXCHANGE ARBITRAGE)

Khi cả Binance và Bybit đều có kết nối Broker:
1. **Khớp khối lượng Coin 2 sàn:**
   - Đọc `stepSize` của Binance Spot và `stepSize` của Bybit Perp.
   - Tính khối lượng chung hợp lệ: $Qty = \min(Qty_{\text{Binance}}, Qty_{\text{Bybit}})$ đã làm tròn xuống theo bước nhảy lớn hơn.
   - Đảm bảo sai số khối lượng giữa 2 chân $\le 5\%$.
2. **Quy trình vào lệnh 2 chân (Two-Leg Entry):**
   - Chân 1 (Spot Binance): Đặt lệnh Mua Market.
   - Chân 2 (Perp Bybit): Đặt lệnh Bán Short Market.
   - Nếu Chân 2 thất bại: Lập tức kích hoạt cơ chế Unwind bán ngược Chân 1 trong vòng $< 300\text{ ms}$ để không bao giờ để tài khoản bị hở rủi ro giá (unhedged).

---

## 7. TIÊU CHÍ NGHIỆM THU BƯỚC 4.5i

| # | Tiêu chí | Phương pháp kiểm tra | Trạng thái mục tiêu |
|---|---|---|---|
| 1 | Unit tests adapter Bybit | `go test -count=1 -race ./internal/broker/bybit/...` | 100% Pass, 0 data race |
| 2 | Chạy kiểm tra API Testnet | `go run ./cmd/bybitcheck` | Trả về HTTP 200, đo lệch giờ $< 500\text{ ms}$ |
| 3 | Thử nghiệm 2 chân chéo sàn | `go run ./cmd/execcheck -exchange=cross` | 10/10 lần mở/đóng delta-neutral trên testnet thành công |
| 4 | Rào chắn an toàn Mainnet | Test AST và `hosts.go` | Từ chối 100% domain mainnet khi chưa xong Bước 4.6 |
