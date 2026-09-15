# KẾ HOẠCH TRIỂN KHAI: TRANG QUẢN LÝ VẬN HÀNH CHIẾN LƯỢC 1 (FUNDING ARBITRAGE PORTAL)

> **Cập nhật:** 2026-09-14  
> **Người lập:** Senior Project Manager & Tech Lead  
> **Áp dụng:** Binance Testnet / Demo (USDⓈ-M Futures + Spot)  
> **Mục tiêu:** Chuyển đổi các công cụ dòng lệnh phân mảnh (`cmd/brokercheck`, `cmd/execcheck`) thành một Web Portal quản trị giao dịch tập trung, an toàn, trực quan.

---

## 1. BỐI CẢNH & VỊ TRÍ TRONG HỆ THỐNG

### 1.1. Hiện trạng
- Giai đoạn 4 đã hoàn thành nghiệm thu 5/6 bước trên **Binance Testnet** (4.1 REST ký HMAC, 4.2 Lệnh Broker, 4.3 Sổ cái Paper, 4.4 Mở vị thế 2 chân delta-neutral, 4.5 Đóng vị thế qua mốc settle).
- Tất cả các thao tác trên hiện đang thực hiện thủ công qua terminal bằng `cmd/execcheck`.
- Tiến trình **Cổng 3.5 (PID 55475)** đang chạy liên tục tại cổng **8085**, mở file `data/scanner.db` suốt 14 ngày (đến hết 2026-09-26).

### 1.2. Ranh giới an toàn tuyệt đối (Safety Guardrails)
1. **Không can thiệp Cổng 3.5:** Tuyệt đối không sửa đổi mã nguồn `cmd/scanner`, không khởi động lại tiến trình, không migrate schema SQLite `data/scanner.db`.
2. **Chỉ chạy trên Testnet:** Portal chỉ chấp nhận các host testnet (`demo-fapi.binance.com` và `testnet.binance.vision`), từ chối mọi host mainnet.
3. **Chỉ bind Loopback:** Portal chỉ lắng nghe trên `127.0.0.1:8087`, không bind `0.0.0.0`.
4. **Cô lập dependency:** `cmd/execportal` không được phép import `cmd/scanner`. Việc liên kết `internal/broker` và `internal/execution` phải được khai báo minh bạch trong danh sách `allowed` của `internal/broker/boundary_test.go`.

---

## 2. KIẾN TRÚC & THÀNH PHẦN

Portal được đóng gói thành một command Go độc lập: `cmd/execportal/`.

```
cmd/execportal/
├── main.go            # Khởi tạo CLI, đọc env, khởi tạo client Binance, bind 127.0.0.1:8087
├── api.go             # REST handlers: /api/account, /api/positions, /api/open, /api/close, /api/reconcile, /api/funding
├── server.go          # HTTP server, routing, serve static UI, middleware CORS/Logging/Shutdown
├── guard_test.go      # Test an toàn: cô lập binary, testnet guard
└── ui/
    ├── index.html     # Giao diện Web Portal (Dark theme, dashboard tiles, modal xác nhận)
    ├── app.js         # Fetch state mỗi 3s, render dữ liệu, xử lý tương tác nút bấm
    └── style.css      # Vanilla CSS hiện đại, responsive, trực quan
```

---

## 3. ĐẶC TẢ REST API BACKEND

Mọi endpoint đều trả về JSON chuẩn, có trường `error_vi` rõ ràng nếu thất bại.

