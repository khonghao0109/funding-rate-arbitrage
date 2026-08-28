# KẾ HOẠCH PHÁT TRIỂN — Crypto Arbitrage Scanner → Trading Bot

> **Cập nhật:** 2026-08-28
> **Trạng thái tổng thể:** Giai đoạn 0 hoàn thành ~90%. Sản phẩm hiện tại là **scanner đọc-only**, chưa phải bot giao dịch.
> **Mục tiêu cuối:** Bot Funding Rate Arbitrage vận hành ổn định, sau đó mở rộng sang các chiến lược phức tạp hơn.
> **Tài liệu kèm theo:** [WORKFLOW.md](WORKFLOW.md) — quy trình code & review · [DATA-REQUIREMENTS.md](DATA-REQUIREMENTS.md) — dữ liệu cần từ sàn · [CONVENTIONS.md](CONVENTIONS.md) — quy ước mã nguồn · [../CLAUDE.md](../CLAUDE.md) — tổng quan cho AI.
>
> **Mỗi bước dưới đây được thực hiện theo đúng vòng lặp 9 pha trong [WORKFLOW.md](WORKFLOW.md), kết thúc bằng một commit duy nhất.**

---

## MỤC LỤC

1. [Hiện trạng dự án](#1-hiện-trạng-dự-án)
2. [Nguyên tắc phát triển](#2-nguyên-tắc-phát-triển)
3. [Bảng tổng hợp các giai đoạn](#3-bảng-tổng-hợp-các-giai-đoạn)
4. [Chi tiết từng giai đoạn](#4-chi-tiết-từng-giai-đoạn)
5. [Kiến trúc mục tiêu](#5-kiến-trúc-mục-tiêu)
6. [Sổ rủi ro](#6-sổ-rủi-ro)
7. [Quyết định cần chốt](#7-quyết-định-cần-chốt)
8. [Đối chiếu với tài liệu 5 chiến lược](#8-đối-chiếu-với-tài-liệu-5-chiến-lược)

---

## 1. HIỆN TRẠNG DỰ ÁN

### 1.1. Thông số

| Hạng mục | Giá trị |
|---|---|
| Ngôn ngữ | Go 1.23.5 |
| Dependency | `gorilla/websocket` v1.5.3, `joho/godotenv` v1.5.1 |
| Tổng dòng code Go | 2.144 dòng (`main.go` 387 + `exchanges/` 1.757) |
| Frontend | Vanilla JS 925 dòng + HTML 682 dòng, TradingView Lightweight Charts |
| Số nguồn dữ liệu | 10 (7 futures + 2 spot + 1 oracle) |
| Cặp giao dịch | BTCUSDT, ETHUSDT, XRPUSDT, SOLUSDT (hardcode tại `main.go:352`) |
| Test coverage | **0%** — không có file `_test.go` nào |
| Persistence | **Không có** — toàn bộ state nằm trong RAM |

### 1.2. Đã có gì

| # | Thành phần | File | Trạng thái |
|---|---|---|---|
| 1 | Connector Binance Futures + Spot | `exchanges/binance.go` | ✅ Hoạt động |
| 2 | Connector Bybit Futures + Spot | `exchanges/bybit.go` | ✅ Hoạt động |
| 3 | Connector OKX Futures | `exchanges/okx.go` | ✅ Hoạt động |
| 4 | Connector Gate.io Futures | `exchanges/gate.go` | ✅ Hoạt động |
| 5 | Connector Kraken Futures | `exchanges/kraken.go` | ✅ Hoạt động |
| 6 | Connector Hyperliquid (DEX) Futures | `exchanges/hyperliquid.go` | ✅ Hoạt động |
| 7 | Connector Paradex Futures | `exchanges/paradex.go` | ✅ Hoạt động |
| 8 | Pyth oracle price (SSE) | `exchanges/pyth.go` | ✅ Hoạt động |
| 9 | Kiểu dữ liệu chuẩn hoá | `exchanges/types.go` | ✅ `PriceData`, `OrderbookData`, `TradeData` |
| 10 | Mid-price `(bid+ask)/2` | `main.go:63` | ✅ |
| 11 | Ma trận spread pairwise | `main.go:213-236` | ✅ |
| 12 | Cảnh báo cơ hội + throttle 10s | `main.go:137-165` | ✅ |
| 13 | WebSocket broadcast tới frontend | `main.go:245-315` | ✅ |
| 14 | Dashboard real-time + biểu đồ | `static/app.js` | ✅ |
| 15 | Auto-reconnect từng sàn | tất cả connector | ⚠️ Sleep cố định, chưa có backoff |

### 1.3. Chưa có gì (khoảng trống chặn đường)

| # | Thiếu | Ảnh hưởng | Chặn giai đoạn |
|---|---|---|---|
| 1 | **Dữ liệu funding rate** — `grep -rin "funding" exchanges/` → 0 kết quả | Không có tín hiệu cốt lõi của chiến lược | GĐ 2 |
| 2 | **Persistence / database** | Restart mất sạch dữ liệu, không backtest được | GĐ 2 |
| 3 | **REST client + HMAC signing** — cả repo chỉ có 1 lời gọi HTTP (`pyth.go:77`) | Không thể đặt lệnh | GĐ 4 |
| 4 | **Mô hình phí** — không có dòng code nào tính phí (README đã sửa lại cho đúng, nay ghi rõ số hiển thị là spread thô) | Tín hiệu lợi nhuận là lợi nhuận thô, không phải ròng | GĐ 1 |
| 5 | **Khái niệm vị thế / margin / PnL** | Không quản trị được rủi ro | GĐ 5 |
| 6 | **Lọc dữ liệu cũ (staleness)** — `Timestamp` bị vứt bỏ tại `main.go:63-72` | Sàn rớt kết nối → giá cũ vẫn sinh tín hiệu giả | GĐ 1 |
| 7 | **Tách bạch spot ↔ perp trong logic** — `main.go:107-135` lấy min/max trên toàn bộ source | Sinh "cơ hội" không thực thi được | GĐ 1 |
| 8 | **Test tự động** | Không an toàn khi động đến tiền thật | GĐ 1 |
| 9 | **Cấu hình** — symbol, ngưỡng, sàn đều hardcode | Không vận hành linh hoạt được | GĐ 1 |
| 10 | **Alert ra ngoài** (Telegram/Discord) | Phải ngồi nhìn màn hình | GĐ 3 |
| 11 | **Instrument metadata** (`stepSize`, `tickSize`, `minNotional`, contract size) | Không tính được size delta-neutral đúng — hai chân lệch nhau ngay lệnh đầu | GĐ 2 |
| 12 | **Bảng ánh xạ spot↔perp có xác thực** | Nguy cơ mở vị thế lệch coin | GĐ 2 |

---

## 2. NGUYÊN TẮC PHÁT TRIỂN

1. **Không nhảy cóc.** Mỗi giai đoạn phải đạt đủ tiêu chí nghiệm thu trước khi sang giai đoạn sau.
2. **Đọc trước, ghi sau.** Mọi module chạm đến tiền thật chỉ được bật sau khi đã chạy chế độ mô phỏng (dry-run) tối thiểu 2 tuần.
3. **Luôn tính lợi nhuận ròng.** Mọi con số hiển thị phải đã trừ phí maker/taker, phí funding, slippage ước tính.
4. **Dữ liệu cũ là dữ liệu sai.** Mọi giá quá `N` giây phải bị loại khỏi tính toán, không được im lặng dùng tiếp.
5. **Credential tách biệt.** API key nằm trong package riêng, không bao giờ log, quyền hạn tối thiểu (bật trade, **tắt withdraw**).
6. **Có test mới có tiền.** Mọi logic tính toán tài chính phải có unit test trước khi dùng vốn thật.

---

## 3. BẢNG TỔNG HỢP CÁC GIAI ĐOẠN

**Tổng: 9 giai đoạn (GĐ 0 → GĐ 8), 40 bước.**

| GĐ | Tên | Số bước | Thời gian | Trạng thái | Kết quả bàn giao |
|---|---|---|---|---|---|
| **0** | Nền tảng scanner | 5 | — | ✅ **90% xong** | Scanner real-time 10 nguồn |
| **1** | Củng cố lõi (Hardening) | 6 | 2–3 tuần | ⬜ Chưa bắt đầu | Scanner đáng tin, có test, có phí |
| **2** | Funding Rate Monitor | 7 | 4–5 tuần | ⬜ Chưa bắt đầu | Thu thập + lưu funding rate 24/7 |
| **3** | Signal, Alert & Backtest | 5 | 3–4 tuần | ⬜ Chưa bắt đầu | Tín hiệu có kiểm chứng lịch sử |
| **4** | Execution Engine | 6 | 6–8 tuần | ⬜ Chưa bắt đầu | Bot đặt lệnh được (vốn nhỏ) |
| **5** | Risk & Vận hành | 5 | 4–6 tuần | ⬜ Chưa bắt đầu | Bot chạy production 24/7 |
| **6** | Basis Trade | 4 | 4–6 tuần | ⬜ Chưa bắt đầu | Bot hỗ trợ 2 chiến lược |
| **7** | CEX-DEX Arbitrage | 1 (phác thảo) | 3–6 tháng | 🔒 Khoá | — |
| **8** | Cross-Chain / Statistical | 1 (phác thảo) | 12+ tháng | 🔒 Khoá | — |

**Tổng thời gian tới bot Funding Rate chạy production (GĐ 1→5): khoảng 5 – 6,5 tháng.**

---

## 4. CHI TIẾT TỪNG GIAI ĐOẠN

---

### GIAI ĐOẠN 0 — NỀN TẢNG SCANNER ✅ *(đã hoàn thành ~90%)*

**Mục tiêu:** Có luồng dữ liệu giá real-time đa sàn, chuẩn hoá, hiển thị được.

| Bước | Nội dung | Trạng thái |
|---|---|---|
| 0.1 | Dựng khung Go + WebSocket server + static frontend | ✅ |
| 0.2 | Viết connector cho 7 sàn futures | ✅ |
| 0.3 | Tích hợp 2 nguồn spot (Binance, Bybit) | ✅ |
| 0.4 | Chuyển từ last-price sang mid-price `(bid+ask)/2` | ✅ |
| 0.5 | Ma trận spread + dashboard + biểu đồ | ✅ |

> Hai mục trong `PLAN.md` ở thư mục gốc đều đã xong — file đó nên được xoá hoặc thay bằng file này.

**Còn nợ 10%:** không có test, không có config, không có xử lý staleness → chuyển sang GĐ 1.

---

### GIAI ĐOẠN 1 — CỦNG CỐ LÕI (HARDENING)

**Mục tiêu:** Biến scanner từ "chạy được" thành "tin được". Đây là giai đoạn **bắt buộc** trước khi thêm bất kỳ tính năng mới nào.
**Thời gian:** 2–3 tuần · **6 bước**

#### Bước 1.1 — Lọc dữ liệu cũ (staleness filter)
- Lưu `Timestamp` cùng giá thay vì vứt bỏ (`main.go:63-72` hiện chỉ giữ `Price`).
- Đổi `map[string]float64` → `map[string]PricePoint{Price, Timestamp, ReceivedAt}`.
- Loại khỏi mọi phép tính nếu `now - ReceivedAt > staleThreshold` (mặc định 5s).
- Hiển thị trạng thái STALE / DISCONNECTED trên UI thay vì im lặng.
- **Nghiệm thu:** ngắt mạng 1 sàn → sàn đó biến mất khỏi ma trận trong ≤ 5s, không sinh cảnh báo giả.

#### Bước 1.2 — Tách bạch Spot ↔ Perpetual
- Thêm trường `MarketType` (`spot` / `perp` / `future` / `oracle`) vào struct thay vì suy ra từ hậu tố chuỗi.
- Chia làm 3 phép tính riêng biệt, không trộn lẫn:
  - **Cross-exchange spread**: perp ↔ perp, hoặc spot ↔ spot (thực thi được)
  - **Basis**: spot ↔ perp *cùng sàn* (nền tảng cho GĐ 2)
  - **Oracle deviation**: Pyth ↔ sàn (chỉ tham chiếu, không giao dịch)
- Sửa `checkArbitrage` (`main.go:107-135`) hiện đang lấy min/max trên toàn bộ source.
- **Nghiệm thu:** UI hiển thị 3 khối riêng, không còn cặp "mua spot sàn A / bán perp sàn B".

#### Bước 1.3 — Mô hình phí
- Tạo `internal/fees/` chứa bảng phí maker/taker theo từng sàn và từng loại thị trường.
- Mọi `ProfitPct` hiển thị đổi thành **lợi nhuận ròng** = spread thô − phí vào − phí ra.
- Ghi rõ trên UI: đây là ước tính, chưa gồm slippage.
- **Nghiệm thu:** có unit test cho hàm tính phí; UI hiển thị lợi nhuận ròng và bỏ được cảnh báo "spread thô" trong README.

#### Bước 1.4 — Cấu hình hoá
- Chuyển symbol, danh sách sàn, ngưỡng cảnh báo, `staleThreshold` từ hardcode sang `config.yaml` (hoặc env).
- **Nghiệm thu:** thêm 1 cặp mới không cần sửa code Go.

#### Bước 1.5 — Kết nối bền bỉ
- Exponential backoff (2s → 4s → 8s → … → tối đa 60s) thay cho `time.Sleep` cố định.
- Ping/pong keepalive + `SetReadDeadline` cho từng connector.
- Metric per-sàn: uptime, số lần reconnect, độ trễ message cuối.
- **Nghiệm thu:** chạy liên tục 72h không cần can thiệp; log rõ sàn nào rớt lúc nào.

#### Bước 1.6 — Bộ test đầu tiên
- Unit test cho: tính mid-price, tính phí, tính spread, staleness filter.
- Golden test: nạp message JSON mẫu của từng sàn → khẳng định parse ra đúng struct.
- **Nghiệm thu:** `go test ./...` xanh; coverage ≥ 60% ở phần logic tính toán.

> **Nợ kỹ thuật ghi nhận, chưa xử lý ở GĐ này:** `broadcastSpreads` (`main.go:170`) chạy ma trận O(n²) + `WriteJSON` tới mọi client trên *mỗi* tick giá. Chấp nhận được ở quy mô hiện tại; xử lý ở GĐ 5 nếu thành nút thắt.

---

### GIAI ĐOẠN 2 — FUNDING RATE MONITOR

**Mục tiêu:** Thu thập và lưu trữ funding rate 24/7 — nguyên liệu bắt buộc của chiến lược.
**Thời gian:** 4–5 tuần · **7 bước**
**📄 Chi tiết dữ liệu:** [DATA-REQUIREMENTS.md](DATA-REQUIREMENTS.md)

> **Cập nhật sau khảo sát 7 sàn (2026-08-28):** giai đoạn này tăng từ 5 lên 7 bước. Lý do: khảo sát cho thấy funding rate **không đồng nhất giữa các sàn** ở mức phá vỡ thiết kế struct ban đầu (OKX đảo ngược ngữ nghĩa `next`, Kraken trả giá trị tuyệt đối thay vì tỷ lệ, ba sàn dùng ba đơn vị khác nhau cho chu kỳ, Paradex không có mốc funding rời rạc). Đồng thời instrument registry và bảng ánh xạ spot↔perp được kéo từ GĐ 4 lên đây, vì không có chúng thì không tính được size delta-neutral để backtest cho đúng.

#### Bước 2.1 — Script xác minh field (làm TRƯỚC khi viết struct) 🔍
- Script một lần: đọc funding rate BTC từ cả 7 sàn, in cạnh nhau, đối chiếu với số hiển thị trên web từng sàn.
- Mục đích: phát hiện sai đơn vị / sai field **trước khi** nó đi vào logic tính toán.
- Xác minh cụ thể 4 điểm còn nghi vấn: OKX `fundingTime` vs `nextFundingTime`; Gate `funding_interval` đơn vị giây; Paradex mô hình liên tục; Kraken `funding_rate` tuyệt đối vs `relative_funding_rate`.
- **Nghiệm thu:** 7 con số khớp với web sàn. Sàn nào không khớp → tìm ra nguyên nhân trước khi đi tiếp.

#### Bước 2.2 — Thiết kế `FundingData` + refactor `Feeds`
- Struct đầy đủ tại [DATA-REQUIREMENTS.md §4](DATA-REQUIREMENTS.md#4-thiết-kế-fundingdata). Ba yêu cầu bắt buộc:
  1. `FundingModel` enum (`discrete` / `continuous`) — Paradex không có mốc funding.
  2. Giữ **cả** `RawRate` (debug) **lẫn** rate chuẩn hoá (`RatePerInterval`, `RatePer8h`, `APRAnnualized`).
  3. `IntervalSec` — chuẩn hoá về **giây ngay tại tầng connector**. Tầng signal không bao giờ thấy phút/giờ/giây lẫn lộn.
- Gom 3 channel thành `Feeds{Price, Orderbook, Trade, Funding}` thay vì thêm tham số thứ 4 vào 10 call site.
- **Nghiệm thu:** unit test chuyển đổi đơn vị cho cả 7 sàn từ payload mẫu.

#### Bước 2.3 — Instrument registry
- Tải `exchangeInfo` (spot + futures) 1 lần/ngày, cache: `tickSize`, `stepSize`, `minNotional`, `status`, contract size/multiplier, leverage brackets.
- Contract size khác nhau: OKX `ctVal × ctMult`, Gate `quanto_multiplier`, Kraken theo spec — các sàn này đặt lệnh theo **số contract**, không theo coin.
- Hàm tính size delta-neutral: làm tròn **xuống** theo `stepSize` **lớn hơn** của hai chân, kiểm `minNotional` của **cả hai** rồi mới đặt.
- **Nghiệm thu:** cho notional bất kỳ → ra size 2 chân hợp lệ ở mọi sàn, hoặc từ chối có lý do rõ ràng.

#### Bước 2.4 — Bảng ánh xạ spot ↔ perp
- Dựng **tự động** từ `exchangeInfo`, thay `switch` hardcode hiện tại trong từng connector.
- Xác thực hai chiều; cặp không ghép được → **từ chối, không đoán**.
- Chú ý Kraken `PF_XBTUSD` quote là **USD** không phải USDT → không hedge thẳng bằng spot USDT.
- 🐛 Sửa luôn bug tiềm ẩn `coin := symbol[:3]` tại [hyperliquid.go:58](../exchanges/hyperliquid.go#L58) — hiện đúng chỉ vì cả 4 symbol có base 3 ký tự; thêm `DOGEUSDT` sẽ subscribe sai coin mà không báo lỗi.
- **Nghiệm thu:** thêm 1 cặp mới → bảng tự dựng đúng trên mọi sàn hỗ trợ, tự loại sàn không hỗ trợ.

#### Bước 2.5 — Thu thập funding rate
- **WebSocket** (ưu tiên): Binance `@markPrice@1s`, Bybit `tickers`, OKX `funding-rate`, Gate `futures.tickers`, Kraken `ticker`, Hyperliquid `activeAssetCtx`.
- ⚠️ Bybit ticker là **snapshot + delta** — field vắng mặt nghĩa là *chưa đổi*, phải merge vào cache, không ghi đè.
- ⚠️ Binance `fundingInfo` **chỉ trả symbol lệch mặc định** → mặc định 8h rồi ghi đè, không đọc ngược lại.
- **Nghiệm thu:** funding rate 4 cặp × 7 sàn realtime, khớp web sàn, đã chuẩn hoá về `RatePer8h` so sánh được chéo sàn.

#### Bước 2.6 — Persistence (SQLite)
- `funding_history(exchange, symbol, raw_rate, rate_per_8h, interval_sec, funding_time, rate_type, recorded_at)`.
- `instruments` (snapshot hằng ngày — để biết `stepSize` đã đổi lúc nào), `price_snapshots` (lấy mẫu 1–5s).
- Backfill lịch sử 6–12 tháng qua REST; lọc `rateType = Special` khi backtest.
- Lưu trữ: 12 tháng funding, 3 tháng price snapshot.
- **Nghiệm thu:** restart không mất dữ liệu; truy vấn được funding 30 ngày; có đủ corpus cho backtest GĐ 3.

#### Bước 2.7 — Dashboard funding
- Bảng funding hiện tại theo sàn × cặp, **quy về cùng đơn vị 8h** để so sánh công bằng, tô màu theo mức hấp dẫn.
- Đếm ngược mốc funding kế tiếp (ẩn với sàn `continuous`).
- Biểu đồ lịch sử funding + basis spot↔perp cùng sàn (thành quả Bước 1.2).
- ⚠️ Nếu mở rộng quá ~20 symbol, tầng broadcast phải sửa trước — xem [§7.3](#73-ngưỡng-mở-rộng-của-tầng-broadcast).
- **Nghiệm thu:** nhìn dashboard biết ngay nên vào cặp nào, sàn nào — và biết con số đang so sánh là cùng đơn vị.

---

### GIAI ĐOẠN 3 — SIGNAL, ALERT & BACKTEST

**Mục tiêu:** Từ dữ liệu thô ra tín hiệu có kiểm chứng, chưa đặt lệnh.
**Thời gian:** 3–4 tuần · **5 bước**

#### Bước 3.1 — Máy tính APY
- `APY = FundingRate × (24 / IntervalHours) × 365` — chuẩn hoá theo chu kỳ thật của từng sàn.
- Tính **APY ròng** = APY thô − phí vào/ra khấu hao theo thời gian giữ dự kiến.
- **Nghiệm thu:** có unit test đối chiếu với tính tay trên nhiều chu kỳ khác nhau.

#### Bước 3.2 — Sinh tín hiệu
- Điều kiện vào lệnh: funding rate > ngưỡng **VÀ** duy trì qua N chu kỳ **VÀ** APY ròng > sàn tối thiểu **VÀ** thanh khoản đủ.
- Điều kiện thoát: funding chuyển âm, APY ròng < ngưỡng, hoặc basis giãn bất thường.
- **Nghiệm thu:** tín hiệu ghi log đầy đủ lý do vào/ra.

#### Bước 3.3 — Backtest engine
- **Viết bằng Go, import trực tiếp `internal/strategy`** — không viết lại luật vào/ra (quyết định Q8, §7.1). Backtest và production phải chạy cùng một đoạn code, nếu không thì Bước 3.5 mất giá trị chẩn đoán.
- Chạy lại logic tín hiệu trên dữ liệu lịch sử của Bước 2.4.
- Funding là **sự kiện rời rạc**: đếm số mốc settle đã đi qua, không nhân APY với thời gian nắm giữ. Lọc `rateType = Special`.
- Báo cáo: tổng lợi nhuận, APR thực tế, max drawdown, số lần đảo chiều funding, tỉ lệ chu kỳ có lãi. Ghi ra SQLite/CSV để phân tích ngoài.
- Quét tham số chạy song song bằng goroutine.
- **Nghiệm thu:** có báo cáo backtest 6 tháng cho ít nhất 2 cặp; engine dùng đúng hàm tín hiệu mà production sẽ dùng (kiểm bằng cách đọc import).

#### Bước 3.4 — Alert ra ngoài
- Telegram bot (ưu tiên) / Discord webhook.
- Nội dung alert: cặp, sàn, funding rate, APY ròng, mốc funding tiếp theo, vốn đề xuất.
- Có throttle, tránh spam (tái sử dụng cơ chế `lastOpportunity` tại `main.go:141-152`).
- **Nghiệm thu:** nhận được alert trên điện thoại, đúng và không lặp.

#### Bước 3.5 — Cổng quyết định 🚦
- Chạy hệ thống ở chế độ chỉ-alert tối thiểu **2 tuần liên tục**.
- Ghi nhật ký thủ công: nếu vào lệnh theo mọi tín hiệu thì kết quả sẽ ra sao.
- **Nghiệm thu:** kết quả mô phỏng khớp với backtest trong sai số chấp nhận được. **Không khớp → quay lại Bước 3.2, không được sang GĐ 4.**

---

### GIAI ĐOẠN 4 — EXECUTION ENGINE

> ⚠️ **Đây là ranh giới rủi ro thật sự:** từ "đọc dữ liệu công khai" sang "giữ credential và di chuyển tiền". Không rút ngắn giai đoạn này.

**Mục tiêu:** Bot tự đặt và đóng vị thế delta-neutral với vốn nhỏ.
**Thời gian:** 6–8 tuần · **6 bước**

#### Bước 4.1 — Hạ tầng REST có ký
- Package riêng `internal/broker/`, tách hoàn toàn khỏi `exchanges/` (đọc-only).
- HMAC-SHA256 signing, xử lý `recvWindow`, đồng bộ đồng hồ với server sàn.
- Rate limiter theo weight của từng sàn.
- API key nạp từ env/secret store, **không bao giờ log**, quyền bật trade / **tắt withdraw**.
- **Nghiệm thu:** gọi được endpoint đọc số dư trên testnet; test đảm bảo key không lọt vào log.

#### Bước 4.2 — Trừu tượng hoá lệnh
- Interface chung: `PlaceOrder`, `CancelOrder`, `GetPosition`, `GetBalance`.
- Hiện thực cho **1 sàn duy nhất trước** (đề xuất Binance — tài liệu tốt nhất, có testnet).
- Xử lý làm tròn theo `stepSize` / `tickSize` / `minNotional` của từng cặp.
- **Nghiệm thu:** đặt và huỷ được lệnh trên Binance testnet.

#### Bước 4.3 — Chế độ Paper Trading
- Cùng logic execution nhưng ghi vào sổ ảo thay vì gửi lên sàn.
- Mô phỏng slippage và phí thực tế.
- **Nghiệm thu:** chạy paper 2 tuần, PnL ảo bám sát kỳ vọng backtest.

#### Bước 4.4 — Mở vị thế delta-neutral
- Đặt đồng thời Spot Long + Perp Short cùng notional.
- Tính chính xác khối lượng để `|Delta| ≈ 0` sau khi làm tròn.
- **Xử lý khớp lệnh một phần** — rủi ro lớn nhất của bước này: nếu 1 chân khớp còn chân kia không, bot đang **trần (unhedged)**. Bắt buộc có logic rollback/hedge khẩn cấp.
- **Nghiệm thu:** test tình huống bơm lỗi (chân 2 thất bại) → bot tự đóng chân 1 trong vài giây.

#### Bước 4.5 — Đóng vị thế
- Đóng cả hai chân khi tín hiệu thoát kích hoạt.
- Ghi nhận PnL thực tế: funding thu được − phí − slippage.
- **Nghiệm thu:** vòng đời mở→giữ→đóng hoàn chỉnh trên testnet.

#### Bước 4.6 — Chạy thật vốn tối thiểu 🚦
- Vốn thật **$200–$500**, 1 cặp (BTCUSDT), 1 sàn.
- Chạy tối thiểu 4 tuần, đối chiếu từng chu kỳ funding với sổ sách bot.
- **Nghiệm thu:** PnL thực tế khớp sổ bot sai số < 5%. Không khớp → dừng, tìm nguyên nhân.

---

### GIAI ĐOẠN 5 — RISK MANAGEMENT & VẬN HÀNH

**Mục tiêu:** Bot chạy production 24/7 an toàn, mở rộng vốn có kiểm soát.
**Thời gian:** 4–6 tuần · **5 bước**

#### Bước 5.1 — Giám sát margin & thanh lý
- Theo dõi liên tục: margin ratio, giá thanh lý, khoảng cách an toàn.
- Cảnh báo phân cấp: 🟡 chú ý → 🟠 giảm vị thế → 🔴 đóng khẩn cấp.
- **Nghiệm thu:** mô phỏng biến động giá mạnh → bot phản ứng đúng ngưỡng.

#### Bước 5.2 — Rebalance tự động
- Khi delta lệch quá ngưỡng (do giá biến động, do phí), tự cân lại.
- Cân nhắc chi phí rebalance so với mức lệch — không rebalance vì những khoản lệch nhỏ.

#### Bước 5.3 — Kill switch & khôi phục trạng thái
- Nút dừng khẩn cấp: đóng toàn bộ vị thế, ngừng nhận tín hiệu mới.
- Khôi phục trạng thái sau khi crash: đọc lại vị thế thật từ sàn, không tin sổ nội bộ.
- **Nghiệm thu:** `kill -9` giữa lúc đang giữ vị thế → khởi động lại nhận diện đúng vị thế đang mở.

#### Bước 5.4 — Phân bổ vốn & giới hạn
- Giới hạn cứng: vốn tối đa/cặp, vốn tối đa/sàn, đòn bẩy tối đa, số vị thế đồng thời.
- Không bao giờ all-in vào một sàn (rủi ro đối tác).

#### Bước 5.5 — Vận hành production
- Deploy VPS gần vùng máy chủ sàn, systemd/Docker, auto-restart.
- Báo cáo PnL hằng ngày qua Telegram.
- Tối ưu tầng broadcast nếu đã thành nút thắt — throttle `broadcastSpreads`, client đăng ký symbol. Ngưỡng và thứ tự sửa ở [§7.3](#73-ngưỡng-mở-rộng-của-tầng-broadcast).
- **Nghiệm thu:** chạy 30 ngày không cần can thiệp thủ công.

**🎯 Đến đây: hoàn thành Bot Funding Rate Arbitrage. Kỳ vọng 5–15%/năm.**

---

### GIAI ĐOẠN 6 — BASIS TRADE

**Mục tiêu:** Thêm chiến lược thứ hai, đa dạng hoá nguồn thu, tái dùng ~70% hạ tầng.
**Thời gian:** 4–6 tuần · **4 bước**

| Bước | Nội dung |
|---|---|
| 6.1 | Thu thập giá futures có đáo hạn (tuần/tháng/quý) — thêm `MarketType: future` + `ExpiryDate` |
| 6.2 | Tính basis & lợi suất annualized theo thời gian còn lại đến đáo hạn |
| 6.3 | Mở rộng execution xử lý hợp đồng có đáo hạn + **tự động rollover** trước ngày đáo hạn |
| 6.4 | Portfolio manager: phân bổ vốn giữa Funding Rate và Basis theo APY ròng kỳ vọng |

**Kỳ vọng sau GĐ 6: 8–20%/năm.**

---

### GIAI ĐOẠN 7 — CEX-DEX ARBITRAGE 🔒 *(khoá — chỉ mở sau GĐ 6)*

Yêu cầu năng lực hoàn toàn mới: Web3 wallet, AMM, gas optimization, phòng vệ MEV.
Không lập kế hoạch chi tiết ở thời điểm này — sẽ viết tài liệu riêng khi GĐ 6 hoàn tất.
Ước lượng 3–6 tháng. Kỳ vọng 20–50%/năm, rủi ro cao hơn hẳn.

---

### GIAI ĐOẠN 8 — CROSS-CHAIN / STATISTICAL 🔒 *(khoá)*

Cross-chain: rủi ro cầu nối là rủi ro mất vốn toàn phần, không phải rủi ro thị trường.
Statistical: cần nền toán định lượng (cointegration, VECM/GARCH, ML/DL) — 18–36 tháng.
Cả hai chỉ nên cân nhắc khi các giai đoạn trước đã sinh lợi ổn định và có vốn dư.

---

## 5. KIẾN TRÚC MỤC TIÊU

```
crypto-futures-arbitrage-scanner/
├── main.go                    # entrypoint, wiring
├── config.yaml                # [GĐ 1.4] symbol, sàn, ngưỡng
├── CLAUDE.md                  # tổng quan cho AI agent
├── docs/
│   ├── PLAN.md
│   ├── WORKFLOW.md
│   ├── DATA-REQUIREMENTS.md
│   └── CONVENTIONS.md
├── exchanges/                 # ĐỌC-ONLY, không chứa credential
│   ├── types.go               # + FundingData, MarketType, Feeds  [GĐ 2.1]
│   ├── binance.go … pyth.go
│   └── funding/               # [GĐ 2.2] thu thập funding rate
├── internal/
│   ├── fees/                  # [GĐ 1.3] bảng phí theo sàn
│   ├── instruments/           # [GĐ 2.3] registry + ánh xạ spot↔perp
│   ├── store/                 # [GĐ 2.3] SQLite, funding history
│   ├── strategy/              # [GĐ 3.2] sinh tín hiệu + APR (tên tránh đụng os/signal)
│   ├── backtest/              # [GĐ 3.3]
│   ├── notify/                # [GĐ 3.4] Telegram/Discord
│   ├── broker/                # [GĐ 4.1] ⚠️ CÓ CREDENTIAL — tách biệt tuyệt đối
│   ├── execution/             # [GĐ 4.4] mở/đóng vị thế delta-neutral
│   └── risk/                  # [GĐ 5] margin, kill switch, giới hạn
└── static/                    # dashboard
```

**Ranh giới quan trọng:** `exchanges/` không bao giờ được import bất kỳ package `internal/` nào. Dữ liệu công khai và credential phải nằm hai phía khác nhau của ranh giới này. Luật phụ thuộc đầy đủ ở [CONVENTIONS.md §12.1](CONVENTIONS.md#121-luật-phụ-thuộc-bắt-buộc-kiểm-tra-khi-review) — vi phạm là lỗi chặn merge.

Các package `internal/` hiện đã tạo, mỗi package có `doc.go` nêu trách nhiệm và ranh giới. Đọc `doc.go` trước khi thêm code vào package đó.

---

## 6. SỔ RỦI RO

| # | Rủi ro | Mức | Giai đoạn xử lý | Biện pháp |
|---|---|---|---|---|
| R1 | Funding rate đảo chiều dương → âm | Cao | GĐ 3.2, 5.1 | Điều kiện thoát tự động, giám sát mỗi chu kỳ |
| R2 | Khớp lệnh một phần → vị thế trần (unhedged) | **Rất cao** | GĐ 4.4 | Rollback tự động, test bơm lỗi bắt buộc |
| R3 | Thanh lý chân futures dù đã delta-neutral | Cao | GĐ 5.1 | Đòn bẩy thấp, buffer margin, cảnh báo phân cấp |
| R4 | Phí ăn hết lợi nhuận | Cao | GĐ 1.3 | Mô hình phí, chỉ hiển thị lợi nhuận ròng |
| R5 | Rò rỉ API key | **Rất cao** | GĐ 4.1 | Tắt quyền withdraw, không log, secret store, IP whitelist |
| R6 | Rủi ro đối tác (sàn sập / đóng băng rút) | Trung bình | GĐ 5.4 | Không tập trung vốn vào 1 sàn |
| R7 | Bot crash khi đang giữ vị thế | Cao | GĐ 5.3 | Khôi phục trạng thái từ sàn, kill switch |
| R8 | Dữ liệu cũ sinh tín hiệu sai | Trung bình | GĐ 1.1 | Staleness filter |
| R9 | Sai chu kỳ funding giữa các sàn (1h vs 8h vs liên tục) | **Cao** | GĐ 2.1, 2.2 | `IntervalSec` chuẩn hoá tại connector; script xác minh trước khi code |
| R10 | Sai field/đơn vị funding (Kraken tuyệt đối, OKX lệch một kỳ) | **Cao** | GĐ 2.1 | Đối chiếu 7 sàn với web sàn ở Bước 2.1 |
| R11 | Ghép sai cặp spot↔perp → vị thế lệch coin | **Rất cao** | GĐ 2.4 | Bảng ánh xạ tự dựng + xác thực hai chiều, từ chối khi không ghép được |
| R12 | Làm tròn 2 chân theo 2 `stepSize` khác nhau → delta ≠ 0 | Cao | GĐ 2.3 | Làm tròn theo `stepSize` lớn hơn, kiểm `minNotional` cả hai |

---

## 7. QUYẾT ĐỊNH

### 7.1. Đã chốt

| # | Quyết định | Chốt ngày |
|---|---|---|
| **Q7** | **Backend dùng Go cho toàn bộ** — WS, REST, registry, signal, execution, risk. Không thêm ngôn ngữ vào đường đi của tiền | 2026-08-28 |
| **Q8** | **Engine backtest viết bằng Go, dùng chung `internal/strategy` với production.** Python chỉ đọc kết quả để phân tích | 2026-08-28 |
| **Q9** | **Không dùng CCXT.** Giữ connector tự viết | 2026-08-28 |
| **Q10** | **Giữ frontend vanilla JS.** Sửa backend broadcast trước, không đổi framework | 2026-08-28 |

#### Q7 — Vì sao Go cho cả REST

REST và WS chia sẻ **cùng bộ kiểu dữ liệu và cùng tầng chuẩn hoá đơn vị**: `fundingInfo` (REST) quyết định `IntervalSec` mà `markPrice` (WS) dùng; instrument registry (REST) quyết định `stepSize` mà execution dùng. Tách ngôn ngữ nghĩa là viết logic chuẩn hoá Kraken/OKX **hai lần** — đúng chỗ mà [DATA-REQUIREMENTS.md §3](DATA-REQUIREMENTS.md#3-khảo-sát-funding-rate-7-sàn) chỉ ra là nơi bug sinh ra.

REST trong Go không cần thư viện ngoài: `crypto/hmac` + `crypto/sha256` cho signing, `net/http` + `context` cho client, `golang.org/x/time/rate` cho rate limit.

#### Q8 — Vì sao backtest cũng Go

Quyết định này bị ràng buộc bởi chính **Bước 3.5** — cổng kiểm chứng backtest với mô phỏng thực tế. Cổng đó chỉ có giá trị chẩn đoán khi backtest và production chạy **cùng một đoạn code**. Nếu logic tín hiệu ở `internal/strategy` (Go) còn backtest viết lại bằng Python, khi hai bên lệch nhau sẽ không phân biệt được *chiến lược sai* hay *hai bản triển khai đã trôi khỏi nhau* — mất luôn thứ mà cổng tồn tại để phát hiện.

Thêm nữa, backtest funding arb là **máy trạng thái duyệt chuỗi sự kiện settle**, không phải đại số ma trận: không vectorize được, nên lợi thế lớn nhất của pandas biến mất. Các thư viện `backtrader`/`vectorbt`/`zipline` đều giả định chiến lược theo nến OHLCV một chân — không mô hình hoá được funding hai chân với chu kỳ khác nhau giữa các sàn.

Quét tham số cũng nhanh hơn hàng chục lần nhờ goroutine song song.

**Ranh giới với Python:**

```
Go     → strategy, backtest engine, execution, risk    (giữ state + chạm tiền)
SQLite → ranh giới duy nhất
Python → đọc kết quả: vẽ equity curve, khám phá ad-hoc  (CHỈ ĐỌC)
```

Python trở thành **bắt buộc** ở GĐ 8 (cointegration, VECM/GARCH, ML) — không có tương đương trong Go. Ranh giới SQLite ở trên đã sẵn sàng cho lúc đó.

#### Q9 — Vì sao không CCXT

CCXT **chuẩn hoá đi** đúng những khác biệt giữa các sàn mà khảo sát phát hiện là có ý nghĩa sống còn: ngữ nghĩa `fundingTime` của OKX, giá trị tuyệt đối của Kraken, mô hình liên tục của Paradex. Một lớp trừu tượng không nhìn thấy bên trong sẽ giấu chính xác những thứ cần nhìn thấy. Cộng thêm việc 7 connector tự viết đã chạy tốt.

#### Q10 — Vì sao giữ vanilla JS

FE hiện tại **đã tối ưu đúng cách** và không phải nút thắt: hàng đợi message gom lô 50ms ([app.js:357](../static/app.js#L357)), chỉ xử lý spread mới nhất và vứt phần còn lại ([app.js:400](../static/app.js#L400)), throttle 300ms cho việc dựng lại ma trận ([app.js:784](../static/app.js#L784)), chart dùng `series.update()` tăng dần.

Lãng phí nằm ở **backend** — xem §7.3. React/Vue giúp quản lý độ phức tạp ứng dụng, không giúp render dữ liệu tần suất cao; muốn đạt hiệu năng như hiện tại còn phải bypass cơ chế reconcile của chúng.

### 7.2. Còn cần chốt

| # | Câu hỏi | Cần trước | Gợi ý |
|---|---|---|---|
| Q1 | Sàn nào làm sàn chính cho execution? | GĐ 4.2 | Binance — tài liệu tốt nhất, có testnet, thanh khoản cao |
| Q2 | Database: SQLite hay PostgreSQL/TimescaleDB? | GĐ 2.3 | SQLite là đủ ở quy mô này; đổi sau nếu cần |
| Q3 | Vốn thật dự kiến cho GĐ 4.6? | GĐ 4.6 | $200–500 để kiểm chứng, không phải để kiếm lời |
| Q5 | Kênh alert: Telegram hay Discord? | GĐ 3.4 | Telegram — tiện trên điện thoại hơn |
| Q6 | Chấp nhận đòn bẩy tối đa bao nhiêu ở chân perp? | GĐ 5.4 | 2–3x; cao hơn thì rủi ro thanh lý vượt lợi ích |

> Q4 (framework FE) đã chuyển thành Q10 ở §7.1.

### 7.3. Ngưỡng mở rộng của tầng broadcast

Phân tích 2026-08-28. Nút thắt **không nằm ở FE** mà ở backend:

- `broadcastSpreads` chạy trên **mỗi tick giá từ bất kỳ sàn nào** ([main.go:168](../main.go#L168)) — dựng ma trận O(n²) rồi `WriteJSON` tới mọi client. FE nhận hàng trăm message/giây rồi vứt gần hết, chỉ giữ cái cuối.
- Backend gửi spread của **tất cả symbol** cho mọi client, FE tự lọc symbol đang xem. Hiện lãng phí 4×; ở 50 symbol sẽ là 50×.

Chi phí thật nằm ở **CPU mã hoá JSON phía Go và băng thông**, không phải DOM.

| Quy mô | Tình trạng |
|---|---|
| 4 symbol × 10 sàn (hiện tại) | ✅ Thoải mái |
| ~20 symbol | ⚠️ Cần throttle backend + client đăng ký symbol |
| 50–100 symbol | ❌ Phải lọc/xếp hạng phía server, chỉ đẩy top N |

**Ngưỡng gãy phụ thuộc số SYMBOL, không phải số sàn** — và điều này chạm trực tiếp mục tiêu funding arb, vì cơ hội tốt thường nằm ở cặp thanh khoản mỏng, nghĩa là sẽ muốn quét nhiều cặp. Dashboard ở **Bước 2.7** sẽ chạm ngưỡng này đầu tiên.

Thứ tự sửa, rẻ nhất trước:

1. Throttle `broadcastSpreads` bằng ticker giống `broadcastPrices` đã làm — ~10 dòng, ăn phần lớn lợi ích
2. Client gửi symbol đang xem, server chỉ đẩy symbol đó — bỏ lãng phí N×
3. FE bỏ `innerHTML` dựng lại toàn bộ, chuyển sang cập nhật `textContent` từng ô — chỉ cần khi vượt ~20 symbol

---

## 8. ĐỐI CHIẾU VỚI TÀI LIỆU 5 CHIẾN LƯỢC

Kế hoạch này chia nhỏ hơn tài liệu gốc, vì tài liệu gốc gộp quá nhiều việc vào "Giai đoạn 1".

| Tài liệu gốc | Kế hoạch này |
|---|---|
| GĐ 0 — Chuẩn bị nền tảng | **GĐ 0** (đã xong) |
| GĐ 1 — Funding Rate Arbitrage | **GĐ 1 + 2 + 3 + 4 + 5** (tách làm 5 vì phần execution & risk bị đánh giá thấp trong tài liệu gốc) |
| GĐ 2 — Basis Trade | **GĐ 6** |
| GĐ 3 — CEX-DEX | **GĐ 7** |
| GĐ 4 — Cross-Chain | **GĐ 8** |
| GĐ 5 — Statistical | **GĐ 8** |

**Khác biệt chính so với tài liệu gốc:** thêm hẳn một giai đoạn "Củng cố lõi" (GĐ 1) ở đầu, tách "Execution" khỏi "Risk Management" thành hai giai đoạn độc lập, và kéo instrument registry + ánh xạ spot↔perp từ phần execution lên GĐ 2. Lý do: codebase hiện tại có 4 vấn đề (staleness, trộn spot/perp, không có phí, không có test) sẽ nhân bản thành lỗi tốn tiền nếu xây tiếp lên trên mà không xử lý trước.

---

## PHỤ LỤC — CHECKLIST NHANH

```
[✅] GĐ 0  Nền tảng scanner              5/5 bước
[  ] GĐ 1  Củng cố lõi                   0/6 bước   ← BẮT ĐẦU TỪ ĐÂY
[  ] GĐ 2  Funding Rate Monitor          0/7 bước
[  ] GĐ 3  Signal, Alert & Backtest      0/5 bước
[  ] GĐ 4  Execution Engine              0/6 bước
[  ] GĐ 5  Risk & Vận hành               0/5 bước
[  ] GĐ 6  Basis Trade                   0/4 bước
[🔒] GĐ 7  CEX-DEX                       khoá
[🔒] GĐ 8  Cross-Chain / Statistical     khoá
```

**Việc tiếp theo cụ thể:** Bước 1.1 — thêm staleness filter, đổi `map[string]float64` thành `map[string]PricePoint` trong `main.go`.