| Endpoint | Method | Chức năng | Input | Output chính |
|---|---|---|---|---|
| `/api/status` | `GET` | Kiểm tra trạng thái Portal | Không | `uptime_sec`, `hosts`, `mode: "testnet_demo"` |
| `/api/account` | `GET` | Đọc số dư ví & kết nối | Không | Số dư USDT, BTC ở Spot & Futures; `clock_skew_ms`; `weight_1m` |
| `/api/positions` | `GET` | Đọc vị thế 2 chân & Delta | `?symbol=BTCUSDT` | `spot_qty_coin`, `perp_qty_coin`, `delta_residual_coin`, `status` (`both_open`, `both_flat`, `unhedged`) |
| `/api/orders` | `GET` | Danh sách lệnh đang mở | `?symbol=BTCUSDT` | Mảng các lệnh đang active trên cả 2 sàn |
| `/api/open` | `POST` | Mở vị thế 2 chân Delta-Neutral | `{symbol, notional_quote, leg_order}` | Kết quả mở 2 chân, trượt giá đo được, `intent_id` sinh ra |
| `/api/close` | `POST` | Đóng toàn bộ 2 chân vị thế | `{symbol, intent_id}` | Kết quả đóng, PnL thực hiện, trượt giá |
| `/api/reconcile` | `POST` | Làm phẳng chân trần khẩn cấp | `{symbol}` | Lệnh cân bằng gửi đi, số lượng coin đã làm phẳng |
| `/api/funding` | `GET` | Đọc lịch sử nạp/trừ funding sàn | `?symbol=BTCUSDT` | Mảng các sự kiện `FUNDING_FEE` đọc từ `/fapi/v1/income` |

*Lưu ý xử lý đồng thời:* Các tác vụ ghi lệnh (`/api/open`, `/api/close`, `/api/reconcile`) bắt buộc có Mutex bảo vệ chống click đúp hoặc race condition.

---

## 4. ĐẶC TẢ GIAO DIỆN WEB (UI/UX)

- **Phong cách:** Dark mode cao cấp (`#0a0a0a`), font chữ monospace (`JetBrains Mono`, `Consolas`), thẻ badge cố định `[BINANCE TESTNET DEMO]` màu vàng nổi bật.
- **Khối Header:**
  - Trạng thái kết nối Spot & Futures (Ping, Skew ms, Weight sử dụng).
  - Thẻ số dư ví Spot USDT/BTC và Futures USDT.
- **Khối Monitor Vị Thế (Trọng tâm):**
  - Thẻ Spot Long song song với thẻ Futures Short.
  - Thanh trạng thái **Delta Residual**: Nếu lệch coin = 0 → badge xanh `DELTA-NEUTRAL (HEDGED)`; nếu lệch > 0 → cảnh báo đỏ `UNHEDGED DELTA RISK`.
- **Khối Bảng Điều Khiển (Control Pad):**
  - Form mở vị thế: Chọn Symbol, nhập Notional ($65 đến $50,000), chọn thứ tự chân (Tuần tự / Song song).
  - Nút `[MỞ VỊ THẾ 2 CHÂN]` (Bắt buộc bật Modal Popup xác nhận trước khi thực hiện).
  - Nút `[ĐÓNG VỊ THẾ 2 CHÂN]` và nút khẩn cấp màu đỏ `[LÀM PHẲNG HẾT (RECONCILE)]`.
- **Khối Lịch Sử Giao Dịch & Đối Soát Funding:**
  - Bảng danh sách các đợt mở/đóng, trượt giá thực tế, và bảng đối soát tiền funding nhận được từ Binance API.
- **Cơ chế cập nhật:** Tự động gọi `/api/account` và `/api/positions` mỗi 3 giây một lần.

---

## 5. KẾ HOẠCH KIỂM THỬ & NGHIỆM THU

### 5.1. Kiểm thử tự động (CI/Offline)
1. `go test -v ./cmd/execportal/...`: Chạy pass 100% không cần kết nối mạng.
2. `internal/broker/boundary_test.go`: Pass toàn bộ (đã khai báo `execportal` vào `allowed`).
3. `go list -deps ./cmd/execportal | grep "cmd/scanner"`: Phải trả về rỗng.

### 5.2. Nghiệm thu thực tế trên Binance Testnet
1. Khởi chạy `go run ./cmd/execportal -port 8087`.
2. Mở trình duyệt `http://127.0.0.1:8087`, kiểm tra số dư và trạng thái kết nối hiển thị chính xác.
3. Mở thử vị thế $65 BTCUSDT → UI cập nhật `both_open`, số coin 2 chân bằng nhau, Delta residual = 0.
4. Đóng thử vị thế → UI cập nhật `both_flat`, số coin về 0, hiển thị PnL thực tế.
5. Kiểm tra tiến trình Cổng 3.5 (PID 55475) vẫn tiếp tục ghi log bình thường, không suy suyển.

---

## 6. KẾT QUẢ TRIỂN KHAI (2026-09-14)

Đã làm và nghiệm thu trên Binance Testnet; số đo đầy đủ ở [PLAN.md — "Công cụ vận hành 4.5b"](PLAN.md) và quyết định **Q16** ở §7.1. Sáu tiêu chí của §5 đều đạt, kể cả mở/đóng $65 BTCUSDT **qua chính giao diện** (0 lỗi console, lệch 0, badge `DELTA-NEUTRAL (HEDGED)`, cửa sổ trần 375 ms) và cổng 3.5 không suy suyển.

### Chỗ khác bản kế hoạch, và vì sao

| Kế hoạch | Thực tế | Lý do |
|---|---|---|
| 8 endpoint | thêm `GET /api/intents` (lịch sử từ cache, trượt giá bps) và `GET /api/market` (luật sàn, cỡ nhỏ nhất gợi ý, mốc settle kế tiếp) | bảng lịch sử và gợi ý cỡ lệnh của §4 cần chúng |
| `status` ∈ `both_open` / `both_flat` / `unhedged` | thêm **`evidence_conflict`** và **`unknown`** | vị thế perp sàn báo khác số các ý định giải thích, hay một lệnh không đọc được, thì không được tô xanh — in cả hai số, không tự hoà giải |
| `spot_qty_coin` | là tổng lệnh CỦA CÁC Ý ĐỊNH đọc lại từ sàn; số dư ví nằm riêng ở `spot_base_balance_qty_coin` | ví testnet có sẵn 1 BTC: `số dư − |perp|` đo quỹ cấp sẵn chứ không đo phòng hộ |
| `POST /api/reconcile {symbol}` tự cân | mặc định **chạy thử** trả kế hoạch và `plan_digest`; gửi lệnh cần `{"apply": true, "plan_digest": …}` khớp kế hoạch đã xem | lệnh chỉ đi đúng thứ người vận hành đã xác nhận trong hộp thoại |
| `/api/close {symbol, intent_id}` | cỡ đóng lấy theo lệnh của chính ý định ở **chân perp short**, và portal giữ **MỘT vị thế mỗi symbol** | `execution.Close` chứng minh đóng xong bằng vị thế perp của cả tài khoản; hai ý định cùng symbol sẽ bị báo gỡ vị thế hỏng giả |
| middleware CORS | **không trả CORS**; mọi `/api/` cần header `X-Execportal-Action` (`read` cho GET), Host/Origin/Sec-Fetch-Site phải là chính portal; POST phải `application/json` | loopback chặn máy khác nhưng không chặn trang web khác trong trình duyệt (CSRF, DNS rebinding) |
| `-bind` cấm `0.0.0.0` | chỉ nhận **IP loopback** (`127.0.0.1`, `::1`); cổng 8082/8085/8086 bị từ chối | một tên máy có thể phân giải ra địa chỉ LAN |
| `GET /api/funding` | 7 ngày gần nhất; `income_qty_in_asset` kèm `asset`, đối soát theo ý định chỉ cộng dòng bằng quote | một dòng BNFCR không phải USDT |
| font JetBrains Mono | dùng khi máy có sẵn, không tải từ ngoài | trang chạy dưới CSP `'self'`; font tải hỏng là lỗi console |

### Việc còn nợ

Ghi đủ ở PLAN mục 4.5b. Mục chặn 4.6: client HTTP của `internal/broker` đi theo redirect, nên một 307 từ host testnet có thể mang API key sang host khác — **đã trả 2026-09-15** (`broker.ErrRedirectAttempted`, `broker.ErrHostNotPinned`; chi tiết ở PLAN 4.5b).

---

## 7. HỢP NHẤT THÀNH TRANG VẬN HÀNH BỐN TAB (2026-09-14, Q17)

Nhiệm vụ tiếp theo gộp ba giao diện (scanner 8085, sổ giấy 8086, cổng lệnh 8087) vào chính portal này: bốn tab Market Scanner, Execution Control, Paper Ledger, Crowding Reversal, thiết kế glassmorphism tối. Số đo, review và nợ ở [PLAN.md — "Công cụ vận hành 4.5c"](PLAN.md); quyết định **Q17** ở §7.1.

| Yêu cầu | Thực tế | Lý do |
|---|---|---|
| "Dùng Google Fonts" | Inter + JetBrains Mono của Google Fonts, **tải về và phục vụ từ binary** (subset latin, latin-ext, vietnamese; giấy phép OFL đi kèm) | CSP `'self'`: một trang đặt lệnh không nạp tài nguyên từ host khác |
| Lightweight Charts | 4.2.1 **đóng gói** (tarball npm khớp sha1/sha512 registry, sha256 ghim trong test), không CDN | ai phục vụ script đó thì bấm được nút lệnh |
| Tab Scanner "kế thừa `static/app.js`" | port sang `ui/js/scanner.js`; `static/` **không đổi** (đến 2026-09-15 — xem 7.1) | tiến trình cổng 3.5 phục vụ `static/` từ đĩa |
| Biểu đồ nến | nến 1 phút **dựng tại trang** từ snapshot giá 200 ms, ghi rõ; giữ cả đường giá theo sàn | scanner không phát nến; không bịa nến của sàn |
| Tab Crowding "theo dõi tỉ lệ long/short" | **snapshot fixture nghiên cứu** (365 ngày cuối, nến 4h), nhãn NGHIÊN CỨU + danh sách việc còn thiếu | ingestion là Bước 6.2, vẫn sau 3.5 và 3.4; binary giữ credential không được biết host mainnet |
| "Tổng số dư ví Spot + Futures" | Tổng **USDT** spot + futures, ghi "không quy đổi coin"; BTC riêng | cộng coin vào USDT cần một giá mà header không nên tự chọn |
| Header "trạng thái kết nối" | thêm chip phòng hộ **xấu nhất trên mọi symbol**, nhấp nháy ở mọi tab | chân trần không nên chỉ thấy được ở tab Execution |
| Portal lấy dữ liệu scanner qua client | relay trong package `feeds`, không giải mã; chỉ nối khi tab Scanner mở; tối đa 3 phiên, 20 phiên/phút | `internal/scanner` ghi broadcast đồng bộ, hạn 2 s mỗi client — client chậm làm cổng chậm |

Nợ mới chặn 4.6: thư viện biểu đồ và JS các tab feed chạy **cùng origin** với API đặt lệnh.

### 7.1. Giao diện chuyển vào `static/` (2026-09-15)

Người vận hành chuyển toàn bộ giao diện từ `cmd/execportal/ui/` ra `static/` ở gốc repo và xoá dashboard cũ (`static/app.js`, `static/index.html`), để repo chỉ còn một cây frontend. Chi tiết và nghiệm thu ở [PLAN.md — "Công cụ vận hành 4.5c"](PLAN.md).

| Yêu cầu | Thực tế | Lý do |
|---|---|---|
| "`//go:embed ../../static`" | `static/embed.go` là package `static` nhúng `index.html css js fonts vendor research`, chỉ export `FS()`; `cmd/execportal` import nó | Go không cho `go:embed` đi lên thư mục cha |
| Xoá dashboard cũ | đã xoá; hàng "`static/` không đổi" ở bảng trên hết hiệu lực | quyết định của người vận hành: một cây frontend |
| "Cổng 3.5 tiếp tục chạy bình thường" | tiến trình không bị đụng; **nhưng cổng 8085 giờ trả trang vận hành từ đĩa** — trang tìm `/api/status`, nhận 404 không có header portal, hiện một dòng chỉ đường tới 8087 và dừng, không mở WebSocket, không hỏi lại | `cmd/scanner` phục vụ `static/` từ đĩa; binary của cổng không được sửa khi đang chạy |
| — | thêm guard: package `static` không import gì ngoài `embed`/`io/fs`, không hàm nào ngoài `FS`, không package con, không file nào cạnh `embed.go`/`index.html` (go build link cả `.s`/`.syso`); cây nhúng phải bằng cây trên đĩa | package này được link vào binary giữ credential từ NGOÀI `cmd/execportal`, nơi các guard cũ không đọc tới |
