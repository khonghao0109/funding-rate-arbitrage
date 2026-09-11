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
| Frontend | Vanilla JS ~1.100 dòng + HTML ~760 dòng, TradingView Lightweight Charts. Danh sách nguồn và symbol **dựng từ message `meta`**, không còn hardcode |
| Số nguồn dữ liệu | 10 (7 futures + 2 spot + 1 oracle) |
| Cặp giao dịch | BTCUSDT, ETHUSDT, XRPUSDT, SOLUSDT (hardcode tại `main.go:352`) |
| Test coverage | **52,5%** ở `main` (39 test, Bước 1.0–1.1); `exchanges/` vẫn 0% |
| Persistence | **Không có** — toàn bộ state nằm trong RAM |

### 1.2. Đã có gì

| # | Thành phần | File | Trạng thái |
|---|---|---|---|
| 1 | Connector Binance Futures + Spot | `exchanges/binance.go` | ✅ Hoạt động |
| 2 | Connector Bybit Futures + Spot | `exchanges/bybit.go` | ✅ Hoạt động |
| 3 | Connector OKX Futures | `exchanges/okx.go` | ✅ Hoạt động |
| 4 | Connector Gate.io Futures | `exchanges/gate.go` | ✅ Hoạt động |
| 5 | Connector Kraken Futures | `exchanges/kraken/` | ✅ Hoạt động |
| 6 | Connector Hyperliquid (DEX) Futures | `exchanges/hyperliquid.go` | ✅ Hoạt động |
| 7 | Connector Paradex Futures | `exchanges/paradex.go` | ✅ Hoạt động |
| 8 | Pyth oracle price (SSE) | `exchanges/pyth/` | ✅ Hoạt động |
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
| 2 | ~~**Persistence / database**~~ | ~~Restart mất sạch dữ liệu, không backtest được~~ | ✅ **Bước 2.6** — SQLite, 3 bảng, backfill 7 sàn |
| 3 | **REST client + HMAC signing** — cả repo chỉ có 1 lời gọi HTTP (`pyth.go:77`) | Không thể đặt lệnh | GĐ 4 |
| 4 | **Mô hình phí** — không có dòng code nào tính phí (README đã sửa lại cho đúng, nay ghi rõ số hiển thị là spread thô) | Tín hiệu lợi nhuận là lợi nhuận thô, không phải ròng | GĐ 1 |
| 5 | **Khái niệm vị thế / margin / PnL** | Không quản trị được rủi ro | GĐ 5 |
| 6 | ~~**Lọc dữ liệu cũ (staleness)**~~ — ✅ xong ở GĐ 1.1: `RecvAt` do scanner đặt, ngưỡng theo từng sàn, giá cũ bị loại khỏi so sánh và hiển thị STALE | — | ✅ GĐ 1.1 |
| 7 | ~~**Tách bạch spot ↔ perp trong logic** — lấy min/max trên toàn bộ source~~ | ~~Sinh "cơ hội" không thực thi được~~ | ✅ **Bước 1.2** |
| 8 | **Test tự động** — 🔄 25 test cho hợp đồng wire ở Bước 1.0; `exchanges/` và `testdata/` vẫn trống | Connector đổi định dạng không ai biết | GĐ 1.6 |
| 9 | **Cấu hình** — symbol, ngưỡng, sàn đều hardcode | Không vận hành linh hoạt được | GĐ 1 |
| 10 | **Alert ra ngoài** (Telegram/Discord) | Phải ngồi nhìn màn hình | GĐ 3 |
| 11 | **Instrument metadata** (`stepSize`, `tickSize`, `minNotional`, contract size) | Không tính được size delta-neutral đúng — hai chân lệch nhau ngay lệnh đầu | GĐ 2 |
| 12 | **Bảng ánh xạ spot↔perp có xác thực** | Nguy cơ mở vị thế lệch coin | GĐ 2 |
| 13 | ~~**`Timestamp` mang hai nghĩa tuỳ sàn**~~ — ✅ xong ở GĐ 1.1: đổi thành `VenueTimeMs`, không sàn nào còn điền đồng hồ nội bộ (5 sàn đã sửa: Bybit, Kraken, Paradex, OKX, Gate) | — | ✅ GĐ 1.1 |
| 14 | ~~**Pyth bị tính như sàn giao dịch** trong `checkArbitrage`~~ | ~~Sinh "cơ hội" mua/bán trên oracle~~ | ✅ **Bước 1.2** |
| 15 | **Khối lượng đỉnh sổ** — đã thu ở 5/9 nguồn báo bằng coin | 4 sàn còn lại báo bằng contract, cần `ctVal`/`quanto_multiplier` | 🔄 **Bước 1.2** xong phần coin; phần contract → GĐ 2 (instruments) |

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

**Tổng: 9 giai đoạn (GĐ 0 → GĐ 8), 42 bước** (GĐ 6 đổi từ Basis Trade 4 bước sang Crowding Reversal 5 bước — Q11, 2026-09-11).

| GĐ | Tên | Số bước | Thời gian | Trạng thái | Kết quả bàn giao |
|---|---|---|---|---|---|
| **0** | Nền tảng scanner | 5 | — | ✅ **90% xong** | Scanner real-time 10 nguồn |
| **1** | Củng cố lõi (Hardening) | 7 | 3–4 tuần | 🔄 **7/7 bước, còn phiên 72h** | Scanner đáng tin, có test, có phí |
| **2** | Funding Rate Monitor | 7 | 4–5 tuần | ✅ **7/7 bước** | Thu thập + lưu funding rate 24/7 |
| **3** | Signal, Alert & Backtest | 5 | 3–4 tuần | 🔄 **2/5 bước** | Tín hiệu có kiểm chứng lịch sử |
| **4** | Execution Engine | 6 | 6–8 tuần | ⬜ Chưa bắt đầu | Bot đặt lệnh được (vốn nhỏ) |
| **5** | Risk & Vận hành | 5 | 4–6 tuần | ⬜ Chưa bắt đầu | Bot chạy production 24/7 |
| **6** | Crowding Reversal *(thay Basis Trade — Q11)* | 5 | 4–6 tuần cho 6.1–6.3, rồi ≥6 tháng paper ở 6.5 | ⬜ Chưa bắt đầu — 6.1 làm ngay được | Chiến lược thứ hai, ĐỊNH HƯỚNG, port Go có parity, qua cổng riêng |
| **7** | CEX-DEX Arbitrage | 1 (phác thảo) | 3–6 tháng | 🔒 Khoá | — |
| **8** | Cross-Chain / Statistical | 1 (phác thảo) | 12+ tháng | 🔒 Khoá | — |

**Tổng thời gian tới bot Funding Rate chạy production (GĐ 1→5): khoảng 5,5 – 7 tháng.**

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

**Còn nợ 10%:** không có config, không có xử lý staleness, connector chưa có test → chuyển sang GĐ 1. Hợp đồng WebSocket đã chốt và có test ở Bước 1.0.

---

### GIAI ĐOẠN 1 — CỦNG CỐ LÕI (HARDENING)

**Mục tiêu:** Biến scanner từ "chạy được" thành "tin được". Đây là giai đoạn **bắt buộc** trước khi thêm bất kỳ tính năng mới nào.
**Thời gian:** 3–4 tuần · **7 bước**

> **Cập nhật sau kiểm tra tiền-giai-đoạn (2026-08-28):** tăng từ 6 lên 7 bước. Kiểm tra code thật phát hiện 3 vấn đề chặn mà bản kế hoạch đầu không thấy: `Timestamp` mang hai nghĩa khác nhau tuỳ sàn nên không dùng làm cơ sở staleness được; một ngưỡng staleness duy nhất sẽ báo nhầm sàn thanh khoản mỏng; và ba bước 1.1–1.3 đều đổi hợp đồng JSON với frontend. Bước 1.0 được thêm để chốt hợp đồng một lần thay vì sửa `app.js` ba lần. Refactor `Feeds` cũng được kéo từ Bước 2.2 lên 1.5 để chỉ sửa 10 call site một lần.

#### Bước 1.0 — Chốt hợp đồng WebSocket ✅
- Thiết kế **một lần** toàn bộ shape JSON mà GĐ 1 sẽ cần: cờ trạng thái nguồn (live/stale/disconnected), ba khối spread tách biệt, số đã trừ phí, metadata nguồn, **và chỗ cho dữ liệu thanh khoản** (khối lượng đỉnh sổ ngay bây giờ, độ sâu đầy đủ ở GĐ 2 — xem [§7.4](#74-chiến-lược-độ-sâu-sổ-lệnh)).
- Cập nhật `app.js` **một lần** theo shape mới. Backend gửi giá trị mặc định hoặc rỗng cho phần chưa có dữ liệu.
- Từ 1.1 đến 1.3 chỉ **điền dữ liệu** vào hợp đồng này, không đổi shape nữa.
- Dọn kèm: `go mod tidy` — `godotenv` đang bị đánh dấu `// indirect` sai, nó được import trực tiếp tại [main.go:14](../main.go#L14).
- **Nghiệm thu:** dashboard chạy đúng như trước trên hợp đồng mới, với mọi trường mới ở giá trị mặc định.

**Đã làm (2026-08-28).** Hợp đồng đầy đủ ở [WS-CONTRACT.md](WS-CONTRACT.md): 4 message type (`meta`, `prices`, `spreads`, `arbitrage`), phong bì `{type, v, server_time_ms}`. `meta` gửi một lần lúc connect và **xoá cả 5 chỗ hardcode** trong `app.js` (PLAN cũ ghi 4 — thiếu `getShortSourceName`). Ma trận thành **mảng nhóm** ngay từ 1.0 (một nhóm `all`, `tradable:false`) để 1.2 chỉ tách nhóm chứ không sửa lại `app.js`. Ô ma trận là object `{spread_gross_pct, spread_after_fees_pct}` — trên wire **không còn trường nào tên `profit_*`**. Chỗ cho thanh khoản (`best_bid`/`best_ask`/`best_bid_qty_coin`/`best_ask_qty_coin`) đã có theo [§7.4](#74-chiến-lược-độ-sâu-sổ-lệnh). `go mod tidy` đã bỏ `// indirect` sai của `godotenv`.

**Bug có sẵn được sửa kèm** (đều nằm trong đúng vùng code của bước, không phải "tiện tay"):
- `checkArbitrage` chia cho `minPrice` có thể bằng 0 → `+Inf` → `json.Marshal` lỗi → **rớt toàn bộ client**. Nay `isUsablePrice` loại giá ≤ 0, NaN và vô cực, và **nói ra lý do** qua `excluded_sources`.
- Dedup cảnh báo là check-then-act qua hai vùng khoá khác nhau; `processPrices` và `processOrderbooks` chạy song song nên hai goroutine cùng qua cửa → cảnh báo trùng.
- `WriteJSON` cho từng client: một giá trị không mã hoá được sẽ **đuổi mọi client** sau khi gửi cho mỗi client một frame cụt. Nay mã hoá một lần rồi mới fan-out.
- `changeSymbol` deref `#symbolStatus` — phần tử này **chưa bao giờ tồn tại** trong `index.html`, nên mọi lần đổi symbol đều ném TypeError.
- Cột "Profit %" sắp xếp theo `data-sort="profit"` trong khi dữ liệu là `profit_pct` → sắp xếp theo cột đó không bao giờ hoạt động.

**Phát hiện ngoài phạm vi — ghi nhận, chưa xử lý:**
- `checkArbitrage` vẫn duyệt mọi nguồn nên **Pyth vẫn có thể xuất hiện trong bảng cảnh báo**. Ma trận đã được đánh dấu `tradable:false` nên không tô cơ hội, nhưng bảng cảnh báo thì chưa. → **Bước 1.2**.
- Hợp đồng mới làm payload `spreads` **to gấp 2,8 lần** (6.861 B so với 2.475 B ở 10 nguồn) và vẫn gửi metadata tĩnh của nhóm mỗi tick. → [§7.3](#73-ngưỡng-mở-rộng-của-tầng-broadcast).
- `broadcast` chạy **đồng bộ trên luồng ingest**; deadline ghi 2s mới thêm chỉ chặn được vô hạn, chưa chặn được N×2s. Cần hàng đợi gửi riêng cho từng client. → [§7.3](#73-ngưỡng-mở-rộng-của-tầng-broadcast).
- ~~`wire_test.go` chép tay danh sách 10 nguồn của `main()`~~ → ✅ **Bước 1.4**: `config.yaml` là danh sách duy nhất, và `cmd/scanner` có test đối chiếu hai chiều với registry connector.
- ~~`#symbolStatus` vẫn không tồn tại trong `index.html`~~ → ✅ **Bước 1.4**: hai chỗ gọi code chết đã xoá.

> Lý do tồn tại bước này: `app.js` có 925 dòng và hardcode danh sách nguồn ở 5 chỗ ([42-53](../static/app.js#L42-L53), [241-250](../static/app.js#L241-L250), [538-547](../static/app.js#L538-L547), [614](../static/app.js#L614)). Sửa nó ba lần liên tiếp mà không có test nào bảo vệ là ba lần có nguy cơ vỡ dashboard.

#### Bước 1.1 — Lọc dữ liệu cũ (staleness filter) ✅
- ⚠️ **Không dùng `Timestamp` hiện tại làm cơ sở.** Field này mang hai nghĩa khác nhau: Binance/OKX/Gate/Hyperliquid/Pyth điền thời gian sàn phát, còn Bybit/Paradex/Kraken điền `time.Now().UnixMilli()` — tức với 3 sàn đó nó luôn "mới" kể cả khi sàn đã ngừng gửi dữ liệu.
- Tách rõ hai field: `VenueTimeMs` (0 nếu sàn không cấp) và `RecvAtMs` (**luôn do bot đặt, tại một chỗ duy nhất**). Staleness đo bằng `RecvAtMs`.
- Đổi `map[string]float64` → `map[string]PricePoint`.
- **Ngưỡng cấu hình được theo từng sàn**, mặc định 10s — một ngưỡng duy nhất sẽ đánh dấu nhầm sàn thanh khoản mỏng đang yên ắng hợp lệ là chết. Ngưỡng thích ứng để sau, không làm ở giai đoạn này.
- Hiển thị trạng thái STALE / DISCONNECTED trên UI (dùng hợp đồng đã chốt ở 1.0).
- Việc lấy venue time thật cho Bybit/Kraken/Paradex tách riêng, không thuộc bước này.
- **Nghiệm thu:** chặn kết nối 1 sàn → sàn đó chuyển STALE trong ≤ ngưỡng của nó, không sinh cảnh báo nào từ dữ liệu đóng băng.

**Đã làm (2026-08-28).** `Timestamp` đổi thành `VenueTimeMs` ở cả 8 connector và **không sàn nào còn điền đồng hồ nội bộ** vào nó — Bybit/Kraken/Paradex trước đây điền `time.Now()`, OKX/Gate rơi về `time.Now()` khi parse lỗi; nay tất cả để 0. `RecvAt` do scanner đặt tại **đúng một chỗ** (`updatePrice`). Ngưỡng theo từng sàn. Giá stale **vẫn gửi** kèm `status:"stale"` để dashboard hiện được sàn chết, nhưng bị loại khỏi ma trận kèm lý do `stale`.

**Ngưỡng được ĐO, không đoán.** Quan sát 5 phút trên cả 4 cặp lúc thị trường hoạt động, khoảng cách lớn nhất giữa hai lần cập nhật:

| Sàn | Gap lớn nhất | Ngưỡng đặt |
|---|---|---|
| hyperliquid | 6,05s | 20s |
| bybit_spot | 4,02s | 15s |
| binance_spot | 3,85s | 15s |
| gate | 3,37s | 15s |
| kraken | 2,95s | 10s |
| bybit | 2,25s | 10s |
| paradex | 1,86s | 10s |
| okx | 1,00s | 10s |
| binance | 0,85s | 10s |

Mỗi ngưỡng dư khoảng 3× so với gap tệ nhất của chính sàn đó. **Pyth không có trong bảng** vì suốt buổi đo nó không gửi gì — nó giữ mặc định và cần đo lại khi feed hoạt động. **Chỉ đúng cho 4 cặp lớn trong giờ hoạt động** — cặp thanh khoản mỏng và giờ đêm cần đo lại (nợ ngưỡng thích ứng, đã ghi ở cuối giai đoạn).

Ngưỡng "mất kết nối" **tách khỏi** ngưỡng "giá cũ" (3× ngưỡng giá, tối thiểu 45s). Hai điều này khác nhau: "báo giá quá cũ để so sánh" là chuyện thường ở feed đẩy-theo-thay-đổi lúc thị trường yên, còn "sàn đã chết" là báo động. Gate, Kraken, Paradex và Pyth **không gửi trade nào**, nên tín hiệu sống duy nhất của chúng là sổ lệnh thay đổi — dùng chung ngưỡng sẽ tô đỏ một socket hoàn toàn khoẻ.

**Nghiệm thu đã thực hiện:** dựng một sàn giả cục bộ nói đúng giao thức Binance, trỏ connector Binance thật vào đó, rồi cho nó **im lặng mà vẫn giữ kết nối** (trường hợp khó hơn đóng TCP — im lặng phải suy ra, không được báo).

```
sàn im lặng → báo STALE:            10,20s  (ngưỡng 10s + 1 nhịp broadcast 200ms)
sàn im lặng → loại khỏi so sánh:    10,01s
cảnh báo sinh từ giá đóng băng:     0
```

**Bug có sẵn được sửa kèm:**
- `now := time.Now()` trong khối cooldown **che mất đồng hồ tiêm vào**, khiến chính test nghiệm thu của bước này vẫn xanh khi gỡ bộ lọc. Phát hiện bằng kiểm chứng đột biến, không phải bằng đọc code.
- `checkArbitrage` chỉ chạy khi có giá về, nên một symbol mà **mọi** nguồn cùng im lặng sẽ không bao giờ được xét lại — ma trận đóng băng vĩnh viễn trên màn hình. Thêm `refreshStaleness` chạy mỗi giây.
- Nguồn chưa từng gửi gì (Pyth trong lần chạy thật) **vắng mặt hoàn toàn** khỏi `source_status`, nên dashboard không thể báo là mất kết nối. Nay mọi nguồn đã đăng ký đều được báo cáo.
- `broadcastSpreads`/`broadcastOpportunity`/`meta` vẫn dùng `time.Now()` trong khi phần còn lại dùng đồng hồ tiêm — phá vỡ đẳng thức `age_ms = server_time_ms - recv_at_ms` của chính hợp đồng.

**Phát hiện ngoài phạm vi — ghi nhận:**
- ⚠️ **`RecvAt` được đóng dấu lúc lấy khỏi channel, không phải lúc đọc socket.** `broadcast` chạy đồng bộ trên cùng goroutine đó, nên một trình duyệt treo (deadline 2s) chặn ingest, `orderbookChan` (1000 chỗ) đầy dần, rồi cả đống tồn đọng được đóng dấu cùng một mốc `now` — báo giá cũ vài giây lên wire với `age_ms ≈ 0` và `status: live`, tức **đúng cái sai mà bước này tồn tại để dẹp**. Cách sửa đúng là đóng dấu ngay lúc đọc socket, và việc đó thuộc **Bước 1.5** (refactor `Feeds` chạm cả 10 connector) cộng với hàng đợi gửi riêng cho từng client ở [§7.3](#73-ngưỡng-mở-rộng-của-tầng-broadcast). Ở quy mô hiện tại (9 sàn, 4 cặp, 1–2 client) chưa quan sát thấy, nhưng nó có thật.
- Ngưỡng mới chỉ đo trên 4 cặp lớn. Thêm cặp thanh khoản mỏng ở GĐ 2 sẽ cần đo lại hoặc chuyển sang ngưỡng thích ứng.
- Lấy venue time thật cho Bybit/Kraken/Paradex vẫn chưa làm (nằm ngoài bước này theo PLAN); ba sàn đó hiện gửi `venue_time_ms: 0`.

#### Bước 1.2 — Tách bạch Spot ↔ Perpetual ↔ Oracle ✅
- Thêm trường `MarketType` (`spot` / `perp` / `future` / `oracle`) thay vì suy ra từ hậu tố chuỗi.
- 💰 **Thu khối lượng đỉnh sổ — đang miễn phí mà bị vứt.** `OrderbookData` chỉ có giá, trong khi Binance ([binance.go:30,32](../exchanges/binance.go#L30-L32)), Bybit ([bybit.go:19](../exchanges/bybit.go#L19)) và OKX ([okx.go:23](../exchanges/okx.go#L23)) đã parse sẵn khối lượng rồi bỏ đi. Thêm `BestBidQtyCoin` / `BestAskQtyCoin` → bộ lọc thanh khoản bậc một, không tốn thêm băng thông.
- 🐛 **Loại Pyth khỏi so sánh giao dịch được.** `checkArbitrage` hiện duyệt toàn bộ `s.prices[symbol]`, nên scanner có thể báo *"mua ở Pyth, bán ở Binance"* — vô nghĩa vì Pyth là oracle không giao dịch được. `MarketType = oracle` chỉ dùng làm tham chiếu.
- ⚠️ **Kraken quote là USD, không phải USDT.** Chênh lệch `PF_XBTUSD` với `BTCUSDT` chứa cả chênh USD/USDT. Loại khỏi so sánh chéo mặc định, hiển thị riêng có ghi chú.
- Chia làm 3 phép tính riêng biệt, không trộn:
  - **Cross-venue spread**: perp↔perp hoặc spot↔spot, cùng quote (thực thi được)
  - **Basis**: spot↔perp *cùng sàn* (nền tảng cho GĐ 2)
  - **Oracle deviation**: Pyth ↔ sàn (chỉ tham chiếu)
- Sửa `checkArbitrage` ([internal/scanner/scanner.go](../internal/scanner/scanner.go)).
- **Nghiệm thu:** UI hiện 3 khối riêng; không còn cặp trộn spot/perp; Pyth không xuất hiện trong khối giao dịch được.

**Đã làm (2026-09-03).** Quy tắc duy nhất: **hai nguồn chỉ được so với nhau khi cùng `market_type` VÀ cùng `quote_asset`**. Cài trong [internal/scanner/grouping.go](../internal/scanner/grouping.go), file mới, tách khỏi `wire.go` vì đây là logic quyết định *cái gì so được với cái gì*, không phải hình dạng message. Backend chia nhóm; **`app.js` không phải sửa vì đã lặp qua mảng nhóm từ 1.0** — trừ một lỗi frontend do chính bước này lộ ra (xem dưới).

| Nhóm | Nguồn | `tradable` |
|---|---|---|
| `perp_usdt` | binance, bybit, okx, gate | ✅ |
| `perp_usd` | hyperliquid, kraken, paradex | ✅ kèm ghi chú quote |
| `spot_usdt` | binance_spot, bybit_spot | ✅ |
| — | pyth | ❌ loại, lý do `oracle` |

- **Cảnh báo sinh TRONG từng nhóm**, không còn min/max trên toàn bộ nguồn. Cooldown khoá theo `symbol|group|buy|sell` để cảnh báo spot không bị cảnh báo perp nuốt mất.
- `basis[]`: spot ↔ perp **cùng sàn và cùng quote** (binance, bybit). `oracle_deviation[]`: từng sàn so với Pyth.
- Nguồn lẻ loi trong nhóm của mình bị loại với lý do `no_peer` — nó không phải ma trận một cột, nó là nguồn không có gì để so.
- `sourceMeta.Tradable` nay **thực sự được đọc**: một nguồn đánh dấu không giao dịch được làm cả nhóm thành tham chiếu, và luồng cảnh báo bỏ qua nhóm tham chiếu.

**Đơn vị khối lượng được ĐO, không đoán.** Tài liệu của cả ba sàn đều render bằng JS nên không đọc được từ môi trường này — theo [CLAUDE.md luật 5](../CLAUDE.md) thì phải nói ra chứ không được đoán. Đo trực tiếp trên payload thật (BTC ≈ $77,5k, XRP ≈ $1,36):

| Sàn | BTC | XRP | Kết luận |
|---|---|---|---|
| binance futures / spot | 7,943 / 16,408 | 92 867 / 20 375 | **coin** → điền |
| bybit linear / spot | 4,944 / 0,458 | 8 884 / 58,8 | **coin** → điền |
| hyperliquid | 1,655 | 45 968 | **coin** → điền |
| okx swap | 1 182,68 | 334,52 | **contract** → để 0 |
| gate futures | 10 099 (int) | 36 | **contract** → để 0 |
| kraken | 0,0929 | 95 000 | mâu thuẫn khảo sát → để 0 |

Khối lượng tỉ lệ nghịch với giá là số coin. OKX 1 182 BTC sẽ là đỉnh sổ $91M và XRP 334 coin sẽ là $456 — lệch vài bậc độ lớn, nên **để 0 thay vì quy đổi bằng phỏng đoán**. Quy đổi cần `ctVal`×`ctMult` (OKX) và `quanto_multiplier` (Gate) từ instrument registry, chưa có. **`0` nghĩa là "chưa biết", KHÔNG phải "không có thanh khoản"** — ghi rõ trong `exchanges.OrderbookData` và trên hợp đồng. Paradex không có gì để thu: kênh `markets_summary` chỉ có giá bid/ask, không có size.

⚠️ **Kraken mâu thuẫn với khảo sát.** [DATA-REQUIREMENTS §3](DATA-REQUIREMENTS.md) ghi Kraken tính bằng contract, nhưng số đo (PF_XBTUSD 0,0929 với BTC ≈ $77,5k) trông như số coin. Không điền vào field tên `...Coin` dựa trên một phép đo mâu thuẫn với khảo sát — để 0, để instrument registry (GĐ 2) phân xử.

**Nghiệm thu đã thực hiện** — chạy scanner thật 40 giây, bắt wire, kiểm bằng script:

```
3 khối riêng trên cả 4 symbol:  perp_usdt · perp_usd · spot_usdt
ô ma trận trộn market_type/quote:  0
cảnh báo (31 cái):  gọi tên pyth 0 · trộn market_type hoặc quote 0
                    perp_usd 20 · perp_usdt 10 · spot_usdt 1
bid < ask và mid == (bid+ask)/2:  đúng ở mọi nguồn có sổ
đỉnh sổ: binance/bybit/hyperliquid có số; okx/gate/kraken/paradex = 0 như thiết kế
```

Dashboard kiểm bằng jsdom nạp `index.html` + `app.js` thật với payload vừa bắt: **11/11** khẳng định đạt, khối hiển thị đúng thứ tự `Perpetual · quote USDT`, `Perpetual · quote USD`, `Spot · quote USDT`, `Basis (spot ↔ perp cùng sàn)`.

**Kiểm chứng đột biến** (phá code, xác nhận test chuyển đỏ): coi oracle như nguồn thường → đỏ; dồn mọi nguồn vào một nhóm → đỏ; vứt lại khối lượng đỉnh sổ → đỏ; tính cảnh báo trên toàn bộ nguồn như trước 1.2 → đỏ; khôi phục early-return của `app.js` → 5/11 khẳng định dashboard đỏ.

**Bug được review tìm ra và sửa trong bước:**
- 🐛 **`performSpreadsMatrixUpdate` return sớm khi không có nhóm nào** — và "không có nhóm" nay là **trạng thái bình thường** (nhóm cần 2 nguồn, nguồn lẻ bị loại `no_peer`). Hậu quả: dashboard hiện "Waiting for price data..." trong khi backend đang gửi cả `basis`, `oracle_deviation` lẫn danh sách `excluded_sources` — tức **giấu đi đúng lời giải thích vì sao ma trận rỗng**. Đây là chỗ duy nhất `app.js` phải sửa ở bước này, ngược với dự đoán của PLAN.
- `sourceMeta.Tradable` chưa bao giờ được đọc; chỉ `market_type == oracle` được kiểm. Một nguồn không giao dịch được mà không phải oracle vẫn lọt vào nhóm tradable và bị gọi tên trong cảnh báo.
- `oracle_deviation` không chặn lệch quote: Pyth quote USD, 6 sàn quote USDT, nên phần lớn số đo mang cả chênh USD/USDT. Thêm `quote_asset_mismatch` (mặc định `false`, hợp đồng cho phép **thêm** trường) và dashboard gắn nhãn "lệch quote". Giữ lại các dòng đó thay vì bỏ, vì đối chiếu sàn với oracle chính là mục đích của khối này.
- Nguồn không có trong registry bị loại với lý do `quote_mismatch` — một khẳng định sai, vì ta không biết gì về nó kể cả quote. Thêm lý do `unregistered`.
- Test `TestProcessOrderbooks_StoresTopOfBook` để rò goroutine, đua với `withRegistry` của test sau khi ghi vào `sourceRegistry` toàn cục. `-race` đỏ khoảng 1/3 lần chạy.
- Thông báo trạng thái rỗng của `app.js` nói sai nguyên nhân khi người dùng tắt hết nguồn ở bộ lọc.

**Phát hiện ngoài phạm vi — ghi nhận, chưa xử lý:**
- ⚠️ **Bybit `orderbook.1` đẩy cả snapshot lẫn delta, connector không phân biệt.** Delta xoá mức đỉnh sổ mang size `"0"`, nay lên wire thành `best_bid_qty_coin: 0` tức "chưa biết". Đây là đúng bẫy số 3 trong [CLAUDE.md](../CLAUDE.md) và phải sửa bằng cách gộp delta vào state cache — thuộc về sửa connector Bybit, không thuộc bước này. Giá cũng chịu ảnh hưởng tương tự và đã sai từ trước bước này.
- `partitionSources` chạy **hai lần mỗi tick** (một cho cảnh báo trong `evaluate`, một trong `newWireSpreads`), cộng `buildBasis` và `buildOracleDeviation`, tất cả trên đúng đường broadcast mà [luật 11](../CLAUDE.md) đã chỉ là nút thắt phía server. Chưa phải bug ở 9 nguồn × 4 cặp, nhưng nó chồng lên đúng điểm nóng → [§7.3](#73-ngưỡng-mở-rộng-của-tầng-broadcast).
- Pyth vẫn **không gửi được gì** (`Pyth SSE connection closed` lặp mỗi 5s), nên `oracle_deviation` rỗng trong lần chạy thật và ngưỡng staleness của nó vẫn chưa đo được. Feed Pyth cần sửa riêng.
- Hyperliquid báo đỉnh sổ lớn hơn hẳn các sàn khác (41 BTC ≈ $3,2M) vì `l2Book` gộp mức giá thô hơn `bookTicker`. So thanh khoản chéo sàn phải tính đến chuyện này → GĐ 2 khi dùng số này để xếp hạng.

#### Bước 1.3 — Mô hình phí giao dịch ✅
- Tạo `internal/fees/` chứa bảng phí maker/taker theo sàn và loại thị trường (bậc mặc định, chưa VIP).
- ⚠️ **Gọi đúng tên: "đã trừ phí giao dịch", KHÔNG phải "lợi nhuận ròng".** Slippage cần độ sâu sổ lệnh, mà hiện chỉ có `bookTicker` tức đỉnh sổ — phải tới GĐ 2 mới có. Đặt tên sai ở đây là lặp lại đúng lỗi mà README vừa được sửa.
- Giả định bảo thủ: taker **cả bốn lượt khớp** — mở và đóng cả hai chân.
  > **Sửa so với bản trước của PLAN.** Mục này từng ghi "taker cả hai chân". Hai
  > là số chân, không phải số lượt khớp: bắt được spread cần mua ở sàn rẻ và bán ở
  > sàn đắt (2 lượt), nhưng chỉ **thoát vị thế** mới biến nó thành tiền (2 lượt
  > nữa) — perpetual không chuyển được giữa hai sàn. Tính hai lượt là nói thiếu
  > đúng một nửa chi phí, tức đúng kiểu nói quá lợi nhuận mà [luật 2](../CLAUDE.md)
  > cấm. Chữ "bảo thủ" trong chính câu này đòi hỏi con số lớn hơn, không phải nhỏ hơn.
- **Nghiệm thu:** unit test cho hàm tính phí; UI ghi rõ số đang hiển thị đã trừ gì và chưa trừ gì.

**Đã làm (2026-09-03).** `internal/fees/` giữ bảng phí bậc mặc định (chưa VIP) và hàm `RoundTripTakerPct`. `sourceRegistry` **không** giữ bản sao — nó lấy số từ đây qua `withFees`, nên phí và trích dẫn nằm đúng một chỗ.

**Chỉ xác minh được 5/9 sàn — 4 sàn còn lại KHÔNG điền.** [Luật 5](../CLAUDE.md) cấm viết con số theo trí nhớ, vì phí sai tạo ra lợi nhuận sai mà không nhìn ra là sai.

| Sàn | Maker | Taker | Nguồn |
|---|---|---|---|
| binance_futures | 2 bps | 5 bps | Trang hỗ trợ Binance (nằm trong ví dụ tính toán; bảng gốc đòi đăng nhập) |
| binance_spot | 10 bps | 10 bps | Bảng biểu phí Binance, dòng Regular User |
| hyperliquid_futures | **1,5 bps** | 4,5 bps | Tài liệu Hyperliquid, bậc 0 |
| kraken_futures | 2 bps | 5 bps | Biểu phí Kraken, Futures bậc 1 |
| paradex_futures | **0,3 bps** | 4,5 bps | Tài liệu Paradex, bậc Pro (đầu cao thang taker) |
| bybit_futures / bybit_spot | — | — | Trang biểu phí không phản hồi |
| okx_futures | — | — | 404 / chỉ có metadata, bảng thật đòi đăng nhập |
| gate_futures | — | — | ⚠️ Gate có công bố 0,0200%/0,0500% nhưng **ghi rõ là cho "USDT-M TradFi Perpetuals"** (cổ phiếu, kim loại, chỉ số, forex) — không phải perp crypto. Dùng nó là đúng bảng, sai thị trường |

`fee_verified: false` **không phải miễn phí**. Cặp nào có một sàn chưa xác minh thì `spread_after_fees_pct` là `null`, và dashboard đánh dấu ô đó còn là số thô. Paradex tính 0% cho tài khoản Retail nên `0` là một mức phí có thật — đó chính là lý do phải có cờ riêng thay vì đọc số `0`.

⚠️ **`Bps` phải là số thực.** Hyperliquid maker 0,015% = **1,5 bps**, Paradex maker 0,003% = **0,3 bps**. [CONVENTIONS §1.2](CONVENTIONS.md) trước đây yêu cầu `Bps` số nguyên để tránh sai số float; tiền đề đó sai với biểu phí thật, nên đã sửa mục đó thay vì làm tròn số. Làm tròn 0,3 → 0 là biến một chân thành miễn phí.

**Nghiệm thu đã thực hiện:**
- Unit test hàm tính phí: 7 test trong `internal/fees` (bốn lượt khớp, sàn chưa xác minh không ra số, mọi mục đã xác minh đều có trích dẫn URL, chặn lệch dấu thập phân, bps thực).
- UI: dashboard thật qua jsdom với payload bắt từ scanner đang chạy — **26/26** khẳng định đạt, trong đó: ghi rõ đã trừ gì (`đã trừ phí giao dịch`), chưa trừ gì (`trượt giá`, `phí funding`), câu `KHÔNG phải lợi nhuận ròng`, ô còn thô bị đánh dấu `*` và **không bao giờ** được tô "cơ hội", sàn chưa có phí có nhãn riêng, oracle **không** bị dán nhãn đó.

**Con số thật từ scanner đang chạy — mọi cảnh báo đều ÂM sau phí:**

```
ETHUSDT  perp_usd  kraken → paradex   thô +0,1422%   sau phí -0,0478%
ETHUSDT  perp_usd  kraken → paradex   thô +0,1044%   sau phí -0,0856%
ETHUSDT  perp_usd  kraken → paradex   thô +0,0793%   sau phí -0,1107%
nhóm perp_usd, 6 ô ma trận:  thô -0,04% … +0,04%   sau phí -0,23% … -0,15%
```

Đây là câu trả lời thật cho câu hỏi "chênh lệch chéo sàn có ăn được không" ở quy mô lẻ: **không**. Một vòng round trip tốn ~0,19% còn spread chéo sàn ở các cặp lớn chỉ vài phần nghìn phần trăm. Nó củng cố đúng điều [CLAUDE.md](../CLAUDE.md) đã nói: biên thật nằm ở funding (5–15% APR), không nằm ở mấy con số này.

**Ngưỡng cảnh báo giữ nguyên trên số THÔ.** Chuyển sang lọc theo số sau phí sẽ tắt gần như toàn bộ cảnh báo. Quyết định "cái gì đáng hành động" là việc của Giai đoạn 3, không phải hệ quả phụ của việc thêm bảng phí. Bù lại, mỗi cảnh báo nay mang theo số sau phí và bảng cảnh báo có thêm cột "Sau phí %".

**Bug được review tìm ra và sửa trong bước:**
- 🐛 **Nhãn "chưa có phí" bị xoá ngay tick giá đầu tiên.** Nó dùng chung class `.source-badge` mà `updateSourcePrices` tìm bằng `querySelector` rồi `remove()` — nửa nhìn thấy được của cả bước không bao giờ sống tới màn hình. Đổi sang class riêng.
- 🐛 **Ô thô và ô đã trừ phí hiển thị y hệt nhau, dùng chung một ngưỡng tô màu**, nên cặp *chưa* xác minh phí trông đẹp hơn và còn được tô "cơ hội" ở mức spread thật thấp hơn. Nay ô thô có dấu `*`, in nghiêng mờ, và **không bao giờ** được tô "cơ hội".
- 🐛 **Bảng cảnh báo chỉ hiện số thô** trong khi payload đã có số sau phí — cùng một cặp đọc là +1,000% ở bảng và −0,048% ở ma trận. Thêm cột "Sau phí %".
- Pyth bị dán nhãn "chưa xác minh biểu phí" trong khi lý do thật là *oracle không giao dịch được* — đúng cái phân biệt mà bảng phí tồn tại để giữ.
- `maker_rebate` nằm trong `cost_basis.excluded`, mà FE render mục đó thành "Chưa trừ: …" — hoàn phí là **thu nhập**, ghi ở đó là chỉ sai hướng cho người đọc. Bỏ.
- `note_vi` liệt kê lại đúng danh sách `excluded` mà FE đã render ngay bên cạnh.
- Test `TestNewWireSpreads_AfterFeesGoesNegative...` chọc vào mọi nhóm bằng một cặp hardcode; chỉ xanh vì fixture tình cờ ra đúng một nhóm.

**Kiểm chứng đột biến** (phá code, xác nhận test đỏ): coi phí chưa xác minh là 0 → đỏ; chỉ tính phí lúc mở → đỏ; làm tròn 1,5 bps thành 2 → đỏ; rơi về số thô khi thiếu phí → đỏ; nhãn phí về class chung → đỏ; bỏ dấu `*` → đỏ. **Một test ban đầu KHÔNG đỏ** khi bỏ chặn tô "cơ hội" cho ô thô — fixture thật chỉ có spread ~0,01%, dưới xa ngưỡng tô màu, nên khẳng định đó rỗng nghĩa. Đã thay bằng fixture 1% và xác nhận nó đỏ.

**Phát hiện ngoài phạm vi — ghi nhận, chưa xử lý:**
- Bốn sàn chưa có phí là khoảng trống lớn nhất còn lại của bước này. **Bước 1.4** (`config.yaml`) là chỗ đúng để người vận hành nhập biểu phí tài khoản của chính mình — vốn dĩ mới là nguồn đúng duy nhất, vì phí phụ thuộc bậc VIP và khối lượng 30 ngày.
- `basis[]` chưa có số sau phí. Chi phí của một vị thế delta-neutral thuộc về tầng chiến lược (GĐ 2–3), không phải bảng hiển thị.
- Chưa mô hình hoá phí rút/chuyển tiền giữa các sàn. Với arbitrage chéo sàn có luân chuyển tài sản, đây là khoản chi phí thật còn thiếu → GĐ 4.

#### Bước 1.4 — Cấu hình hoá ✅
- Chuyển symbol, danh sách sàn, ngưỡng cảnh báo, ngưỡng staleness theo sàn từ hardcode sang `config.yaml`.
- Backend gửi kèm **metadata nguồn**, FE tự dựng danh sách thay vì hardcode ở 4 chỗ.
- **Nghiệm thu:** thêm 1 cặp hoặc 1 sàn mới không cần sửa code Go **lẫn** JavaScript.

**Đã làm (2026-09-03).** `config.yaml` ở gốc repo là **nguồn sự thật duy nhất** cho cặp giao dịch, danh sách nguồn, ngưỡng cảnh báo, ngưỡng staleness từng sàn, biểu phí, và cổng HTTP. `internal/config` nạp và **kiểm tra** nó; `scanner.Configure(cfg)` dựng registry từ đó; `cmd/scanner` lặp trên `cfg.Sources` thay cho 10 lời gọi `go exchanges.Connect*` cứng.

⚠️ **P1 phát hiện tiêu chí nghiệm thu không đạt được nếu chỉ chuyển `symbols` sang YAML.** Thêm một cặp hôm nay phải sửa Go ở **năm** connector, không phải một: Pyth chỉ có feed ID cho BTCUSDT, Kraken/Paradex/Gate/OKX mỗi sàn một bảng ánh xạ cứng, Hyperliquid dùng `symbol[:3]`. Nên **ánh xạ ký hiệu cũng vào config**:

```yaml
symbols:
  - { symbol: BTCUSDT, base: BTC, quote: USDT }   # base/quote KHAI, không cắt chuỗi

sources:
  - source: okx_futures
    symbol_format: "{base}-{quote}-SWAP"          # → BTC-USDT-SWAP
  - source: kraken_futures
    symbol_format: "PF_{base}USD"
    symbol_map: { BTCUSDT: PF_XBTUSD }            # Kraken gọi bitcoin là XBT
  - source: pyth
    symbol_map: { BTCUSDT: e62df6c8… }            # feed ID, không khuôn nào suy ra được
```

Thứ tự áp dụng: `symbol_map` → `symbol_format` → (có map mà không có cặp → **bỏ qua cặp đó ở sàn đó**) → nguyên ký hiệu chuẩn. Một cơ chế thay cho **sáu** bảng cứng; `exchanges/` **−254 dòng, +113 dòng**.

**Nghiệm thu đã thực hiện — sửa ĐÚNG `config.yaml`, không đụng Go lẫn JS:**

| Thêm gì | Sửa gì | Kết quả chạy thật |
|---|---|---|
| Cặp `DOGEUSDT` | 1 dòng trong `symbols:` | 8/9 sàn trả giá ngay, kể cả Hyperliquid `0,0828` |
| Sàn `bybit_spot_mirror` (dùng lại connector `bybit_spot`) | 1 khối trong `sources:` | Có nhãn, màu, giá riêng và xuất hiện trong `meta` |

`DOGEUSDT` được chọn cố ý vì base dài **4** ký tự — đúng trường hợp `symbol[:3]` cũ sẽ lặng lẽ đăng ký nhầm sang `DOG`. Bug đó (PLAN xếp cho Bước 2.4) biến mất như hệ quả của thiết kế này.

**Bug lộ ra ngay trong lúc nghiệm thu:** `bybit_spot_mirror` có mặt trong `source_status` nhưng **không bao giờ có giá** — connector hardcode `Source: "bybit_spot"` trong 16 chỗ, nên hai mục config dùng chung connector cùng báo về một tên. Tên nguồn là **cấu hình**, không phải giao thức, nên nay truyền vào connector. Nếu không đo bằng cách chạy thật thì lỗi này lọt: test và build đều xanh.

**Nợ được trả:**
- `wire_test.go` chép tay danh sách 10 nguồn (ghi ở Bước 1.0) → xoá; nay `cmd/scanner` đối chiếu **hai chiều**: mọi source phải trỏ tới connector có thật, và mọi connector phải được ít nhất một source dùng.
- `#symbolStatus` — phần tử chưa bao giờ tồn tại trong `index.html` → xoá 2 chỗ gọi code chết.
- Biểu phí 4 sàn chưa xác minh nay có chỗ nhập tay, kèm hướng dẫn ngay trong `config.yaml`.

**Kiểm tra mà cấu hình phải qua** (mỗi cái chặn một lỗi *im lặng*, không phải lỗi ồn ào): cặp trùng, nguồn trùng, `market_type` lạ, thiếu `quote_asset` (không xếp nhóm được), `stale_after_sec: 0` (đánh dấu stale ngay khi vừa tới), phí `verified: true` mà thiếu `doc_url`, phí `verified: false` mà vẫn có số (trông như đã kiểm), nguồn không phục vụ cặp nào (kết nối rồi ngồi im mà vẫn "khoẻ"), **hai cặp ánh xạ về cùng một ký hiệu sàn** (tra ngược lấy cái đầu, cặp kia im lặng mất dữ liệu), cổng rỗng (`ListenAndServe` bind cổng ngẫu nhiên). `KnownFields(true)` biến một key gõ sai thành lỗi thay vì âm thầm dùng mặc định.

**Test chạy trên `config.yaml` THẬT, không dùng fixture** — `TestMain` của package `scanner` nạp chính file được ship. Một fixture sẽ để config trôi dạt trong khi test vẫn xanh, mà nay nó là nơi duy nhất danh sách nguồn tồn tại. Test *logic* chia nhóm thì ngược lại: dùng registry giả, để thêm một sàn không làm vỡ test của luật.

**Bug được review tìm ra và sửa trong bước:**
- Hai cặp ánh xạ về cùng ký hiệu sàn không bị chặn — `{base}` sẽ gộp `BTCUSDT` và `BTCUSDC` thành `"BTC"`, tra ngược lấy cái đầu, cặp kia **im lặng không bao giờ có dữ liệu**.
- `server.port` rỗng → `ListenAndServe(":")` bind cổng ngẫu nhiên trong khi log in ra `http://localhost:`.
- `group_id` viết thường `quote_asset` còn ba khối khác so khớp nguyên văn → `Usdt` vừa vào nhóm USDT vừa bị coi là lệch quote. Nay chuẩn hoá hoa/thường lúc nạp.
- `colspan="7"` ở hai hàng trống của bảng cảnh báo trong khi Bước 1.3 đã thêm cột thứ 8.

**Phát hiện ngoài phạm vi — ghi nhận:**
- ⚠️ `withRegistry` trong test ghi vào biến toàn cục trong khi goroutine của test trước còn chạy. Hiện an toàn **chỉ vì may**: `refreshStaleness` thoát sớm ở `hasClients()` và `processTrades` chỉ ghi timestamp, nên không chạm registry. Thêm một lượt đọc registry trước hai chốt đó là thành lỗi `-race` thật. Cách sửa đúng là cho Scanner cách dừng goroutine của nó → **Bước 1.5** (`ctx`). `-race -count=10` hiện sạch.
- Paradex trả `DOGE-USD-PERP` giá **0,182** trong khi 8 sàn khác báo ~0,083. `symbol_format` sinh ra một ký hiệu **hợp lệ về hình thức** nhưng có thể không phải thị trường mình tưởng — và chính ma trận chéo sàn của scanner là thứ phát hiện ra. Thêm cặp mới phải kiểm lại giá từng sàn, không chỉ kiểm nó có dữ liệu.
- `cmd/scanner` coverage 0% (chỉ có test đối chiếu config↔connector, không chạy `main`). Kiểm thật là chạy scanner, không phải test đơn vị.

#### Bước 1.5 — Kết nối bền bỉ + refactor `Feeds` ✅
- **Gộp refactor `Feeds` từ Bước 2.2 lên đây** — cả hai bước đều sửa chữ ký của 10 connector ([cmd/scanner/main.go](../cmd/scanner/main.go)), làm rời nhau là sửa hai lần:

```go
type Feeds struct {
    Ctx       context.Context
    Price     chan<- PriceData
    Orderbook chan<- OrderbookData
    Trade     chan<- TradeData
    Conn      chan<- ConnEvent   // connector tự báo trạng thái kết nối
}

func ConnectBinanceFutures(source string, symbols []Symbol, f Feeds)
```

> **Đối chiếu P1 (2026-09-03) — hai chỗ khung trên đã lỗi thời so với code thật:**
>
> 1. **Chữ ký.** Bước 1.4 đã đổi connector thành `(source string, symbols []Symbol, ...)`:
>    tên nguồn là cấu hình, và ánh xạ ký hiệu sàn đã làm sẵn ở `cmd/scanner`. Khung
>    viết `symbols []string` là từ trước 1.4. Giữ nguyên hai tham số đó, chỉ gộp
>    `ctx` + 3 channel thành `Feeds`.
> 2. **Bỏ trường `Funding`.** Khai báo nó bây giờ đòi phải có kiểu `FundingData`,
>    mà thiết kế struct đó là **Bước 2.2** và Bước **2.1 tồn tại chính để xác minh
>    field TRƯỚC khi viết struct**. Khai sẵn theo phỏng đoán là đảo ngược thứ tự đó
>    và vi phạm luật 5 (không bịa field API sàn). Không mất gì: mục đích của `Feeds`
>    là **thêm một channel về sau là thay đổi cộng thêm, không đụng chữ ký nào** —
>    Bước 2.2 thêm một dòng vào struct và sửa 0 connector. Đó đúng là thứ việc gộp
>    này mua được.
> 3. **Thêm `Conn`.** `source_status.state` ở Bước 1.1 là **suy ra từ im lặng**;
>    hợp đồng nói Bước 1.5 đổi thành "connector tự báo". Muốn vậy phải có đường
>    truyền ngược từ connector về scanner — một channel nữa, đúng chỗ `Feeds` vừa
>    làm cho rẻ.

- **Đóng dấu `RecvAt` ngay lúc đọc socket**, không phải lúc lấy khỏi channel — xem phát hiện ghi ở Bước 1.1. Đây là lúc chạm cả 10 connector nên là chỗ rẻ nhất để làm.
- Exponential backoff (2s → 4s → … → tối đa 60s) thay `time.Sleep` cố định.
- Ping/pong keepalive + `SetReadDeadline` cho từng connector.
- Mọi goroutine có điều kiện thoát qua `ctx`.
- Metric per-sàn: uptime, số lần reconnect, độ trễ message cuối.
- **Nghiệm thu:** chạy 72h không can thiệp; `ctx` huỷ làm mọi connector dừng sạch trong ≤5s.

**Kết quả (2026-09-03).** `ctx` huỷ → **10/10 connector dừng trong 369µs**, ngân sách
là 5s. Chạy liên tục 196 giây với 9 sàn: **không sàn nào rớt, không lần reconnect
nào**, `uptime_sec` và `reconnect_count` chạy thật trên wire.

🔶 **Vế "72h không can thiệp" CHƯA chạy** và không thể chạy trong một phiên làm
việc. Không hạ chuẩn: nó vẫn là điều kiện thoát của Giai đoạn 1, ghi ở mục §1
cuối GĐ. Cái đã kiểm được và có ý nghĩa cho một phiên dài:

- Vế thứ hai của tiêu chí (`ctx` huỷ ≤5s) — **đã đạt, đo được, có test tự động**.
- Ba cơ chế mà một phiên 72h phụ thuộc vào, mỗi cái có test đi kèm và đều được
  kiểm bằng cách **cố tình phá rồi xem test có đỏ không**: backoff leo đúng và
  không reset sai, read deadline bắt được socket còn mở mà ngừng đẩy, ping của
  server được tính là hoạt động.
- Backoff đã chạy thật trên một endpoint hỏng thật (Pyth 401) và leo đúng dãy
  2→4→8→16→32→60s tới trần.

Cái 196 giây **không** chứng minh được: rò rỉ bộ nhớ, trôi goroutine, hành vi khi
sàn bảo trì, hoặc giới hạn thời gian sống của kết nối mà sàn áp (Binance có, và
tài liệu về nó không đọc được từ đây).

**Thiết kế: một vòng đời dùng chung thay chín bản sao.** Chín connector WebSocket
trước đây mỗi cái tự dial, tự ngủ, tự thử lại — chín bản sao của cùng một vòng
lặp, và cả chín mang đúng ba lỗi giống nhau: `time.Sleep` cố định (thử lại sàn
chết mỗi 2 giây, **43.200 lần quay số một ngày** — đủ để một sự cố biến thành lệnh
cấm vì rate limit sống lâu hơn chính sự cố đó), không có read deadline (socket
nửa-mở không bao giờ gửi thêm byte nào **không phân biệt được** với thị trường
đang yên, connector chờ nó vĩnh viễn), và không có cách nào dừng. Chín bản sao
cũng là chín cơ hội để một bản vá được áp dụng tám lần.

Nay chỉ còn [`runStream`](../exchanges/stream.go). Connector chỉ khai thứ riêng của
sàn: URL, cách subscribe, cách giữ nhịp, cách parse một frame. `exchanges/` giảm
**−1.264 dòng, +1.290** trong khi nhận thêm cả một tầng bền bỉ và bộ test đầu tiên.

**Keepalive: đọc tài liệu, rồi ĐO.** Tra được 6/7 sàn. Đo lại từng cái bằng probe
thật trước khi tin:

| Sàn | Tài liệu nói | Đo được 2026-09-03 |
|---|---|---|
| OKX | ngắt sau 30s im lặng; gửi **chuỗi thô** `ping` | `ping` → `pong` ✅ |
| Bybit | `{"op":"ping"}` mỗi 20s | → `{"ret_msg":"pong"}` ✅ |
| Hyperliquid | đóng nếu 60s không gửi gì; `{"method":"ping"}` | → `{"channel":"pong"}` ✅ |
| Paradex | **server** ping mỗi 55s, client phải pong trong 5s | ping giao thức → pong ✅ |
| Gate | tài liệu **khuyên dùng ping tầng giao thức** | ping giao thức → pong ✅ |
| Kraken Futures | "gửi ping ít nhất mỗi 60s" — **không nói dạng message** | `{"event":"ping"}` → ❌ `{"event":"alert","message":"Bad websocket message"}`; ping giao thức → pong ✅ |
| Binance | không đọc được (trang render client-side) | ping giao thức → pong ✅ |

⚠️ **Kraken là lý do phải đo.** `{"event":"ping"}` là phỏng đoán hiển nhiên, và nó
**sai**. Nếu ship, Kraken sẽ nhận một message rác mỗi 30 giây suốt 72 giờ. Tài
liệu cho biết *khoảng cách*, không cho biết *hình dạng*; chỗ tài liệu im lặng thì
đo, không đoán (luật 5).

**Hai lỗi tinh vi của gorilla/websocket, cả hai đều làm rớt socket khoẻ mạnh:**
1. Handler pong mặc định **không làm gì** — feed yên tĩnh nhưng còn sống sẽ hết
   hạn read deadline.
2. Handler ping mặc định **trả pong nhưng không đụng vào read deadline**, mà
   control frame bị nuốt bên trong `ReadMessage` chứ không trả về cho caller. Với
   Paradex — sàn giữ kết nối bằng cách ping *chúng ta* — mặc định sẽ giết một
   socket hoàn toàn khoẻ mạnh sau mỗi `readTimeout`.

**`state` do connector báo — nhưng KHÔNG thay thế suy luận từ im lặng, mà HỢP với
nó.** Mỗi bên biết thứ bên kia không biết. Connector biết sàn mất kết nối ngay một
giây sau tick cuối (im lặng còn gọi nó khoẻ thêm 45 giây). Im lặng biết thứ
connector không thể biết, và **đây mới là lỗi hay lẩn**: một subscription bị sàn
âm thầm huỷ để lại socket *thật sự đang mở*, khoẻ theo mọi thước đo connector có,
và không bao giờ đẩy dữ liệu nữa. Nên `connected` + im lặng quá ngưỡng = vẫn
`disconnected`.

**Bằng chứng cho lý do Bước 1.0 tồn tại:** metric kết nối cần đúng ba trường
(`state`, `reconnect_count`, `uptime_sec`), cả ba đã đặt sẵn trên wire từ 1.0 với
mặc định ghi rõ. Đổ dữ liệu thật vào chúng cần **0 dòng JavaScript**. `state:
"reconnecting"` cũng vậy — dashboard đã có chấm trạng thái cho nó từ 1.1, chỉ là
backend chưa bao giờ gửi. (JS duy nhất phải sửa là hiển thị *thêm* uptime và số
lần nối lại trong tooltip — tính năng mới, không phải sửa vì hợp đồng đổi.)

**Bỏ trường `Funding` mà khung ở trên khai sẵn** — lý do ở khối đối chiếu P1 phía
trên: nó đòi kiểu `FundingData` mà Bước **2.1 tồn tại chính để xác minh field
TRƯỚC khi viết struct**. Không mất gì, vì đó đúng là thứ `Feeds` mua được: Bước
2.2 thêm một dòng vào struct và sửa **0 connector**.

**Review tìm 5 lỗi, sửa cả 5:**
- `streamDialer` làm rơi `Proxy: http.ProxyFromEnvironment` mà `websocket.DefaultDialer`
  vốn có → cả 9 connector WS lặng lẽ bỏ `HTTPS_PROXY`, trong khi Pyth (dùng
  `http.DefaultClient`) vẫn theo. Kiểu hỏng chỉ lộ ra ở máy người khác.
- `healthySession` (60s) **bằng đúng** `defaultReadTimeout` (60s) → một sàn nhận
  kết nối rồi im bặt bị read deadline giết ở đúng mốc đủ điều kiện, reset backoff
  **mỗi lần**, và bị quay số lại mỗi ~60s vĩnh viễn thay vì leo lên trần. Nay
  `healthySession = 2 × readTimeout` **và** phiên phải thực sự có dữ liệu mới được
  reset — socket không mang gì thì chưa từng hoạt động, dù mở bao lâu.
- Đếm reconnect khoá vào "đã báo cáo lần nào chưa" → sàn nào **quay số lần đầu
  thất bại** sẽ bị tính lần kết nối thành công đầu tiên là một lần reconnect.
- `shutdown` cấp cho HTTP server trọn `shutdownBudget` **rồi mới** bắt đầu đếm cho
  connector → tối đa 10s so với 5s đã ghi tài liệu, và số đo in ra (chính là con
  số nghiệm thu) tính cả phần HTTP. Nay một ngân sách chung, đếm từ lúc huỷ ctx.
- `invalidateFreshness` xoá `state` nhưng để nguyên `uptime_sec`/`reconnect_count`
  → tooltip vẫn khoe "kết nối liên tục 2g" sau khi chính socket của dashboard chết.

**Nợ được trả:** `RecvAt` nay đóng dấu **tại lúc đọc socket** (nợ ghi ở Bước 1.1);
`withRegistry` không còn nguy hiểm vì test dừng được goroutine của nó qua `ctx`
(nợ ghi ở Bước 1.4); Paradex nuốt lỗi subscribe bằng một thân `if` rỗng — nay lỗi
kết thúc phiên; Kraken giữ order book **xuyên qua reconnect**, mô tả một phiên
không còn tồn tại — nay `Subscribe` xoá sạch.

**Phát hiện ngoài phạm vi — ghi nhận, chưa sửa:**
- 🔴 **Pyth đã chết từ lâu và code cũ giấu điều đó.** `hermes.pyth.network` trả
  **401 Unauthorized**. Code cũ gọi `http.Get` mà **không kiểm status code**:
  scanner đọc thân lỗi, vòng lặp kết thúc, ngủ 5 giây, lặp lại — chỉ log "Pyth SSE
  connection closed". Đây là lý do thật của dòng ghi trong `config.yaml`: "suốt
  buổi quan sát Pyth không gửi gì". Nay 401 hiện rõ và backoff leo đúng
  2→4→8→16→32→60s. Sửa cần endpoint hoặc khoá mới → **Bước 2.x**, không phải ở đây.
- Kraken **có** gửi `timestamp` (ms) trong cả `book_snapshot` lẫn mọi delta — đo
  được. `processKrakenOrderbook` chỉ nhận sổ đã ráp nên chưa xuyên qua được; đây
  đúng là khoản nợ "venue time thật cho Bybit/Kraken/Paradex" đã ghi ở cuối GĐ 1.
- ~~`exchanges/` mới có test **vòng đời kết nối** (17 test, 20,8%), **chưa có test
  parse** cho cả 10 connector.~~ → đã xong ở Bước 1.6: 89 test, 63,8%.

#### Bước 1.6 — Bộ test đầu tiên ✅
- Tạo `exchanges/testdata/` — **phải chạy scanner và dump payload thật** của từng sàn trước, chưa có sẵn.
- Golden test: nạp payload mẫu → khẳng định parse ra đúng struct.
- Unit test: mid-price, tính phí, spread, staleness filter, chuyển đổi đơn vị.
- **Nghiệm thu:** `go test ./...` xanh, `-race` sạch; coverage ≥ 60% ở phần logic tính toán.

**Kết quả (2026-09-03).** `exchanges` từ **20,8% → 63,8%**; toàn dự án 211 test,
`-race -count=2` sạch. Phần còn 0% là chín hàm `ConnectX` một dòng (`runStream(f,
xStream(...))`) và vòng lặp mạng của Pyth — nối dây I/O, không phải logic tính.

| Gói | Test | Coverage |
|---|---|---|
| `exchanges` | 89 | 63,8% |
| `internal/scanner` | 84 | 89,3% |
| `internal/config` | 31 | 78,2% |
| `internal/fees` | 5 | 100% |
| `cmd/scanner` | 2 | — (chỉ đối chiếu config↔connector) |

**Đối chiếu P1: phần lớn danh sách "unit test" ở trên đã có rồi.** Mid-price,
tính phí, spread và staleness filter đã được phủ từ Bước 1.1 và 1.3
(`internal/scanner` 84 test, `internal/fees` 100%). Khoảng trống thật sự chỉ nằm ở
`exchanges/`: **không một dòng nào** chạm vào phần parse của 10 connector. Nên bước
này dồn toàn bộ vào đó, thay vì viết lại thứ đã có.

**Công cụ capture dùng chính `streamConfig` của production.** Mỗi connector được
tách thành `ConnectX()` gọi `xStream()`; `TestCaptureTestdata` dựng đúng
`streamConfig` đó — cùng URL, cùng message subscribe — và chỉ thay `Handle`. Một
công cụ capture tự viết lại message subscribe sẽ **trôi khỏi connector và ghi lại
payload không ai thật sự nhận**, đúng cái mà golden test sinh ra để chặn. Chạy lại
khi sàn đổi payload:

```
CAPTURE_TESTDATA=1 go test -run TestCaptureTestdata -timeout 5m ./exchanges/
```

**Bản capture đầu tiên hỏng, và cách nó hỏng đáng ghi.** Giữ 80 frame *đầu tiên*
nghĩa là kênh ồn ào bỏ đói kênh khác: OKX đẩy 76 frame trade trước frame `books5`
đầu tiên, nên file vàng của OKX **không có lấy một bản cập nhật sổ lệnh nào**; và
Paradex — vốn phát summary cho **mọi** thị trường nó niêm yết, kể cả quyền chọn —
cho ra một lát cắt ngẫu nhiên không chứa `BTC-USD-PERP`. Test chạy trên đó sẽ
XANH mà không khẳng định được gì. Nay capture giữ theo **dạng frame**
(`frameKind`): vân tay gồm trường phân loại của sàn cộng với thị trường đã đăng ký
mà frame nhắc tới, tối đa 8 frame mỗi dạng, chạy đủ 30 giây.

---

🔴 **Lỗi thật do golden test bắt được: mọi trade của Binance đều bị gắn nhãn
"sell".**

`encoding/json` ưu tiên khớp tag chính xác nhưng **có fallback khớp không phân
biệt hoa thường**. Payload aggTrade mang cả `"m"` (buyer is maker) lẫn `"M"` (cờ
bỏ đi, luôn `true`). Struct chỉ khai `m`. Nên: `"m":false` khớp chính xác → đúng;
rồi `"M":true` không có chỗ khớp chính xác, khớp hoa-thường vào `m` và **ghi đè**.
`IsMaker` luôn thành `true`, side luôn thành `sell`.

Cách lỗi này ẩn mình là điển hình: build xanh, parse không lỗi, giá đúng, khối
lượng đúng — chỉ một trường bool sai. Hiện chưa ai dùng `Side` (chỉ `markSourceAlive`
đọc trade), nên nó vô hại **hôm nay**; GĐ 3 backtest và GĐ 2 phân tích dòng lệnh
sẽ đọc nó.

Đã **quét toàn bộ 9 file golden** tìm mọi cặp key chỉ khác nhau hoa/thường:
`b`/`B`, `a`/`A` (Binance, Gate), `e`/`E` (Binance), `s`/`S` (Bybit). **Tất cả đều
đã khai đủ cả hai vế** nên khớp chính xác thắng — `m`/`M` là chỗ duy nhất hở. Sửa
bằng cách khai `Ignore bool \`json:"M"\``.

> **Quy tắc rút ra:** khai **cả hai** vế của mọi cặp key khác nhau chỉ ở hoa/thường,
> kể cả vế không dùng. Bỏ trống một vế không phải là "bỏ qua nó" — mà là "để nó ghi
> đè lên vế kia".

---

**Phát hiện thứ hai từ dữ liệu thật: `bookTicker` của Binance SPOT không có mốc
thời gian.** Bản futures mang `"E"`, bản spot không mang gì cả — cùng connector,
cùng tên stream, khác payload. Nên `venue_time_ms` bằng 0 cho sổ lệnh Binance spot
là **do sàn**, không phải do connector. Đã ghi vào bảng kỳ vọng của test.

**Test khẳng định cái gì.** Không phải "parse không lỗi", mà đúng những điều phần
còn lại của hệ thống *dựa vào* và *không tự kiểm được*: symbol phát ra phải là
symbol đã đăng ký (Paradex phát cả quyền chọn), `Source` là **cấu hình** chứ không
phải hằng số trong connector (lỗi Bước 1.4), dấu thời gian nhận sống sót, đồng hồ
sàn **không bao giờ** là đồng hồ của ta, và khối lượng phải bằng coin hoặc bằng 0
— **không bao giờ** là số contract đội lốt tên `...Coin`.

**Sáu đột biến, sáu lần đỏ** (phá cơ chế rồi xem test có bắt không): OKX điền số
contract vào trường `...Coin`; Hyperliquid tráo bid/ask; Paradex bỏ lọc symbol;
Kraken ngừng áp delta; Bybit điền đồng hồ ta vào `venue_time_ms`; Binance bỏ lại
trường `M`.

⚠️ **Lần đột biến Kraken đầu tiên KHÔNG đỏ — test rỗng, lần thứ ba trong dự án.**
Test khẳng định "top of book có đổi", nhưng so sánh **giữa hai symbol khác nhau**
(BTC và ETH), nên "có đổi" luôn đúng kể cả khi delta không được áp. Sửa thành so
theo từng symbol. Đây chính là lý do mọi test quan trọng đều phải bị phá thử.

**Pyth không có payload thật.** `hermes.pyth.network` trả **401** ở cả SSE lẫn REST
(phát hiện ở Bước 1.5). Fixture của Pyth vì thế là **TỔNG HỢP** và được ghi rõ như
vậy trong `pyth_test.go`: nó chứng minh số học (expo, giây→ms) và luồng định
tuyến, **không** chứng minh định dạng hiện tại của Pyth còn khớp `PythSSEResponse`.
Phải kiểm lại bằng payload sống trước khi tin oracle này lần nữa.

**Bybit: bản ghi chỉ có `snapshot`, không có `delta` nào** trong suốt cửa sổ quan
sát ở cả hai stream. Lỗi snapshot/delta (ghi ở CLAUDE.md) vì thế **vẫn mở** và
chưa có dữ liệu vàng để sửa. `TestBybit_TheRecordingContainsOnlySnapshots` ghim
đúng điều đó: hôm nào capture lại bắt được delta, test đỏ — và lúc đó đã có sẵn dữ
liệu để sửa.

**Review tìm 6 lỗi; sửa 4, ghi nợ 2.**

Sửa:
- `time.After(time.Until(deadline))` **âm** khi `server.Shutdown` đã tiêu hết ngân
  sách → `select` chọn **ngẫu nhiên** giữa timer đã hết và channel đã đóng, in ra
  `WARNING: connectors still running` về những connector vừa dừng trong vài chục
  micro giây — đúng dòng log mà tiêu chí nghiệm thu đọc từ đó.
- Pyth dùng `http.DefaultClient`, **không có timeout đọc header**, mà watchdog chỉ
  được lên dây *sau khi* response về. Một server bắt tay TLS xong rồi im sẽ treo
  goroutine Pyth vĩnh viễn: không lỗi, không sự kiện, không thử lại.
- Pyth đọc SSE bằng `bufio.Scanner` với hạn mặc định **64KB**, trong khi một dòng
  `data:` lớn dần theo số feed id trong `config.yaml`. Thêm đủ feed là mọi phiên
  chết ngay dòng đầu với `ErrTooLong` — vòng lặp reconnect vĩnh viễn sinh ra từ
  một lần sửa YAML. (Chính `readFrames` trong test đã nâng hạn này và ghi lý do,
  còn production thì chưa — để nguyên thì không biện minh được.)
- Lý giải của việc dời `RecvAt` bị **viết ngược** ở 5 chỗ. Đúng phải là: đóng dấu
  lúc *lấy khỏi hàng đợi* làm **đồng hồ chạy lại từ đầu**, nên scanner đang ùn tắc
  báo mọi sàn vừa mới cập nhật trong khi phục vụ giá cũ vài giây. Đóng dấu tại
  socket gộp độ trễ hàng đợi vào tuổi dữ liệu — nên ùn tắc **hiện ra** thành
  staleness thay vì bị giấu. Vị trí code vẫn đúng; chỉ lời giải thích sai.

**Nợ ghi nhận, chưa xử lý:**
- 🔴 **Subscription bị sàn âm thầm huỷ thì không có gì buộc nối lại.** Read deadline
  được gia hạn bởi **mọi** frame, mà Bybit/OKX/Hyperliquid trả lời keepalive bằng
  frame **dữ liệu**. Nên một socket còn mở nhưng đã mất subscription sẽ không bao
  giờ bị dựng lại: scanner *phát hiện* được (suy luận từ im lặng hạ trạng thái
  xuống `disconnected`) nhưng **không hành động được**. Đúng dạng hỏng mà phiên
  72h sinh ra để loại trừ. Cách sửa cần watchdog dữ liệu trong connector, hoặc
  giới hạn tuổi phiên rồi nối lại định kỳ.
- `framesRead` trong `shouldResetBackoff` đếm cả frame keepalive, nên trên ba sàn
  đó một phiên chỉ toàn pong vẫn đủ điều kiện reset backoff. Muốn phân biệt thì
  `Handle` phải báo lại nó có sinh ra dữ liệu thị trường hay không.
- Bản ghi `binance_futures` **không có frame aggTrade nào** trong 30 giây. Probe
  sau đó không kết luận được: fstream ngừng gửi *mọi thứ* cho host này (giống bị
  giới hạn số kết nối sau đợt capture), và lần chạy scanner sau đó thì Binance
  futures hoạt động bình thường. Phần parse trade của nó được phủ qua
  `binance_spot`, chung handler.

> **Nợ kỹ thuật ghi nhận, chưa xử lý ở GĐ này:** tầng broadcast (xem [§7.3](#73-ngưỡng-mở-rộng-của-tầng-broadcast)); lấy venue time thật cho Bybit/Kraken/Paradex; ngưỡng staleness thích ứng.
>
> **Từ review độc lập Bước 1.0 (2026-09-03) — ✅ đã xử lý xong (2026-09-03):**
> - ~~**F2** — `serverNow()` được định nghĩa kèm `clockOffsetMs` nhưng 0 caller~~ → đã hết cùng F3: `serverNowMs()` giờ là nguồn thời gian cho chart, đếm tuổi alert và `formatTimeAgo` (vào ở `7867fe2`).
> - ~~**F3** — điểm biểu đồ lấy mốc `Date.now()`~~ → `addPriceToHistory` và `breakChartLine` dùng `serverNowMs()`, có chặn thời gian không tăng khi offset được ước lượng lại (vào ở `7867fe2`).
> - ~~**F4** — `lastUpdate: Date.now()` ghi vào state nhưng không ai đọc~~ → xoá field ở commit dọn dẹp đầu GĐ 2.

---

### GIAI ĐOẠN 2 — FUNDING RATE MONITOR

**Mục tiêu:** Thu thập và lưu trữ funding rate 24/7 — nguyên liệu bắt buộc của chiến lược.
**Thời gian:** 4–5 tuần · **7 bước**
**📄 Chi tiết dữ liệu:** [DATA-REQUIREMENTS.md](DATA-REQUIREMENTS.md)

> **Cập nhật sau khảo sát 7 sàn (2026-08-28):** giai đoạn này tăng từ 5 lên 7 bước. Lý do: khảo sát cho thấy funding rate **không đồng nhất giữa các sàn** ở mức phá vỡ thiết kế struct ban đầu (OKX đảo ngược ngữ nghĩa `next`, Kraken trả giá trị tuyệt đối thay vì tỷ lệ, ba sàn dùng ba đơn vị khác nhau cho chu kỳ, Paradex không có mốc funding rời rạc). Đồng thời instrument registry và bảng ánh xạ spot↔perp được kéo từ GĐ 4 lên đây, vì không có chúng thì không tính được size delta-neutral để backtest cho đúng.

#### Bước 2.1 — Script xác minh field (làm TRƯỚC khi viết struct) 🔍 ✅
- Script một lần: đọc funding rate BTC từ cả 7 sàn, in cạnh nhau, đối chiếu với số hiển thị trên web từng sàn.
- Mục đích: phát hiện sai đơn vị / sai field **trước khi** nó đi vào logic tính toán.
- Xác minh cụ thể 4 điểm còn nghi vấn: OKX `fundingTime` vs `nextFundingTime`; Gate `funding_interval` đơn vị giây; Paradex mô hình liên tục; Kraken `funding_rate` tuyệt đối vs `relative_funding_rate`.
- **Nghiệm thu:** 7 con số khớp với web sàn. Sàn nào không khớp → tìm ra nguyên nhân trước khi đi tiếp.

> **Kết quả (2026-09-03).** `cmd/fundingcheck` đọc cả 7 sàn song song, in raw cạnh
> chuẩn hoá, tự chấm 8 verdict (mỗi sàn một, cộng kiểm biên độ chéo sàn) — tất cả
> PASS. Verdict khẳng định **đơn vị và ngữ nghĩa**, không ghim cấu hình hôm nay;
> kiểm chéo sàn so |rate/8h| với **trung vị**, phát hiện mọi lệch hệ số **≥5×**
> (÷8/×8, 60×, absolute-vs-relative ~10⁵×) và chấp nhận funding âm/lẫn dấu.
> Cả 4 điểm 🟡 xác minh xong, và **một điểm phải sửa tài liệu**:
> `relative_funding_rate` của Kraken là rate **mỗi-1h dùng nguyên, KHÔNG chia 8**
> — khảo sát cũ ghi ngược. Ba phát hiện mới: OKX `nextFundingRate` giờ trả rỗng
> (`method=current_period`); Binance `fundingInfo` hiện phủ **toàn bộ** perp
> TRADING (777 symbol, BTCUSDT trong đó vì cap ±0,3% bị chỉnh) với interval
> 4h/8h/**1h** — 4h chiếm đa số; Bybit REST ticker thêm `fundingIntervalHour`
> (GIỜ) — đơn vị thứ tư cho cùng khái niệm. Vế "đối chiếu web sàn" bằng mắt được
> thay thế có lý do — phương pháp thay thế và toàn bộ chi tiết ở
> [DATA-REQUIREMENTS.md §3 + phụ lục](DATA-REQUIREMENTS.md#3-khảo-sát-funding-rate-7-sàn).

#### Bước 2.2 — Thiết kế `FundingData` + refactor `Feeds` ✅
- Struct đầy đủ tại [DATA-REQUIREMENTS.md §4](DATA-REQUIREMENTS.md#4-thiết-kế-fundingdata). Ba yêu cầu bắt buộc:
  1. `FundingModel` enum (`discrete` / `continuous`) — Paradex không có mốc funding.
  2. Giữ **cả** `RawRate` (debug) **lẫn** rate chuẩn hoá (`RatePerInterval`, `RatePer8h`, `APRAnnualized`).
  3. `IntervalSec` — chuẩn hoá về **giây ngay tại tầng connector**. Tầng signal không bao giờ thấy phút/giờ/giây lẫn lộn.
- ~~Gom 3 channel thành `Feeds`~~ — đã thực hiện ở **Bước 1.5**. Ở đây **thêm** field `Funding chan<- FundingData` vào `Feeds` (kiểm 2026-09-03: struct hiện chỉ có `Ctx`/`Price`/`Orderbook`/`Trade`/`Conn` — channel Funding **chưa** được khai báo trước) rồi nối vào scanner.
- **Nghiệm thu:** unit test chuyển đổi đơn vị cho cả 7 sàn từ payload mẫu.

> **Kết quả (2026-09-03).** `exchanges/funding.go` giữ `FundingData` (tên field
> theo CONVENTIONS §1: `RatePer8hFrac`, `APRFrac`, `NextFundingAtMs`…) và
> `deriveFundingRates`; mỗi sàn một builder `normalize<Venue>Funding` trong
> `<venue>_funding.go` (CONVENTIONS §4) nhận trọn envelope
> `(symbol, source, recvAt, venueTimeMs, …)` để không thể tạo reading thiếu
> định danh. `Feeds` thêm đúng một field + `SendFunding`; scanner giữ bản ghi
> mới nhất theo symbol × source (chặn symbol ngoài config, chuẩn hoá `RecvAt`
> như `updatePrice`). Nghiệm thu: test chuyển đổi đơn vị cả 7 sàn từ giá trị
> payload thật 2026-09-03, chạy cùng `-race`.
>
> **Review adversarial tìm ra lần sửa khảo sát thứ hai:** Kraken WS
> `next_funding_rate_time` là **epoch ms tuyệt đối**, không phải "ms còn lại"
> như DATA-REQUIREMENTS từng ghi (badge ✅ cũ dán nhầm — 2.1 chỉ kiểm REST,
> field này chỉ có trên WS). Hai probe WS độc lập cùng xác nhận; thiết kế theo
> bản cũ sẽ ra mốc settle năm ~2083 và tầng đếm settle kết luận Kraken không
> bao giờ settle. Chi tiết: [DATA-REQUIREMENTS §3.3⑥](DATA-REQUIREMENTS.md).
>
> **Nợ ghi nhận từ review, chưa xử lý:**
> - ~~Chu kỳ 1h của Kraken ghim trong `kraken_funding.go`~~ ✅ **trả ở 2.6.**
>   Sàn vẫn không công bố interval ở message funding nào, nhưng
>   `historicalfundingrates` giờ có bản ghi trong `exchanges/testdata/` và
>   `TestKrakenFundingHistoryGolden` chốt modal gap == 3600 — re-record là tự
>   kiểm lại hằng số. Đo cả năm 2026-09-04: 3600s×8744, 7200s×6, 10800s×1.
> - Map funding của scanner không tự loại reading cũ; **Bước 2.7 phải đo độ
>   tươi từ `RecvAt` khi hiển thị** (subscription chết mà hiển thị rate cũ như
>   mới là đúng lỗi 1.6 lặp lại ở tầng UI).
> - `updatePrice` không chặn symbol ngoài config (phơi bày sẵn có, trước 2.2;
>   `updateFunding` đã chặn) — xử lý khi đụng tầng đó.
> - Một loạt điểm bền vững của `cmd/fundingcheck` (guard interval Binance/Bybit,
>   median khi số sàn chẵn, so timestamp bằng parse thay vì thứ tự chuỗi,
>   `slices.Sort` thay sort tay, verdict Gate bớt ghim hình dạng "chia hết cho
>   3600") — commit `fix(fundingcheck)` riêng ngay sau bước này.

#### Bước 2.3 — Instrument registry ✅
- Tải `exchangeInfo` (spot + futures) 1 lần/ngày, cache: `tickSize`, `stepSize`, `minNotional`, `status`, contract size/multiplier, leverage brackets.
- Contract size khác nhau: OKX `ctVal × ctMult`, Gate `quanto_multiplier`, Kraken theo spec — các sàn này đặt lệnh theo **số contract**, không theo coin.
- Hàm tính size delta-neutral: làm tròn **xuống** theo `stepSize` **lớn hơn** của hai chân, kiểm `minNotional` của **cả hai** rồi mới đặt.
- **Nghiệm thu:** cho notional bất kỳ → ra size 2 chân hợp lệ ở mọi sàn, hoặc từ chối có lý do rõ ràng.

> **Kết quả (2026-09-03).** 9 fetcher `Fetch<Venue>Instruments` trong
> `exchanges/<venue>_instruments.go` (mọi khối lượng quy về COIN, sàn contract
> giữ `IsContract` + `ContractSizeCoin`), golden test trên response thật
> (`testdata/instruments_*.json`, ghi lại bằng `CAPTURE_TESTDATA=1`);
> `internal/instruments.Registry` refresh 24h — nguồn hỏng giữ số hôm qua và
> bị nêu tên, refresh thành công thay trọn map nguồn (market bị gỡ không sống
> mãi); `SizeDeltaNeutral` đúng thuật toán đề ra, thêm kiểm size nằm trên lưới
> **cả hai** chân và `MaxQtyCoin`. Nghiệm thu chạy hai lớp: test sizing đủ
> 7 perp × 2 spot trên số liệu thật + quét notional liên tục; chạy sống cổng
> 8085: **"instrument registry: 36 instruments across 9 sources"** — đủ 4 cặp
> × 9 nguồn, không lỗi fetch. **Giải xong mâu thuẫn Kraken của 1.2** (xem
> [DATA-REQUIREMENTS §5](DATA-REQUIREMENTS.md#5-instrument-registry--✅-xây-ở-bước-23-2026-09-03)):
> PF_ đặt theo contract nhưng 1 contract = 1 đơn vị base — khảo sát lẫn phép đo
> đều đúng. Phát hiện đáng giá: **PF_XRPUSD và Hyperliquid XRP giao dịch XRP
> NGUYÊN từng con** (precision/szDecimals = 0) — bước lưới thô nhất hệ nằm ở
> XRP, không phải BTC. Leverage bracket Binance cần chữ ký → GĐ 4
> (`MaxLeverageX = 0`, các sàn khác đọc công khai được).
>
> **Review 10-angle; các lỗi thật đã sửa trong bước:** fetch trả 0 instrument
> từng xoá cache và đóng dấu như thành công; Gate 404 một contract từng giết cả
> nguồn; Binance spot `?symbols=[...]` chết cả request vì 1 symbol lạ (đổi sang
> fetch toàn bộ + lọc local); Bybit/OKX gói lỗi trong HTTP 200 không được đọc;
> chân spot dạng contract từng bị phát ra coin im lặng; MinQty bịa "một bước"
> cho Kraken/Paradex (giờ 0 = không công bố, sizing tự giữ sàn một-bước);
> refresh tuần tự 9 nguồn × timeout 30s; `TickSize` thiếu hậu tố đơn vị
> (→ `TickSizeQuote`); tham số sizing 3 float trần dễ hoán vị (→
> `SizingRequest`); comment mồ côi `fundingFrom*` sau đổi tên 2.2.
>
> **Nợ ghi nhận từ review, chưa xử lý:**
> - Bảy builder funding (commit 2.2) nhận 7 tham số vị trí — hai `int64` kề
>   nhau hoán vị được mà vẫn biên dịch (CONVENTIONS §6.2: >4 → struct). Gom
>   thành struct khi 2.5 chạm đúng các call site đó.
> - ~~Cadence 1h Kraken: ghi bản ghi `historicalfundingrates` vào testdata +
>   golden khoảng cách == 3600~~ ✅ **làm ở 2.6** như đã hẹn.
> - `cmd/fundingcheck`: verdict OKX dùng `<` chặt — chạy đúng mốc settle có
>   thể false-FAIL; verdict Gate tin đồng hồ máy (lệch >60s false-FAIL). Sửa
>   khi có dịp đụng công cụ.
> - Fetcher theo-symbol (Bybit/OKX/Gate/Paradex) gọi tuần tự trong nguồn —
>   đủ cho 4 cặp; vượt ~10 cặp thì chuyển sang endpoint danh sách của sàn.
> - `TickSizeQuote = 0` (Hyperliquid, quy tắc 5 chữ số có nghĩa) là sentinel;
>   GĐ 4 đặt lệnh cần mã hoá quy tắc thành dữ liệu (`PxDecimals`/`SigFigs`).

#### Bước 2.4 — Bảng ánh xạ spot ↔ perp ✅
- Dựng **tự động** từ `exchangeInfo`, ~~thay `switch` hardcode hiện tại trong từng connector~~ (tiền đề cũ — hardcode đã chết ở Bước 1.4; cái 2.4 thật sự xây là tầng XÁC THỰC trên registry 2.3).
- Xác thực hai chiều; cặp không ghép được → **từ chối, không đoán**.
- Chú ý Kraken `PF_XBTUSD` quote là **USD** không phải USDT → không hedge thẳng bằng spot USDT.
- ~~🐛 Sửa bug `coin := symbol[:3]` ở Hyperliquid~~ → ✅ **đã xong ở Bước 1.4** như hệ quả của việc đưa ánh xạ ký hiệu vào `config.yaml`: Hyperliquid dùng `symbol_format: "{base}"`, và nghiệm thu đã chạy thật với `DOGEUSDT` (base 4 ký tự).
- **Nghiệm thu:** thêm 1 cặp mới → bảng tự dựng đúng trên mọi sàn hỗ trợ, tự loại sàn không hỗ trợ.

> **Kết quả (2026-09-03).** `Instrument` mang thêm `BaseAsset`/`QuoteAsset` do
> **sàn tự khai** (Kraken tự khai base "BTC" cho PF_XBTUSD → không cần bảng
> alias XBT; OKX swap để RỖNG `baseCcy`/`quoteCcy`, dùng `ctValCcy`/`settleCcy`;
> Hyperliquid quote là hằng "USD" có tài liệu). `BuildHedgeMapping`
> ([internal/instruments/mapping.go](../internal/instruments/mapping.go)) là hàm
> thuần: xác thực config↔sàn hai chiều (base theo symbol, quote + market type
> theo nguồn, một-chợ-native-một-symbol), ghép spot×perp cùng quote, và xuất
> `Rejections` hạng nhất — perp USD giữa toàn spot USDT bị từ chối nêu tên,
> cả hai hướng. `cmd/scanner` dựng lại bảng sau mỗi refresh, log khi đổi.
> Nghiệm thu chạy sống cổng 8085: thêm `DOGEUSDT` → 8 hedge pairs tự dựng +
> 3 từ chối quote USD; thêm `XLMUSDT` (Paradex không niêm yết) → Paradex
> **tự loại bằng vắng mặt** (53 = 6×9−1). Lần chạy đó bắt được lỗi thật:
> Paradex trả **404** cho market lạ và fetcher chết cả nguồn — sửa cùng bước,
> kèm hai hình dạng "không niêm yết" khác (OKX `code 51001`; Bybit linear
> `retCode 10001` "symbol invalid", spot lại trả `retCode 0` + list rỗng).
> Chi tiết ở [DATA-REQUIREMENTS.md §6](DATA-REQUIREMENTS.md#6-bảng-ánh-xạ-spot--perp--✅-xây-ở-bước-24-2026-09-03).
>
> **Review 10 góc tìm và đã SỬA trong bước:** ① `strings.ToUpper` ở từng
> fetcher là sai hướng — Hyperliquid có 7 thị trường chữ lẫn (`kPEPE` = 1000
> PEPE) nên viết hoa là bịa tài sản, mà `config.yaml` cũng không thể viết hoa
> `base:` (nó là định danh sàn khi `symbol_format: "{base}"`); chuyển sang giữ
> nguyên văn hai phía + `sameAsset`/`EqualFold` sở hữu luật hoa-thường ở chỗ so
> sánh. ② Bybit khớp `retMsg` phân biệt hoa-thường → đổi cách viết là cả nguồn
> `bybit_futures` chết vĩnh viễn; nay gấp chữ + khớp hai từ rời. ③ OKX/Bybit
> chưa tôn trọng sentinel 404 như Gate/Paradex → đã thêm. ④ Vắng mặt im lặng
> khắp nơi làm lỗi gõ `symbol_map` vô hình → `Refresh` nêu tên symbol không
> quay về; thông điệp "0 instrument" nêu cả hai cách đọc. ⑤ `nativeClaims` đếm
> bản ghi nên chẩn đoán sai khi input trùng lặp → đếm số symbol PHÂN BIỆT.
> ⑥ Nhánh `default:` trong vòng ghép không thể chạm tới → chuyển việc từ chối
> market type không hedge được lên chuỗi validation (nơi mọi từ chối khác ở).
> ⑦ Phát hiện-thay-đổi so chuỗi log → so cấu trúc (`reflect.DeepEqual`), vì
> log chỉ in tên nguồn nên sàn đổi `stepSize` sẽ render y hệt.
>
> **Nợ ghi nhận (ngoài phạm vi 2.4):**
> - `SourceClaim`/`PairAssets` là bản sao cấu trúc của `config.Source`/
>   `config.Symbol` — cố ý để `internal/instruments` không phụ thuộc
>   `internal/config`, nhưng thêm field phải sửa ba nơi. Xem lại khi 2.7 cần
>   thêm trường (Paradex quote USD nhưng settle USDC).
> - Bốn vòng fetch theo-symbol (Bybit/OKX/Gate/Paradex) lặp cùng một khuôn
>   lặp-bỏ-qua-vắng-mặt → gom thành một helper chung khi thêm sàn thứ 5, hoặc
>   khi 2.7 cần đếm/log số vắng mặt.
> - `afterRefresh` gắn ở vòng lặp `Run`, không phải ở `Refresh` — một caller
>   gọi thẳng `Refresh` (nút "làm mới ngay" của 2.7) sẽ đổi registry mà không
>   dựng lại mapping. 2.7 quyết định `Subscribe` hay notify trong `Refresh`.
> - Từ vựng market type (`spot`/`perp`) hiện khai ở 4 package không có ràng
>   buộc biên dịch nào nối chúng.

#### Bước 2.5 — Thu thập funding rate ✅
- **WebSocket** (ưu tiên): ~~Binance `@markPrice@1s`~~ (**không đẩy dữ liệu tới môi trường này** — đo 2026-09-04: 4.782 frame bookTicker và 0 markPriceUpdate trên cùng socket trong 45s; chuyển sang REST `premiumIndex`, xem [DATA-REQUIREMENTS §3.4⑦](DATA-REQUIREMENTS.md)), Bybit `tickers`, OKX `funding-rate`, Gate `futures.tickers`, Kraken `ticker`, Hyperliquid `activeAssetCtx`, Paradex `funding_data.{market}`.
- ⚠️ Bybit ticker là **snapshot + delta** — field vắng mặt nghĩa là *chưa đổi*, phải merge vào cache, không ghi đè.
- ⚠️ Binance `fundingInfo` **theo docs chỉ trả symbol lệch mặc định** (thực tế 2026-09-03 phủ 100% — vẫn phải mặc định 8h rồi ghi đè, không đọc ngược lại; xem [DATA-REQUIREMENTS §3.3](DATA-REQUIREMENTS.md)).
- **Nghiệm thu:** funding rate 4 cặp × 7 sàn realtime, khớp số `cmd/fundingcheck` đọc qua REST tại cùng thời điểm (và web sàn khi đối chiếu được bằng mắt), đã chuẩn hoá về `RatePer8hFrac` so sánh được chéo sàn.

> **Kết quả (2026-09-04).** Chạy sống cổng 8085: **28 reading = 4 cặp × 7 sàn**,
> tuổi mọi reading < 30s. Đối chiếu BTC với `cmd/fundingcheck` (đọc REST, cố ý
> không dùng chung code) tại cùng thời điểm: binance +0,8678 / bybit +0,6233 /
> gate +0,6300 / kraken −1,1839 / okx +0,7151 bps/8h — **khớp đến chữ số cuối**;
> hyperliquid (+0,6142 vs 0,6047) và paradex (+0,7519 vs 0,7481) lệch nhỏ đúng
> bằng bản chất tích luỹ liên tục của hai sàn này giữa hai lần đọc. Kraken và
> Hyperliquid hiển thị `every 3600s`, năm sàn còn lại `28800s` — đúng chu kỳ
> thật của từng sàn.
>
> **Ba phát hiện đổi thiết kế** (chi tiết + số đo ở [DATA-REQUIREMENTS §3.4](DATA-REQUIREMENTS.md)):
> ⑦ Binance `@markPrice@1s` không đẩy gì → REST `premiumIndex` theo từng symbol
> (dạng không lọc là 198.811 byte ≈ 1,1 GB/ngày cho 4 dòng, weight 10 so với 1).
> ⑧ Hyperliquid `nextFundingTime` là mốc kỳ **đang chạy** (đo qua ranh giới giờ:
> 02:47→02:00, 03:01→03:00, trong khi BinPerp/BybitPerp cùng response trả
> 08:00) → cộng một chu kỳ. ⑨ Bybit `fundingIntervalHour` là **chuỗi** `"8"`;
> khai `*int64` làm hỏng cả decode và Bybit im lặng không phát funding nào.
>
> **Review 10 góc tìm và đã SỬA trong bước:** Bybit publish trên MỌI delta
> (~40 msg/s, và mỗi lần làm tươi `RecvAt` — phá đúng cơ chế phát hiện
> subscription chết mà 2.7 cần) → chỉ publish khi trường funding đổi thật;
> `HasCap` bật kèm floor = 0 (đọc thành "không bao giờ âm") → tách `HasFloor`;
> một symbol lỗi giết cả vòng poll Binance → `continue` + `errors.Join`;
> goroutine `fundingInfo` rò khi shutdown → `WaitGroup`; Kraken unmarshal 2 lần
> mỗi frame (sổ ~1.800 mức) → dispatch trên `feed` đã decode; Gate so sánh
> rate bằng CHUỖI → parse số; `withFunding` suy từ hậu tố URL → tham số tường
> minh; log APR không nhãn gross (luật 2) → `APR_gross` + tiêu đề GROSS.
>
> **Nợ ghi nhận:** OKX/Gate/Paradex vẫn speculative-unmarshal mỗi frame như
> Kraken trước đây — gom về một envelope dispatch khi 2.7 đụng các connector
> này. `fundingLogEvery` 60s là cửa sổ quan sát duy nhất của 2.5; 2.7 thay bằng
> dashboard và khi đó nên bỏ hoặc hạ tần suất log.

#### Bước 2.6 — Persistence (SQLite) ✅
- `funding_history(exchange, symbol, raw_rate, rate_per_8h, interval_sec, funding_time, rate_type, recorded_at)`.
- `instruments` (snapshot hằng ngày — để biết `stepSize` đã đổi lúc nào), `price_snapshots` (lấy mẫu 1–5s).
- Backfill lịch sử 6–12 tháng qua REST; lọc `rateType = Special` khi backtest.
- Lưu trữ: 12 tháng funding, 3 tháng price snapshot.
- **Nghiệm thu:** restart không mất dữ liệu; truy vấn được funding 30 ngày; có đủ corpus cho backtest GĐ 3.

> **⚠️ Đối chiếu P1 (2026-09-04) — bốn giả định trên KHÔNG đúng như viết.**
> Đo trực tiếp 7 endpoint lịch sử funding trước khi code (chi tiết + payload
> thật: [DATA-REQUIREMENTS §8](DATA-REQUIREMENTS.md)):
>
> 1. **"6–12 tháng" không đồng đều — hai sàn không thể.** Binance, Bybit,
>    Hyperliquid, Kraken trả đủ ≥12 tháng. **OKX chỉ giữ ~3 tháng**
>    (`after` lùi 90 ngày còn dữ liệu, lùi 120 ngày trả mảng rỗng). **Gate
>    chặn cứng 180 ngày** (`from time exceeds 180-day limit`). Backfill phải
>    khai báo độ phủ THẬT theo từng sàn, không được để GĐ 3 tưởng 7 sàn cùng
>    dài như nhau.
> 2. **Paradex không có settlement để backfill.** `/v1/funding/data` là mẫu
>    funding index mỗi **5 giây** (5.000 bản ghi/trang = 6,9 giờ). 6 tháng ở
>    độ phân giải gốc là ~3,1 triệu dòng/market. Vì index là **luỹ kế**, lấy
>    mẫu theo giờ là đủ và chính xác — backfill Paradex đi theo cửa sổ
>    `start_at`/`end_at` từng giờ, và độ phủ mặc định ngắn hơn sáu sàn kia.
> 3. **Tên cột trong dòng trên vi phạm CONVENTIONS §1.** `raw_rate`,
>    `rate_per_8h`, `funding_time`, `recorded_at` không mang đơn vị, mà cột
>    SQLite là định danh vượt ranh giới (GĐ 8 Python đọc thẳng). Schema thật
>    dùng `rate_per_8h_frac`, `funding_at_ms`, `recorded_at_ms`, …
> 4. **`price_snapshots` 1–5s không khả thi — ĐO THẬT, không ước lượng.**
>    Một dòng tốn **133,3 byte** trên đúng schema này
>    (`MEASURE_STORE=1 go test -run TestPriceSnapshotRowCost ./internal/store/`,
>    180.000 dòng, 2026-09-04). Với 36 chuỗi và 90 ngày lưu trữ: 5s →
>    56,0 triệu dòng ≈ **7,46 GB**; 10s ≈ 3,73 GB; **30s ≈ 1,24 GB**; 60s ≈
>    0,62 GB. Vị thế funding giữ hàng NGÀY nên 30s vẫn cho 2.880 mẫu/ngày/chuỗi.
>    Số **đang dùng là 30s**, khai trong `config.yaml`, và test config chặn
>    khoảng 10–60s để lần sửa sau không lặng lẽ quay về 5s.
>
> Ngoài ra `interval_sec` của một dòng lịch sử **không đọc được từ payload**:
> không sàn nào công bố interval kèm từng mốc settle (trừ Paradex).
> Nó được suy từ **khoảng cách mốc settle đo được**, và mỗi dòng giữ thêm
> `gap_prev_sec` = khoảng cách thật tới mốc trước, để một kỳ settle bị bỏ lỡ
> hay một lần đổi cadence hiện ra trong DỮ LIỆU chứ không nằm trong comment.

> **Kết quả (2026-09-04).** Ba package mới và một lệnh mới:
> `internal/store/` (SQLite qua `modernc.org/sqlite` thuần Go — giữ được build
> `CGO_ENABLED=0`, không cần toolchain C ở máy chạy), `internal/history/`
> (REST sàn → store, dùng chung giữa scanner và backfill, và là chỗ DUY NHẤT
> biết cả hai đầu), `cmd/backfill/`, cùng 7 fetcher
> `exchanges/<venue>_funding_history.go`. Ba bảng: `funding_history`
> (khoá `source+symbol+funding_at_ms`), `price_snapshots`,
> `instrument_snapshots` (khoá theo NGÀY UTC). `FundingHistoryEntry` **không**
> mang `RecvAt` — luật 13 chỉ cho ba chỗ đóng dấu, một cho mỗi transport, và
> một rate settle từ tháng Ba không có "thời điểm nhận"; thời điểm GHI là việc
> của store (`recorded_at_ms`).
>
> **Nghiệm thu chạy thật, không khẳng định suông:**
> 1. *Restart không mất dữ liệu* — `cmd/backfill` nạp 22.518 dòng cho BTCUSDT;
>    scanner khởi động lại trên đúng file đó, top-up lấy về 28 dòng chồng lấn
>    của Kraken/Hyperliquid/Paradex và **ghi thêm đúng 1** (mốc settle mới),
>    bốn sàn 8h không ghi thêm dòng nào. WAL checkpoint sạch (0 byte) khi tắt.
> 2. *Truy vấn được funding 30 ngày* — `go run ./cmd/backfill -check 30`:
>    **2.517 mốc settle** đọc qua `store.FundingHistory` (đúng đường truy vấn
>    GĐ 3 sẽ dùng), đủ 7 sàn, trung bình 0,44–0,82 bps/8h — cùng bậc độ lớn,
>    không sàn nào lệch đơn vị.
> 3. *Đủ corpus* — độ sâu THẬT theo từng sàn, đo trên BTCUSDT 12 tháng:
>    Binance/Bybit/Kraken/Hyperliquid **365 ngày**; Gate **178,7**; OKX
>    **92,7**; Paradex **83,3** (trần page budget của công cụ, không phải của
>    sàn). Báo cáo in thẳng ba nhóm khác nhau — *tới không đủ xa*, *nhịp lệch
>    vài giây do jitter mốc*, và *hai cadence cùng có trọng số* (nhóm cuối mới
>    là cảnh báo ⚠️: `interval_sec` là modal nên nửa kia của kho bị sai 2× ở
>    `rate_per_8h_frac`).
> 4. Scanner chạy sống cổng 8085 (soak 8082 không đụng tới): mở store, chụp
>    36 instrument sau 2 phút, 216 dòng giá = 6 vòng × 36 chuỗi mỗi 30s, tắt
>    sạch trong **256µs**, không cảnh báo storage.
>
> **Bốn phát hiện đổi thiết kế, chi tiết + số đo ở
> [DATA-REQUIREMENTS §9](DATA-REQUIREMENTS.md):** ① độ sâu lịch sử không đồng
> đều (OKX ~3 tháng, Gate 180 ngày); ② Paradex không có settlement, chỉ có mẫu
> index mỗi 5 giây, nên lấy mẫu theo GIỜ (index luỹ kế nên không mất gì);
> ③ `interval_sec` phải ĐO chứ không đọc, và một lần đổi cadence sai 2× suốt
> nhiều tháng nên `gap_prev_sec` phải nằm cạnh nó; ④ giá 1 dòng price sample
> **133,3 byte** → 5s × 90 ngày = 7,46 GB, nên chu kỳ dùng thật là 30s.
>
> **Review tự tìm và đã SỬA trong bước:** query `(? = '' OR symbol = ?)` khiến
> SQLite bỏ qua index — dựng câu lệnh theo điều kiện; ảnh chụp instrument và
> prune tick lần đầu sau nguyên một chu kỳ (6h/24h) nên máy restart hằng ngày
> **không bao giờ** chụp và **không bao giờ** dọn — thêm warm-up 2 phút / 5 phút;
> chờ recorder lúc tắt không có hạn — thêm ngân sách 5s như phía connector;
> đường DB chứa `?` bị driver đọc thành tham số DSN → mở nhầm file, giờ từ chối;
> thị trường sàn không niêm yết trả 404 / OKX `51001` / Bybit `10001` bị coi là
> lỗi → giờ là "không có", dùng chung đúng một hàm nhận diện với Bước 2.4;
> `LogResults` in "0 series" mỗi lần tắt; **một request lỗi làm hỏng cả chuỗi** —
> một chuỗi Paradex là 2.000 request và 9 phút, mất trắng vì request thứ 1.999
> trả 502 là đánh đổi không ai chọn, nên mỗi trang được thử lại 3 lần (không thử
> lại "sàn không niêm yết" và không thử lại khi ctx đã huỷ), và `cmd/backfill`
> exit ≠ 0 khi còn chuỗi hỏng vì top-up hằng giờ CHỈ lấp phần mới, không lấp lại
> năm cũ.
>
> **Nợ ghi nhận:** `fetchInstrumentJSON` giờ phục vụ cả instrument, funding REST
> và lịch sử funding — tên nói dối, đổi khi có bước chạm đủ rộng vào
> `exchanges/`. Top-up Kraken tải 1 MB/cặp/giờ vì sàn luôn trả cả năm bất kể
> cửa sổ (không tránh được, đã ghi vào `config.yaml` kèm gợi ý nới chu kỳ).
> Trần `maxFundingHistoryPages` giới hạn Paradex ở ~83 ngày; nếu GĐ 3 cần sâu
> hơn thì chạy backfill nhiều lượt hoặc nâng trần.

#### Bước 2.7 — Dashboard funding ✅ (2.7a + 2.7b)

> **Tách đôi (2026-09-04, có duyệt).** Bước này gánh hai việc tách bạch được và
> mỗi việc nghiệm thu riêng được: **2.7a** bảng funding + độ tươi + đếm ngược +
> biểu đồ lịch sử; **2.7b** độ sâu sổ lệnh REST 9 sàn + quy đổi contract→coin +
> cột thanh khoản + bảng `depth_snapshots`. Hai commit, không hạ tiêu chí nào.

- Bảng funding hiện tại theo sàn × cặp, **quy về cùng đơn vị 8h** để so sánh công bằng, tô màu theo mức hấp dẫn.
- ⚠️ Đo độ tươi mỗi reading từ `RecvAt` trước khi hiển thị — map funding của scanner giữ bản ghi cuối **mãi mãi** (ghi nhận ở Bước 2.2); subscription chết mà bảng vẫn tô rate cũ như mới là lặp lại lỗi 1.6 ở tầng UI.
- Đếm ngược mốc funding kế tiếp (ẩn với sàn `continuous`).
- Biểu đồ lịch sử funding + basis spot↔perp cùng sàn (thành quả Bước 1.2).
- **Lấy độ sâu sổ lệnh qua REST `depth?limit=100`** cho các cặp ứng viên, mỗi 1–4h — cả spot lẫn perp. Xếp hạng cơ hội **phải** kèm thanh khoản, nếu không screener sẽ đẩy cặp APR cao/sổ mỏng lên đầu ([§7.4](#74-chiến-lược-độ-sâu-sổ-lệnh)).
- ⚠️ Kiểm **phía bid của chân spot** — đó là chỗ kẹt lúc thoát, không phải phía ask lúc vào.
- ⚠️ Nếu mở rộng quá ~20 symbol, tầng broadcast phải sửa trước — xem [§7.3](#73-ngưỡng-mở-rộng-của-tầng-broadcast).
- **Nghiệm thu:** nhìn dashboard biết ngay nên vào cặp nào, sàn nào — và biết con số đang so sánh là cùng đơn vị.

> **⚠️ Đối chiếu P1 (2026-09-04) — sáu điều khác giả định của kế hoạch.**
>
> ① **Không có gì lấy độ sâu sổ lệnh** (`grep depth` = 0). Dò sống cả 9 nguồn,
> tất cả HTTP 200, và ra bốn bẫy: **Kraken** trả `bids` **TĂNG DẦN** — `bids[0]`
> là giá **1**, best bid là phần tử CUỐI — và không có tham số limit (1.825 bid +
> 870 ask, 44 KB); **Hyperliquid** `l2Book` chỉ 20 mức/phía, trải đúng 0,025%
> quanh mid trên BTC, tức **hẹp hơn cả cửa sổ 0,1%** nên số đo được ở đó là cận
> dưới; **Gate/OKX** trả size theo **contract nguyên** (`quanto_multiplier`
> 0,0001 · `ctVal` 0,01) nên không quy đổi thì Gate trông sâu gấp 10.000 lần;
> **Paradex** trả 100 bid nhưng chỉ 43 ask, best ask cách best bid **0,22%**.
> → toàn bộ nằm ở 2.7b.
>
> ② **Ngưỡng độ tươi của giá KHÔNG dùng được cho funding.** Đo 44 phút liên tục
> trên cổng 8085: binance 16s · okx 67s · paradex 71s · gate 4s · kraken 1s ·
> hyperliquid 1s — nhưng **bybit 2.639s và vẫn đang tăng**, vì Bước 2.5 đã sửa nó
> thành *chỉ phát khi trường funding đổi thật*. Tuổi reading của Bybit **không có
> trần theo quan sát**, nên ngưỡng tuổi không thể là thứ phát hiện subscription
> chết ở đó. → thêm phép kiểm thứ hai độc lập với sàn: **mốc settle đã trôi qua**
> thì reading mô tả một kỳ đã kết thúc, dù nó vừa về một giây trước
> ([WS-CONTRACT §9.2](WS-CONTRACT.md)).
>
> ③ **Paradex là lý do phải đo ĐỦ LÂU**: sau 4 phút nó cho 18s, sau 44 phút cho
> 71s. Ngưỡng đặt theo cửa sổ ngắn sẽ báo nhầm một feed khoẻ là chết.
>
> ④ **Ba sàn perp không hedge được** với spot đang cấu hình — Kraken, Hyperliquid,
> Paradex quote USD, spot chỉ có USDT (12/12 từ chối, đúng thiết kế Bước 2.4).
> Bảng funding không nói ra điều này sẽ khoe APR 18,65% của Kraken XRP như một cơ
> hội **không mở được**. → mỗi ô mang `hedge_spot_source` + lý do từ chối.
>
> ⑤ **Lịch sử funding không đi qua WebSocket được**: `internal/scanner` không biết
> `internal/store` và không nên biết. → HTTP `GET /api/funding/history`, ghi thành
> [WS-CONTRACT §10](WS-CONTRACT.md).
>
> ⑥ **Tầng broadcast tệ hơn §7.3 ước tính rất nhiều.** Đo trên bản trước khi sửa,
> một client, 30 giây: **101.627 message `spreads` = 3.387/giây = 11,3 MB/giây**,
> chiếm **99,85%** toàn bộ frame. §7.3 mô tả đúng cơ chế nhưng không có số; con số
> thật khiến việc throttle không còn là "nên làm sớm" mà là điều kiện để hai
> message mới chen được vào.

> **Kết quả 2.7a (2026-09-04).** Message `funding` mới (đẩy sau `meta` lúc kết
> nối rồi mỗi 5s), `meta.funding_basis` + `funding_stale_after_sec` +
> `funding_publish_mode`, REST `/api/funding/history`, và tab Funding trên
> dashboard: ma trận **sàn × cặp quy về bps/8h**, bảng chi tiết theo cặp (chu kỳ,
> đếm ngược, độ tươi, chân spot hedge, hoà phí), biểu đồ lịch sử + độ phủ.
> Hợp đồng mở rộng đúng luật mục 8 — chỉ thêm, `v` vẫn là `1`.
>
> **Nghiệm thu chạy sống, cổng 8085, soak 8082 không bị đụng:**
> - **28 reading = 4 cặp × 7 sàn**, cùng đơn vị bps/8h. Đếm ngược đúng chu kỳ
>   thật: 157 phút cho năm sàn 8h, 37 phút cho Kraken và Hyperliquid (settle theo
>   giờ), Paradex hiện **"liên tục"** và không có đếm ngược — đúng model của nó.
> - **Đường staleness chứng minh đầu-cuối**: chạy một cấu hình cố ý đặt
>   `funding_stale_after_sec: 1` cho Binance → ô đó báo `stale`/`age` ở tuổi 13s
>   trong khi sáu sàn còn lại trên cùng socket vẫn `live`, và **Bybit ở tuổi 19s
>   vẫn `live`** dưới ngưỡng 29.100s của nó — đúng ý đồ thiết kế.
> - **Hoà phí chỉ hiện ở nơi có số thật**: 11,9 ngày cho BTC/binance và 10,0 ngày
>   cho ETH và XRP; `null` ở mọi cặp chạm sàn chưa xác minh biểu phí, và `null`
>   trên SOL vì rate âm — rate âm là chi phí, chi phí thì không hoà vốn.
> - **REST trả 7 series cho BTCUSDT** kèm `coverage[]`: Binance/Bybit 364,7 ngày ·
>   Kraken/Hyperliquid 365,0 · Gate 178,7 · OKX 92,7 · Paradex 83,3 — độ phủ đi
>   kèm biểu đồ chứ không phải ghi chú bên cạnh.
> - **Throttle broadcast**: 3.387 `spreads`/giây (11,3 MB/s) → **20,0/giây**
>   (65 KB/s), tức đúng `số symbol × 5/giây` và có trần. Cảnh báo `arbitrage`
>   **không** bị hoãn.
>
> **Nợ ghi nhận:** 2.7b chưa làm (độ sâu + thanh khoản + `depth_snapshots`), nên
> `best_*_qty_coin` của OKX/Gate/Kraken/Paradex vẫn là `0`. Bảng vẫn xếp theo thứ
> tự cấu hình chứ chưa xếp hạng — cố ý: xếp hạng mà chưa có thanh khoản là đẩy
> cặp APR cao/sổ mỏng lên đầu (§7.4). §7.3 mục 2 (client đăng ký symbol) vẫn còn:
> 20 msg/s hiện tại là 4 symbol, ở 50 symbol sẽ là 250/s.

> **Kết quả 2.7b (2026-09-04).** 9 fetcher độ sâu trong `exchanges/*_depth.go`,
> `internal/depth` (quy đổi contract→coin + đo cửa sổ), bảng `depth_snapshots`
> (schema lên **v2**), message `depth` + `meta.depth`, hai cột thanh khoản trên
> bảng chi tiết, và **khoản nợ 1.2 đã trả**: OKX/Gate/Kraken giờ phát khối lượng
> đỉnh sổ dạng contract và scanner quy đổi qua registry.
>
> **Bốn cái bẫy, đo trực tiếp** (chi tiết ở [DATA-REQUIREMENTS §11](DATA-REQUIREMENTS.md)):
> ① **Kraken trả bid TĂNG DẦN** — `bids[0]` là giá **1**, best bid ở cuối; nên
> `finishDepthBook` sắp xếp vô điều kiện chứ không tin thứ tự tài liệu.
> ② Ba sàn niêm yết theo **contract** (Gate ×0,0001 · OKX ×0,01 · Kraken ×1) —
> không quy đổi thì Gate trông sâu gấp 10.000 lần; không biết hệ số thì **từ
> chối công bố**, không mặc định 1.
> ③ **Vượt trần mức thì MẤT CẢ SỔ**: Gate trả HTTP 400 ở 400 mức, Paradex nói
> thẳng `"Depth: must be no greater than 100."`. Bắt được ngay trong lượt nghiệm
> thu khi nâng 100 → 1000 làm hỏng 4 chuỗi Paradex đang khoẻ.
> ④ **100 mức là KHÔNG ĐỦ** — ở 100 mức thì **7/9 sàn không chạm nổi cửa sổ
> 0,1%**, nên gần như mọi con số là cận dưới và bảng sẽ xếp hạng theo *sàn nào
> trả nhiều mức nhất*. Đã nâng lên trần từng sàn (binance 1000 · bybit 500/200 ·
> okx 400 · gate 300 · paradex 100 · kraken cả sổ · hyperliquid cố định 20):
> 6/9 phủ 0,1%, và **không con số nào làm cả chín phủ 0,5%** → đó là lý do
> `covers_0_1pct`/`covers_0_5pct` có mặt trên wire.
>
> **Nghiệm thu chạy sống, cổng 8085, soak 8082 không bị đụng:**
> - **36/36 phép đo** = 4 cặp × 9 nguồn, không lỗi nào, lưu đủ vào SQLite.
> - **Xếp hạng thanh khoản có ý nghĩa ngay**, BTCUSDT trong ±0,1% phía bid:
>   bybit-F 26,3M · binance-F 18,6M · okx 15,0M · kraken 14,8M · gate 9,7M ·
>   binance-S 8,2M · hyperliquid 2,3M · bybit-S 2,0M — và **paradex 0,016M**.
>   Paradex trả +1,0000 bps/8h funding y hệt các sàn khác trên một sổ mỏng hơn
>   **ba bậc độ lớn**: đúng trường hợp §7.4 nói screener thiếu độ sâu sẽ đẩy lên
>   đầu bảng.
> - **Nợ 1.2 đóng, đo trên wire**: gate 1,3384 · okx 1,8104 · kraken 0,0369 BTC,
>   trước đó cả ba là `0`. 8/9 nguồn có số thật; Paradex vẫn `0` vì sàn không
>   công bố size — `0` là "chưa biết", không phải "không có thanh khoản".
>
> **Nợ ghi nhận:** `fetchInstrumentJSON` giờ phục vụ instrument, funding REST,
> lịch sử funding VÀ độ sâu — tên nói dối lần thứ tư; đổi tên trong một commit
> `refactor(exchanges)` riêng vì nó chạm ~26 file cơ học. Cửa sổ 0,5% là cận dưới
> ở 6/9 sàn và sẽ còn thế: không sàn CEX nào trả đủ mức. Mô hình slippage từ
> những con số này là Bước 3.1, và đó mới là chỗ được dùng chữ "ròng".

---

#### Review độc lập GĐ 2 + đợt vá theo review ✅ (2026-09-04)

> **Phán quyết: ĐẠT — không lỗi chặn.** Review chạy 5 trục song song (chuẩn hoá
> funding · store/history/backfill · registry/mapping/sizing · depth · wire/
> dashboard), mỗi phát hiện phải có bằng chứng file:dòng; kèm kiểm chứng độc
> lập ngoài agent: truy vấn thẳng corpus (90.083 mốc khớp bản ghi nghiệm thu,
> mốc Gate lưu nguyên văn lệch 1–3s, số học per-8h/APR sai số 0 ở 1e-12),
> `go test`/`-race` toàn bộ, luật phụ thuộc, kỷ luật commit. Đường tiền đúng ở
> cả 7 sàn — mọi bẫy trong bảng trap đều có test ghim bằng payload thật.
>
> **Đợt vá theo review — 7 commit code + 1 commit docs, đã xong:**
> ① `fix(exchanges)` sổ WS Kraken sort snapshot vô điều kiện (cùng sàn đã đo
> được đảo thứ tự theo transport ở REST); trần lệnh OKX `maxLmtSz/maxMktSz`
> (350 BTC market) và Binance `MARKET_LOT_SIZE` (120 vs 1000) vào `MaxQtyCoin`
> = min hai trần; lọc `type=flexible_futures` cho Kraken; recognizer 51001
> dùng chung; log khi `fundingIntervalHour` không parse được.
> ② `fix(instruments)` số contract chốt trên đúng lưới sàn (127,999…97 → 128 —
> Gate chỉ nhận contract nguyên); retry refresh lũy tiến 5m→6h khi một nguồn
> hỏng vĩnh viễn; test nhánh MinQty>step chưa từng được chạy.
> ③ `fix(depth)` `is_contract_book` là KHAI BÁO của fetcher, không suy từ hệ
> số ≠1 — Kraken (contract, hệ số 1) hết bị dán nhãn coin, và sổ coin không
> còn bị chặn khi registry thiếu market.
> ④ `fix(store,config,backfill)` schema **v3**: `mark_price`→`mark_price_quote`
> qua migration từng-bước THẬT (có test file v2 thật; file v0-có-bảng bị từ
> chối); comment `interval_sec` phủ trường hợp continuous (Paradex 28800 cạnh
> gap ~3600 là đúng thiết kế); cận 10–60s vào `Validate`; backfill exit ≠ 0
> khi bị huỷ giữa chừng; bỏ `log.Fatalf` nuốt `defer Close`.
> ⑤ `fix(scanner)` 405 cho non-GET trên `/api/funding/history`; test ghim thứ
> tự push meta→funding→depth lúc connect.
> ⑥ `fix(ui)` mất socket thu hồi cả độ tươi funding/depth (bài học 1.6 ở tầng
> client); ô ma trận đánh dấu `∅ hedge` + `APR thô`; đếm ngược có trần ("qua
> mốc Xg" thay vì "đang settle…" vô hạn); cột sổ perp đổi sang phía ASK — cặp
> EXIT đúng như §7.4.
> ⑦ `fix(exchanges)` `is_estimated` theo ĐO chứ không theo comment: Gate
> **true** (probe 2026-09-04: rate trôi 0,000075→0,000074 trong 12s giữa kỳ;
> `funding_rate_indicative` deprecated và bằng hệt `funding_rate` nên phép so
> cũ dán nhãn "đã chốt" cho số đang trôi), Kraken **false** (docs + probe
> §3.3⑥: field WS là số đã settle của giờ vừa xong; bản ước lượng nằm riêng ở
> `relative_funding_rate_prediction`). Golden ghim per-venue.
>
> **Nợ review ghi nhận, CHƯA xử lý (không chặn GĐ 3):**
> - Binance funding phụ thuộc trọn vào `fundingInfo` thành công ≥1 lần — thiết
>   kế cố ý ("không interval thì không reading"), chỉ ghi để nhớ điểm tựa đơn.
> - `store.FundingHistory` với symbol rỗng là full-table scan — GĐ 3 đọc cửa
>   sổ toàn-symbol trên corpus cả năm thì thêm index theo `funding_at_ms`.
> - `mark_price`/`index_price`/`raw_rate` trên WIRE không mang hậu tố đơn vị —
>   hợp đồng §9 đã đóng dấu, KHÔNG đổi (luật 12); khác với cột SQLite đã đổi.
> - Các nợ cũ giữ nguyên hiệu lực: speculative-unmarshal OKX/Gate/Paradex
>   (2.5), `SourceClaim`/`PairAssets` ba-nơi-một-thay-đổi (2.4), gọi thẳng
>   `Refresh()` không dựng lại mapping (chưa có caller), tên
>   ~~`fetchInstrumentJSON`~~ (✅ trả khi tổ chức lại `exchanges/` 2026-09-04:
>   giờ là `exchanges.FetchJSON`), broadcast
>   theo symbol (§7.3 mục 2), trần trang Paradex ~83 ngày.

#### Tổ chức lại cây `exchanges/` ✅ (2026-09-04)

> Package phẳng 66 file / 11.220 dòng tách thành cây: **lõi** (`exchanges/` —
> types, Feeds, vòng đời `RunStream`, `FetchJSON`, các hàm chuẩn hoá chung),
> **8 package sàn** (`exchanges/<venue>/` — trọn bộ connector/funding/depth/
> instruments/history + `testdata/` riêng), **`exchanges/venues/`** (4 bảng
> registry — nơi duy nhất import đủ 8 sàn, tránh vòng import; các capture test
> REST xuyên sàn cũng nằm đây) và **`exchanges/exchangestest/`** (harness:
> recorder, capture, bộ kiểm hợp đồng). Việc tách KHÔNG làm yếu golden test:
> trước kia một vòng lặp replay mọi sàn qua CÙNG một bộ khẳng định — giờ mỗi
> package sàn gọi đúng bộ khẳng định đó trong `exchangestest`
> (`CheckBookContract`/`CheckFundingContract`), nên một sàn vẫn không thể lệch
> hợp đồng dữ liệu một cách im lặng. Trả luôn món nợ tên: `fetchInstrumentJSON`
> → `exchanges.FetchJSON`. Bên ngoài chỉ đổi bốn call site bảng registry
> (`venues.Connectors()` v.v.) và hai chỗ nhận bảng qua tham số
> (`depth.New`/`history.New` — đảo phụ thuộc, entrypoint là nơi biết danh sách
> sàn). Luật phụ thuộc giữ nguyên: không package nào dưới `exchanges/` import
> `internal/`. 382 test, `go test ./...` + `-race` xanh; re-record giờ là
> `CAPTURE_TESTDATA=1 go test -run TestCapture ./exchanges/...`.

### GIAI ĐOẠN 3 — SIGNAL, ALERT & BACKTEST

**Mục tiêu:** Từ dữ liệu thô ra tín hiệu có kiểm chứng, chưa đặt lệnh.
**Thời gian:** 3–4 tuần · **5 bước**

#### Bước 3.1 — Máy tính APR ✅ (2026-09-04)
- `APR = RatePerInterval × (31.536.000 / IntervalSec)` — chuẩn hoá theo chu kỳ thật của từng sàn.
- Tính **APR ròng** = APR thô − phí vào/ra − **slippage ước tính từ độ sâu** (thành quả Bước 2.7), khấu hao theo thời gian giữ dự kiến.
- Đây là chỗ đầu tiên trong lộ trình được phép dùng chữ "ròng" — trước đó chưa có độ sâu nên chưa có slippage ([§7.4](#74-chiến-lược-độ-sâu-sổ-lệnh)).
- Slippage phải tính cho **đúng size dự kiến**, không phải cho size tối thiểu.
- **Nghiệm thu:** unit test đối chiếu với tính tay trên nhiều chu kỳ; APR ròng của một cặp sổ mỏng phải thấp hơn rõ rệt so với khi bỏ qua slippage.

> **Kết quả 3.1 (2026-09-04).** `internal/strategy` có ba tầng:
> `EstimateFill` (slippage một lượt khớp từ sổ đo được), `RoundTripCost` (4
> lượt khớp: MUA spot + BÁN perp lúc vào, BÁN spot + MUA perp lúc thoát) và
> `NetAPR`. Từ đây chữ **"ròng"** có định nghĩa kiểm được: đã trừ hoa hồng
> taker 4 lượt **và** slippage đo cho đúng vốn dự kiến — và `ExcludedVI` đi
> kèm con số liệt kê 5 khoản **chưa** trừ (vay/ký quỹ chân spot, basis giãn
> giữa vào và ra, sổ lệnh lúc thoát, phí chuyển tài sản, rủi ro thanh lý).
>
> **Mô hình slippage — vì sao nó có hình dạng này.** Store lẫn wire chỉ giữ
> độ sâu ở dạng **tổng hợp**, không giữ danh sách mức: hai con số luỹ kế mỗi
> phía (trong 0,1% và trong 0,5%), giá tốt nhất, spread, và sổ trả tới đâu.
> Nên mô hình dựng lại **đường luỹ kế** từ đúng những điểm có thật —
> `(0, nửa spread) → (sâu trong 0,1%, 0,1%) → (sâu trong 0,5%, 0,5%)` — rồi
> lấy tích phân hình thang dọc theo nó. Tuyến tính giữa hai điểm nghĩa là giả
> định thanh khoản rải đều trong cửa sổ; sổ thật dày hơn ở sát giá, nên con số
> **cao hơn** thực tế — hướng an toàn cho một ước lượng lợi nhuận, và được nói
> ra thay vì để người sau tự phát hiện.
>
> Hai điều mô hình **từ chối** làm: (1) ngoại suy quá sổ đo được — lệnh lớn hơn
> độ sâu trong 0,5% **không được định giá**, đó chính là cổng chặn cứng
> [§7.4](#74-chiến-lược-độ-sâu-sổ-lệnh) mục 1; (2) để phản hồi bị cắt cụt đi
> qua như phép đo — khi lượt khớp ăn quá mức xa nhất sàn công bố, kết quả mang
> cờ `DepthIsLowerBound` với nghĩa **độ sâu là cận dưới ⇒ chi phí là cận trên**.
> Lý do hai điều đó khác nhau cũng được ghi thành hai câu từ chối khác nhau:
> "sàn thật sự mỏng" ≠ "sàn chỉ trả tới 0,09%".
>
> **Nghiệm thu chạy trên `depth_snapshots` THẬT (đo 2026-09-04), BTCUSDT, giữ
> 30 ngày, mọi sàn cùng gán rate 1,0 bps/8h — nên khác biệt duy nhất là chi phí
> vào/ra:**
>
> | Vốn | Xếp hạng theo APR ròng | Cùng số đó nếu BỎ slippage | Ghi chú |
> |---|---|---|---|
> | 20.000 | hyperliquid 7,40% · binance 7,29% · kraken 7,28% · **paradex 5,10%** | 7,42 · 7,30 · 7,30 · **7,42** | APR **thô** cả bốn đều 10,95% |
> | 60.000 | hyperliquid 7,37% · binance 7,29% · kraken 7,27% | 7,42 · 7,30 · 7,30 | **paradex TỪ CHỐI** — sổ chỉ có 21.543 trong cửa sổ 0,5% |
> | 2.000.000 | binance 6,87% · kraken 6,78% · **hyperliquid 6,30%** ⚠ | 7,30 · 7,30 · 7,42 | hyperliquid tụt từ hạng 1 xuống hạng 3 và mang cờ cận dưới |
> | 12.000.000 | binance 3,48% ⚠ · kraken 3,06% ⚠ | 7,30 · 7,30 | **hyperliquid TỪ CHỐI**; hai sàn còn lại đều là cận trên chi phí |
>
> Đây đúng là ca [§7.4](#74-chiến-lược-độ-sâu-sổ-lệnh) mô tả: **thứ hạng đổi
> theo size**. Xếp theo funding thô thì bốn sàn bằng nhau ở mọi size; xếp theo
> APR ròng thì hyperliquid thắng ở 20k và **bị loại** ở 12M. Slippage của
> paradex ở 20k là **0,1911%** so với **0,0005%** của binance — chênh 380 lần
> trên cùng một con số funding, và làm mất **2,32 điểm phần trăm** APR: **5,10%
> so với 7,42%** nếu bỏ qua slippage. Cả hai con số đến từ **cùng một lượt
> chạy** và trùng khít với unit test `TestNetAPR_ThinBookCostsRealAPRAgainstIgnoringSlippage`,
> vốn nạp đúng các hàng `depth_snapshots` đó — tiêu chí "sổ mỏng phải thấp hơn
> rõ rệt" đo được bằng số chứ không bằng khẳng định.
>
> Ba sàn `verified: false` (bybit, okx, gate) **từ chối ra số** thay vì tính
> phí bằng 0 — "chưa tra" không phải "miễn phí" (Bước 1.3).
>
> **Review độc lập → REQUEST CHANGES, đã vá hết trước khi commit.** Review chạy
> ngữ cảnh sạch, 9 phép đột biến; **2 phép sống sót** và cả hai đã được ghim lại:
> ① đảo ánh xạ `SideBuy→ask / SideSell→bid` mà **toàn bộ suite vẫn xanh**, vì
> mọi fixture đều đối xứng bid=ask — đúng thứ `cost.go` tồn tại để bảo vệ (chân
> đau là BÁN spot lúc thoát). Nay có sổ lệch (bid mỏng 2.000, ask dày 250.000) và
> phép đảo đó chết ngay; ② ngưỡng `DepthIsLowerBound` không được ghim bằng số.
> Ngoài ra: **NaN/±Inf đi lọt mọi cổng từ chối** (mọi guard là `<= 0`, luôn false
> với NaN) và ra ngoài dưới dạng `OK: true` — nay có `isFinite`/`isPositiveFinite`
> ở cả ba biên, cộng trần `maxHoldingDays` vì phép đổi float64→int64 quá tầm là
> **implementation-dependent** trong Go (bão hoà `MaxInt64` trên arm64, âm trên
> amd64 — hai máy sẽ bất đồng, đúng thứ cổng 3.5 không chịu được); hai câu từ
> chối trước đây **khẳng định điều dữ liệu không chứng minh nổi** ("sàn chỉ trả
> tới đó" — `Summary` không mang số mức đã YÊU CẦU nên không phân biệt được
> "trả thiếu" với "hết sổ"), nay nói đúng phần đo được; `NetAPR` giờ **đối chiếu
> danh tính** (`Cost.PerpSource`/`Symbol` phải khớp rate) sau khi review chỉ ra
> chính các test cũ đang ghép chi phí sàn này với rate sàn khác; `RoundTripInput`
> nhận `At` + `MaxBookAge` (hợp đồng `doc.go`: thời điểm đánh giá **truyền vào**,
> không đọc đồng hồ) vì sổ độ sâu quét tối đa mỗi giờ; và `AppliedVI` nay nói rõ
> **MẪU SỐ** — mọi % tính trên notional MỘT CHÂN, không phải trên vốn triển khai.
>
> Review cũng bắt **số nghiệm thu bản nháp là số ghép**: "2,32 điểm (5,10% so với
> 8,52%)" lấy 2,32 từ unit test và 5,10% từ lượt chạy thật. Gốc rễ: fixture của
> test dùng số phía BID cho cả hai phía, và biểu phí Paradex trong test là 0 thay
> vì **4,5 bps** như `config.yaml` (config cố ý lấy bậc **Pro** để con số sau phí
> là cận DƯỚI của cái giữ lại được; câu "Paradex retail 0%" trong CLAUDE.md là
> lược giản). Đã sửa cả hai; test và lượt chạy nay ra **cùng** 5,10% / 7,42%.
>
> **Nợ ghi nhận.** ① Corpus `depth_snapshots` hiện chỉ có **1 mẫu/nguồn/cặp**
> (di sản lượt nghiệm thu 2.7b), nên Bước 3.3 **không có** độ sâu lịch sử để
> mô hình slippage theo thời gian: backtest phải nêu giả định slippage của nó
> thành tham số, không được vờ như đọc được từ sổ quá khứ. ② Wire vẫn công bố
> `funding_basis.model: "gross"` — cố ý: chưa message nào mang số ròng, và
> nhãn phải nói đúng cái đang gửi. Đổi khi Bước 3.2/3.4 đẩy tín hiệu lên wire.
> ③ `breakevenDaysFeesOnly` trong `internal/scanner` vẫn tự khấu hao phí theo
> cách riêng; nó chỉ tính phí nên không sai, nhưng khi scanner bắt đầu hiển thị
> số ròng thì nên gọi thẳng `internal/strategy` thay vì giữ hai phép tính.
> ④ `SettlementsInHold` là `floor(hold / interval)`, bằng số mốc settle ĐI QUA
> chỉ khi vị thế mở đúng biên; lệch nhiều nhất một mốc, tức **1,1% lợi nhuận
> thô** trên chu kỳ 30 ngày/8h. Đếm chính xác cần thời điểm vào lệnh so với lưới
> settle của sàn — thứ một ước lượng nhìn về phía trước chưa có. Chọn `floor`
> có chủ đích (hướng thận trọng) và **cả hai vế của cổng 3.5 lấy từ cùng hàm
> này**, nên nó không thể thành nguồn bất đồng giữa chúng.

#### Bước 3.2 — Sinh tín hiệu ✅ (2026-09-04)
- Điều kiện vào lệnh: funding rate > ngưỡng **VÀ** duy trì qua N chu kỳ **VÀ** APY ròng > sàn tối thiểu **VÀ** thanh khoản đủ.
- Điều kiện thoát: funding chuyển âm, APY ròng < ngưỡng, hoặc basis giãn bất thường.
- **Nghiệm thu:** tín hiệu ghi log đầy đủ lý do vào/ra.

> **Kết quả 3.2 (2026-09-04).** `internal/strategy/signal.go`: `EvaluateEntry`
> và `EvaluateExit` — hai hàm DUY NHẤT, vì Bước 3.5 chấm điểm bằng cách so
> production với backtest và một cổng giữa hai bản triển khai không phân biệt
> được "chiến lược sai" với "hai bản đã trôi khỏi nhau" (Q8, §7.1).
>
> **Sáu điều kiện vào**, mỗi điều kiện trả về một `Check{Name, Passed, DetailVI}`
> có số kèm theo: `hedge_leg` · `history_depth` · `rate_threshold` ·
> `persistence` · `liquidity` · `net_apr`. **Mọi check đều chạy kể cả khi một
> cái đã hỏng** — dừng sớm thì nêu tên lỗi đầu tiên và giấu phần còn lại, người
> vận hành sửa một thứ rồi chạy lại mới thấy thứ kế, trong khi log đã có sẵn
> câu trả lời. **Bốn điều kiện thoát**, ngược cực (Passed = đã KÍCH HOẠT):
> `hedge_gone` · `funding_negative` · `net_apr_floor` · `basis_widened`.
>
> **Ba quyết định thiết kế, đo trên corpus thật:**
>
> ① **Quyết định trên lịch sử ĐÃ SETTLE, không dùng rate đang hình thành.** Hai
> lý do độc lập cùng chỉ một hướng: backtest đứng ở một mốc quá khứ chỉ có số
> đã settle, nên nếu tín hiệu phụ thuộc rate đang trôi thì cổng 3.5 **không tái
> lập được quyết định kể cả về nguyên tắc**; và `IsEstimated` mang nghĩa KHÁC
> NHAU từng sàn (Gate `true` = đang trôi thật; Kraken `false` = số đã settle
> của giờ vừa xong, **không** dự báo mốc kế) nên mọi luật viết theo cờ đó sẽ có
> bảy nghĩa — đúng cách đọc ngây thơ mà bảng bẫy cảnh báo. Rate live vẫn được
> ghi vào log cho người vận hành, nhưng không vào phép tính.
>
> ② **Check phụ thuộc không được bịa nguyên nhân thứ hai.** Chạy thật phát
> hiện: perp quote USD không có chân hedge thì `liquidity` và `net_apr` báo
> *"biểu phí của (nguồn không tên) chưa xác minh"* — ba dòng đổ lỗi ba thứ
> trong khi chỉ sai một thứ, và dòng to nhất chỉ sai chỗ. Nay chúng trả
> "Chưa đánh giá — không có chân hedge".
>
> ③ **Thoát theo APR ròng phải BỀN qua N mốc, không theo một mốc.** Đo trên
> binance BTCUSDT tháng 8/2026, bps/8h: 0,79 → 0,51 → 0,23 → 0,20 → 0,83 →
> 1,00. Luật một-mốc đóng ở 0,20 rồi lỡ nhịp hồi hai mốc sau, **trả phí vòng cả
> hai chiều** để làm việc đó. Ngưỡng giữ thấp hơn ngưỡng vào (hysteresis) là
> chưa đủ: nếu thoát quyết trên một mốc trong khi vào quyết trên cửa sổ bền thì
> luật thoát nhạy hơn luật vào và khoảng cách hai ngưỡng không mua được gì.
> **Đảo dấu funding thì vẫn thoát ngay** trên mốc mới nhất — tiền đi ra mỗi kỳ
> (rủi ro R1), không có lý do chờ.
>
> **Nghiệm thu — chạy `EvaluateEntry`/`EvaluateExit` trên corpus thật** (lịch sử
> funding đã lưu, sổ lệnh đã lưu, ánh xạ hedge từ instrument snapshot):
>
> - **Ngưỡng chặt** (0,8 bps/8h bền 6 kỳ, APR ròng ≥ 5%, vốn 50k, giữ 30 ngày):
>   **0 vào lệnh / 28 bỏ qua** — trung thực, vì funding BTC hiện chỉ 0,16–0,86
>   bps/8h. Thống kê chặn: liquidity 24 · net_apr 24 · persistence 19 ·
>   hedge_leg 12 · rate_threshold 11.
> - **Ngưỡng lỏng** (0,3 bps/8h bền 3 kỳ, APR ròng ≥ 2%): **4 vào lệnh**, tất cả
>   đều là `binance_futures ← binance_spot` — cặp duy nhất vừa có chân hedge
>   USDT vừa có biểu phí đã xác minh CẢ HAI chân. APR ròng **5,71% (BTC) ·
>   6,74% (ETH) · 6,70% (XRP) · 6,97% (SOL)** — nằm gọn trong dải **5–15%**
>   CLAUDE.md nêu cho chiến lược này, tức số ra không phải là "edge" tưởng tượng.
> - **Thoát, phát lại đúng mốc funding đảo dấu 2026-06-17**: `funding_negative`
>   kích hoạt ở −0,6685 bps/8h ("vị thế đang TRẢ chứ không thu") và
>   `net_apr_floor` ở −10,98%; `hedge_gone` và `basis_widened` **không** kích
>   hoạt — bốn điều kiện độc lập, không cái nào ăn theo cái nào.
> - Mỗi quyết định in đủ 6 (hoặc 4) dòng lý do kèm số, cộng khối "đã trừ" /
>   "CHƯA trừ" lấy thẳng từ `RoundTrip` — tức tiêu chí "ghi log đầy đủ lý do
>   vào/ra" đo được bằng output chứ không bằng khẳng định.
>
> **Một lỗi tìm được khi review, đã sửa trong cùng bước.** `exitNetAPRFloor` đếm
> `under == len(recent)` trong khi `under` chỉ tăng cho mốc định giá được: một
> mốc không định giá được nằm trong cửa sổ khiến **lệnh thoát do suy giảm KHÔNG
> BAO GIỜ kích hoạt** — vị thế cưỡi qua cả một chế độ funding đã chết, trả hoa
> hồng mà không bao giờ thu lại. Cùng chỗ đó, `worstFrac/bestFrac` gieo mầm theo
> `i == 0` nên nếu mốc đầu bị bỏ qua thì câu log báo "thấp nhất 0.00%" — một
> con số không sàn nào công bố. Nay đếm theo `priced`, và có test ghim bằng một
> rate hữu hạn nhưng đủ lớn để tràn khi annualize (ca duy nhất chạm được nhánh
> đó, vì `usableSettled` đã lọc mọi hình dạng còn lại).
>
> **Nợ ghi nhận.** ① Tín hiệu chưa lên wire và chưa có alert — đó là Bước 3.4;
> `funding_basis.model` vẫn là `"gross"` cho tới lúc đó. ② `Params` chưa được
> nạp từ `config.yaml`; hiện là tham số hàm, do caller dựng. Bước 3.3 quét tham
> số nên sẽ chốt hình dạng trước, rồi mới đưa vào YAML.

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
- **Bật WS `depth` incremental trong lúc vào/ra lệnh, tắt khi đang giữ vị thế** — đây là chỗ duy nhất trong lộ trình cần sổ lệnh realtime ([§7.4](#74-chiến-lược-độ-sâu-sổ-lệnh)).
- Kiểm độ sâu ngay trước khi đặt: nếu sổ đã mỏng đi so với lúc sinh tín hiệu, huỷ thay vì vào ở giá xấu.
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

### GIAI ĐOẠN 6 — CROWDING REVERSAL *(thay Basis Trade — quyết định Q11, [§7.1](#71-đã-chốt))*

**Mục tiêu:** Thêm chiến lược thứ hai — **định hướng (long/short), KHÔNG delta-neutral** —
port từ gói nghiên cứu Python `crowding-reversal-python-share-20260904` sang Go,
chứng minh bằng parity với fixture của gói, rồi đi qua đúng các cổng mà track
funding phải đi. Chạy trong cùng tiến trình Go, cùng ingestion / store / config
(Q7, Q8), nhưng **vòng quyết định riêng**: nến 4h UTC, không phải sự kiện settle.
**Thời gian:** 4–6 tuần cho 6.1–6.3 · **5 bước** · **chỉ 6.1 được làm ngay**;
6.2 trở đi chờ track funding đóng GĐ 1 (kết luận soak) và 3.3–3.5, vì đó là ba
cổng đang đứng giữa bot funding và quyết định 3.5, và một nhánh mới không được
đẩy chúng ra sau.

**Luật đóng băng của gói nghiên cứu (không đổi khi port):** BTCUSDT/ETHUSDT
perp USD-M Binance; nến 4h UTC đóng-phải; z-score của `ln(tỉ lệ TÀI KHOẢN
long/short toàn thị trường)` trên 90/180/360 nến × ngưỡng vào |z| 1,0/1,25 ×
ngưỡng ra 0,25/0,50 = **12 thành viên đồng trọng số**; đám đông cực long → short,
cực short → long; giữ đến khi |z| về dưới ngưỡng ra; lọc xu hướng momentum
30/60/120 ngày cần ≥ 2 phiếu; phân bổ nghịch biến động với hiệp phương sai
BTC/ETH, mục tiêu vol 24%/năm (hồ sơ paper: 20%), tổng |notional| ≤ 100% vốn;
trễ 1 nến; thiếu dữ liệu → flat, không fill.

**Con số của gói nghiên cứu, ghi đúng cơ sở để không ai đọc sai:** CAGR lãi kép
dev 26,94% · validation 2024+ 36,84% · toàn mẫu 2021-07→2026-07 31,79%; MaxDD
−9,05% (validation, đóng ngày) · −9,34% (toàn mẫu) · −9,84% (validation, đóng
4h); bootstrap 95% CAGR 13–49%, đuôi MaxDD −13,8%; tuần dương 41,5%, tháng
dương 66,7%, dưới nước dài nhất 105 ngày. **Đó là "sau phí GIẢ ĐỊNH 5 bps/chiều
+ funding lịch sử", không phải "ròng"** theo quy tắc 2: chưa có trượt giá đo từ
sổ, chưa có thanh lý, sự cố sàn. Một sàn, 4,5 năm, đoạn validation đã được xem
nhiều lần. Tác giả chỉ duyệt reference engine + paper trading ≥ 6 tháng luật
đóng băng; **không duyệt vốn thật**. Con số 5–15% ở CLAUDE.md là của track
funding và là APR đơn trên notional một chân; hai track **không cùng đơn vị và
không cộng được**.

| Bước | Nội dung | Phụ thuộc |
|---|---|---|
| 6.1 | Port toán học lõi & parity với fixture | Không — làm ngay được |
| 6.2 | Ingestion: tỉ lệ long/short 5m + close theo quy ước biên đã chốt, schema v4 | GĐ 1 đóng; 3.3–3.5 xong |
| 6.3 | Backtest crowding: vòng lặp nến 4h riêng trong `internal/backtest`, gọi hàm production | 6.1; 3.3 |
| 6.4 | Portfolio manager: phân bổ vốn giữa Funding và Crowding theo lợi suất sau chi phí VÀ tương quan vị thế | 6.3; 3.5 |
| 6.5 | Tái lập chéo sàn (chỉ dữ liệu) + paper trading ≥ 6 tháng luật đóng băng — cổng 🚦 | 6.2; 6.3 |

#### Bước 6.1 — Port toán học lõi & parity (làm ngay được)
- **Vị trí:** `internal/strategy/crowding` chỉ khi `internal/strategy/doc.go`
  được sửa để nói gói này chứa một chiến lược delta-neutral và một chiến lược
  định hướng; nếu không thì gói `internal/crowding` ngang hàng, có `doc.go`
  riêng. Phần lag / P&L / equity của fixture là **backtest** (6.3), không nằm
  trong gói strategy.
- **Phạm vi thật, không phải "4 hàm":** fixture chỉ có đầu ra **trung bình 12
  thành viên** (`expected_score`, `expected_signal` là bội của 1/12,
  `expected_target` là trung bình rồi mới lọc xu hướng), không có cột nào là
  đầu ra của một hàm đơn lẻ. Port cả đường đi 9 định nghĩa (192 dòng Python):
  `rolling_zscore`, `crowding_scores`, `hysteresis_signal`, `causal_weights`,
  `trend_confirmation_votes`, `apply_trend_confirmation`, `ensemble_targets`,
  `StrategyConfig`, hằng số. Kích thước vol của mỗi thành viên = lookback của
  chính nó.
- **Ngữ nghĩa pandas phải tái hiện, ghi vào `doc.go` trước khi code:** `ln(ratio)`
  chỉ khi ratio > 0, NaN không fill; `min_periods = max(20, int(0.8·lookback))`
  đếm giá trị HỮU HẠN; std/var/cov MẪU (ddof = 1); score NaN → state 0 (reset,
  không giữ); `scale = min(target_vol / vol, 1.0)`, `scale[:minimum] = 0`;
  momentum `pct_change(days×6) > 0` với mask "đủ lịch sử" (thiếu ≠ phiếu giảm);
  trung bình 12 rồi mới lọc xu hướng; `BarsPerYear = 6 × 365,25`.
- **Định danh mang đơn vị (quy tắc 4):** `LookbackBars`, `EntryZ` / `ExitZ`
  (không thứ nguyên), `TargetVolAnnualFrac`, `MaxGrossFrac`, `VolWindowBars`,
  `MinPeriodsBars`, `BarsPerYear`, `FeeFracPerSide`. Đầu ra là *phần equity*
  (`TargetFracOfEquity`), không bao giờ mang chữ `Net`.
- **Fixture:** `rust_parity_fixture.csv.gz` (1,2 MB nén; bản thô 3,8 MB = 1,7×
  toàn bộ repo, không commit thô) + `rust_reference_manifest.json` trong
  `testdata/`; test kiểm SHA-256 (`fa9eae27…`) sau khi giải nén rồi mới đọc;
  offline như mọi test khác.
- **Nghiệm thu** (spike 2026-09-11 đã đo là đạt được, nên đây là ngưỡng, không
  phải hy vọng): `expected_target` |err| ≤ 1e-10 (đo 1,6e-14); `expected_score`
  có dung sai ghi rõ (đo 2,5e-13); `expected_signal` **bằng tuyệt đối** — nó là
  bội của 1/12, lệch là máy trạng thái sai, không phải làm tròn; 0 / 10.957 dòng
  vượt ngưỡng. **Không đòi bit-exact:** `math.Log` của Go lệch glibc đúng 1 ulp ở
  8,4% giá trị ratio. Port 161 dòng test Python thành test bảng cho: NaN reset
  state, đếm `min_periods`, mask availability, gross ≤ 1. Ba lỗi ngữ nghĩa đã đo
  làm lệch target 0,18–0,40 equity ở 113–268 dòng — review nhìn vào đó, không
  nhìn vào 1e-10.
- **Giới hạn phải ghi trong doc.go:** parity chứng minh Go = Python trên đường
  4h ĐÃ GỘP; không chứng minh tầng gộp nến (đổi quy ước biên làm CAGR
  26,94/36,84 → 28,84/38,49 — xem 6.2), không chứng minh edge.
- Cổng 1 (P1): commit `docs:` này đi trước; 6.1 là MỘT commit.

#### Bước 6.2 — Ingestion cho crowding
- **Chốt quy ước biên TRƯỚC khi viết bảng:** close của fixture tại T là close của
  nến **1 phút MỞ tại T** (resample đóng-phải), lệch nến 4h của sàn từ −4 đến
  +58 USDT ở các biên đã đo. Hoặc (a) giữ fixture → lưu klines 1m
  (`interval_sec = 60`), quyết định chờ đến T + 60 s; hoặc (b) dùng nến 4h của
  sàn → sinh lại fixture bằng `strict_completed_panel` và lấy số của biến thể
  đó. Không được lẫn hai quy ước.
- **Endpoint tỉ lệ:** `GET /futures/data/globalLongShortAccountRatio`
  (**không phải** `/fapi/v1/…` — đường sai trả 404 và `FetchJSON` đọc thành
  "không niêm yết", bảng lặng lẽ trống), `period=5m`, `limit ≤ 500`, **chỉ 30
  ngày gần nhất**; trích dẫn tài liệu trong comment như `funding_rest.go`.
  Lịch sử 5 năm chỉ có từ kho dump công khai `data.binance.vision`
  (`futures/um/daily/metrics/<SYMBOL>/`, BTC từ 2020-09-01, ETH từ 2021-12-01,
  cột `count_long_short_ratio`) — cần đường ingestion zip/CSV mà repo chưa có.
  Hai nguồn cho một cột: **đo REST vs dump khớp trên 30 ngày chồng lấn** trước
  khi tin.
- **Hình dạng:** fetcher lịch sử (như `FundingHistoryFetchers`), **không có
  `RecvAt`** — quy tắc 13, một mẫu 5 phút là sự kiện đã đóng, không có "độ tươi";
  driver từ `internal/history` theo lịch biên. Không gắn vào poller `premiumIndex`.
- **Schema v4:** `long_short_ratio_samples(source, symbol, period_sec,
  period_end_at_ms, long_short_account_ratio, long_account_frac,
  short_account_frac, raw_ratio_field, recorded_at_ms)`; klines với
  `open_time_ms` / `close_time_ms` / `interval_sec`; mọi cột mang đơn vị; bump
  version + migration.
- **Đầu vào 4h** = mẫu 5m mới nhất trong (T − 4h, T]; thiếu → NULL → score NaN →
  target 0; **không fill** (gói nghiên cứu có 119 nến trống liên tiếp
  2021-12-30 → 2022-01-19 và ngồi ngoài).
- **Nghiệm thu:** (1) REST vs dump: max |Δ| trên 30 ngày chồng lấn; (2) đầu vào
  4h do Go dựng từ dump = `input_*` của fixture trên 2021-07 → 2026-07, 0 lệch;
  (3) `go test ./...` vẫn offline.

#### Bước 6.3 — Backtest crowding
- Vòng lặp **riêng** trong `internal/backtest` theo nến 4h, gọi đúng hàm
  production của 6.1 (Q8). Không dùng chung vòng sự kiện settle của funding;
  dùng chung phần báo cáo / ghi store.
- Chạy từ fixture trước (đầu vào 5 năm đã có sẵn ở đó), từ store sau; hai kết
  quả phải bằng nhau.
- `position[t] = target[t−1]`; `funding_pnl = −position × rate` **chỉ ở nến
  settle** (bucket theo `round_to_hour(stamp)`, stamp trong store giữ nguyên);
  không pro-rate; không hằng số 28800 — đọc `interval_sec` / `gap_prev_sec`
  (quy tắc 3, 6).
- **Nghiệm thu:** tái tạo `core_statistics` của manifest (toàn mẫu 3,978×,
  CAGR 31,79%, MaxDD −9,34% đóng ngày; net ≤ 1e-12 / nến, equity ≤ 1e-9 tương
  đối); báo cáo ghi rõ cơ sở (ngày / 4h), nhãn **"sau phí giả định 5 bps/chiều"**,
  MaxDD kèm đuôi bootstrap. Sau đó **Go là reference đóng băng** (sinh fixture
  từ Go), Python thôi là nguồn sự thật — nếu không, Go mãi là bản thứ hai mà
  `internal/backtest/doc.go` cấm.

#### Bước 6.4 — Portfolio manager (giữ từ kế hoạch cũ, đổi đối tượng)
- Phân bổ vốn giữa Funding và Crowding theo lợi suất sau chi phí **và tương
  quan vị thế đo từ 6.3** — hai track **không độc lập**: funding arb short perp
  khi đám đông long và funding dương; crowding cũng short perp ở chế độ đó, và
  khi crowding LONG nó **trả** đúng khoản funding track kia thu.
- **Mỗi chiến lược một sub-account.** Cùng BTCUSDT/ETHUSDT perp trên một tài
  khoản thì sàn báo một vị thế ròng mỗi symbol: quy tắc 7 không quy được vị
  thế cho chiến lược nào, chân spot của funding thành trần, một khoản funding
  được ghi hai lần. Chi tiết chế độ vị thế / ký quỹ kiểm tài liệu Binance ở
  GĐ 4 (quy tắc 5).
- **Nghiệm thu:** báo cáo tương quan vị thế và tỉ trọng funding trong lợi nhuận
  crowding từ 6.3; quy tắc phân bổ có test; lợi suất tổng KHÔNG phải tổng hai
  con số.

#### Bước 6.5 — Tái lập chéo sàn và cổng paper 🚦
- Chỉ dữ liệu, không giao dịch ở sàn khác: Bybit `GET /v5/market/account-ratio`
  (kiểm từ repo tài liệu chính thức của Bybit; `buyRatio` = tỉ lệ người dùng net
  long), OKX rubik long-short-account-ratio (chưa kiểm). Connector hai sàn đã có.
- Paper trading **≥ 6 tháng, luật đóng băng**, hồ sơ 20% target-vol (dev 22,99% /
  validation 29,99% CAGR; MaxDD −7,75% ngày / −8,41% 4h); không sửa tham số vì
  chuỗi thua. Ghi mỗi quyết định: stamp dữ liệu gốc, target, giá lý thuyết, giá
  khớp được, chi phí thật.
- **Nghiệm thu:** paper khớp backtest 6.3 trong sai số chấp nhận được (gương của
  3.5). **Không khớp → quay lại 6.1 / 6.2, không sang execution.**

**Ràng buộc kéo sang GĐ 4–5 cho track này** (ghi ở đây để 4.x / 5.x không quên):
fail-closed khi tỉ lệ thiếu hoặc cũ (ngưỡng = khoảng công bố ĐO ĐƯỢC × ~3, cách
`config.yaml` đang làm); cổng "nến đã đóng" ở biên 4h UTC; phụ thuộc một sàn
không có giảm nhẹ cho tới khi 6.5 xong; và mọi ngưỡng cắt theo drawdown phải
nêu **cơ sở** (đóng ngày / 4h / mark liên tục; đỉnh từ đầu hay trượt; equity
sub-account hay cả tài khoản; luật vào lại) TRƯỚC khi nêu số — 10% nằm trong
vùng vận hành bình thường của chính backtest (0,16 điểm trên MaxDD 4h; 37,46%
đường bootstrap vượt −10%), không phải đuôi. Một stop-loss là luật mới mà backtest
chưa chạy: thêm vào thì chạy lại 6.3 và sinh lại fixture, không thêm âm thầm.

**Kỳ vọng sau GĐ 6:** không cộng hai con số. Track funding: 5–15% APR đơn (kỳ
vọng thiết kế; đo được 5,71–6,97% ròng ở 4 cặp Binance). Track crowding: số ở
trên, sau phí giả định, chờ 6.5. Lợi suất tổng đo ở 6.4 sau khi có tương quan.

---

### GIAI ĐOẠN 7 — CEX-DEX ARBITRAGE 🔒 *(khoá — chỉ mở sau GĐ 5 và sau cổng 6.5)*

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
├── cmd/
│   ├── scanner/               # entrypoint
│   ├── fundingcheck/          # [GĐ 2.1] kiểm chéo field funding 7 sàn
│   └── backfill/              # [GĐ 2.6] nạp lịch sử funding vào SQLite
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
│   ├── <venue>_funding.go     # [GĐ 2.2] chuẩn hoá đơn vị funding từng sàn
│   ├── <venue>_funding_ws.go  # [GĐ 2.5] thu funding realtime
│   └── <venue>_funding_history.go  # [GĐ 2.6] rate ĐÃ settle, cho backtest
├── internal/
│   ├── scanner/               # engine + hợp đồng wire (đã tách khỏi gốc 2026-09-03)
│   ├── fees/                  # [GĐ 1.3] bảng phí theo sàn
│   ├── instruments/           # [GĐ 2.3] registry + ánh xạ spot↔perp
│   ├── history/               # [GĐ 2.6] REST sàn → store (scanner + backfill)
│   ├── store/                 # [GĐ 2.6] SQLite: funding_history,
│   │                          #          price_snapshots, instrument_snapshots
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
| R11 | Ghép sai cặp spot↔perp → vị thế lệch coin | **Rất cao** | GĐ 2.4 ✅ | Bảng ánh xạ tự dựng từ base/quote SÀN TỰ KHAI + xác thực hai chiều, từ chối nêu tên khi không ghép được — `BuildHedgeMapping`, Bước 2.4 |
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
| **Q11** | **Bỏ Basis Trade khỏi lộ trình; GĐ 6 trở thành Crowding Reversal** — chiến lược định hướng port Go từ gói nghiên cứu Python, parity với fixture, qua cổng riêng 6.5. Giữ 6.4 portfolio manager. **Quyết định LỘ TRÌNH, có thể đảo ngược** — không phải kết luận "basis trade không hoạt động" | 2026-09-11 |

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

#### Q11 — Vì sao bỏ Basis Trade, và vì sao KHÔNG kết luận gì về nó

**Bằng chứng đã đo** — gói nghiên cứu `crowding-reversal-python-share-20260904`
(2026-09-04), file `STRATEGY_RESEARCH_CONCLUSION.md`, mục "永续—季度交割 日历价差"
(chênh lệch lịch perp ↔ futures quý, thuần hợp đồng, Binance): cấu hình tốt nhất
chọn TRƯỚC trên đoạn phát triển chỉ **0,25%/năm ở 5 bp/chiều**, **−0,11% / −0,83%
ở 10 / 20 bp**; funding thu được bị chi phí roll hai chân và biến động chênh lệch
hợp đồng ăn hết; tác giả không mở đoạn validation vì không có cấu hình đạt.
Con số "8–20%/năm sau GĐ 6" của kế hoạch cũ là ước đoán thiết kế ngày
2026-08-28, chưa từng được đo — cùng loại với "5–15%" của track funding.

**Giới hạn của bằng chứng, và vì thế đây là quyết định lộ trình chứ không phải
phán quyết:** chỉ MỘT biến thể (perp ↔ quý, không phải cash-and-carry spot ↔ quý
như GĐ 6 cũ mô tả), MỘT sàn, trong cửa sổ dữ liệu của gói. Ai muốn đưa Basis
Trade trở lại phải **đo cash-and-carry spot ↔ quý trên corpus thật, có chi phí
roll**, không được dùng lại "8–20%" làm lý do. Slot GĐ 6 dùng cho Crowding
Reversal vì gói nghiên cứu đã có sẵn fixture parity 10.957 dòng và spike
2026-09-11 đo được port Go đạt 1e-10 trong khoảng một ngày công — xem GĐ 6.

**Không xoá theo:** trường `basis[]` trên wire (1.2), `MaxBasisPct` /
`MaxBasisWidenPct` và luật thoát `basis_widened` (3.2), chú thích trong
`internal/scanner/grouping.go` — đó là basis spot ↔ perp đo được, không phải
Basis Trade; contract WS đang đóng băng (quy tắc 12). Cũng bỏ nhu cầu
`MarketType: future` + `ExpiryDate` trong registry — một đơn giản hoá thật.

**Hệ quả phải nói ra:** kế hoạch gốc là hai chiến lược cùng delta-neutral; sau
Q11 dự án là **một chiến lược delta-neutral cộng một chiến lược định hướng**.
Câu "delta-neutral xuyên suốt" chỉ còn đúng cho track funding, không cho cả hệ
thống — CLAUDE.md và README đã sửa cùng commit này.

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

0. **Đo được ở Bước 1.0:** hợp đồng mới làm payload `spreads` to gấp **2,8 lần** (6.861 B so với 2.475 B, 10 nguồn) vì mỗi tick gửi lại metadata tĩnh của nhóm (`note_vi`, `market_type`, `quote_asset`) và một `spread_after_fees_pct: null` cho mỗi ô. Đây là cái giá đã biết của việc chốt hợp đồng một lần, và nó làm ba việc dưới đây đáng làm sớm hơn.
1. Throttle `broadcastSpreads` bằng ticker giống `broadcastPrices` đã làm — ~10 dòng, ăn phần lớn lợi ích
2. Client gửi symbol đang xem, server chỉ đẩy symbol đó — bỏ lãng phí N×
3. FE bỏ `innerHTML` dựng lại toàn bộ, chuyển sang cập nhật `textContent` từng ô — chỉ cần khi vượt ~20 symbol
4. **Hàng đợi gửi riêng cho từng client.** `broadcast` hiện chạy đồng bộ trên luồng ingest dưới `wsWriteMutex`; deadline 2s thêm ở Bước 1.0 biến "treo vô hạn" thành "treo có chặn", nhưng N client kẹt vẫn tốn N×2s mỗi tick, đủ để `priceChan` đầy và chặn ngược các connector

### 7.4. Chiến lược độ sâu sổ lệnh

Phân tích 2026-08-28. Câu hỏi: dự án có cần thu thập độ sâu sổ lệnh không?

**Có — nhưng không phải dạng stream realtime, và không phải ở GĐ 1.**

#### Vì sao funding arb ít nhạy với slippage hơn arbitrage giá

Lợi nhuận đến từ **phí funding**, không phải chênh lệch giá. Chi phí vào/ra là chi phí **một lần**, khấu hao theo thời gian giữ — khác hẳn arbitrage chéo sàn nơi slippage ăn thẳng vào biên vốn đã mỏng.

```
Binance, taker cả hai chân:
  Vào:  spot 0,10% + perp 0,05% = 0,15%
  Ra:   spot 0,10% + perp 0,05% = 0,15%
  Riêng phí:                      0,30%

  Funding 0,01%/8h = 0,03%/ngày → hoà vốn sau ~10 ngày
  Thêm slippage 0,20%  → tổng 0,50% → hoà vốn sau ~17 ngày
```

Slippage **không giết giao dịch, nhưng kéo thời gian hoà vốn tăng ~70%**. Nó quyết định *cơ hội nào đủ tiêu chuẩn*, không quyết định *giao dịch có khả thi không*.

#### Ba chỗ độ sâu là bắt buộc

1. **Xác định size — cổng chặn cứng.** Muốn vào $60.000 mà sổ chỉ có $10.000 trong phạm vi 0,1% thì không vào được ở mức giá đã mô hình hoá.
2. **Bất đối xứng lúc thoát.** Vào lệnh khi funding hấp dẫn (thị trường bình thường); thoát khi funding đảo chiều — mà điều đó **tương quan với căng thẳng thị trường**, đúng lúc sổ mỏng đi. **Độ sâu lúc vào không dự báo được độ sâu lúc ra.** Chân đau nhất là phía bid của spot lúc thoát.
3. **Xếp hạng cơ hội.** Funding cao thường nằm ở alt thanh khoản mỏng. Thiếu độ sâu, screener sẽ xếp *200% APR với sổ $2.000* trên *15% APR với sổ $500.000* — không phải thiếu thông tin mà là **thông tin sai hướng**.

#### Nhưng không cần stream realtime

| Cách | Chi phí | Cần không |
|---|---|---|
| WS `depth` incremental | Dựng lại sổ lệnh, quản sequence number, phát hiện gap, resync | ❌ Chỉ khi **đang đặt lệnh** |
| REST `depth?limit=100` định kỳ | Một request mỗi vài phút cho cặp ứng viên | ✅ Đủ cho screening |

Funding arb giữ vị thế **hàng ngày đến hàng tuần**. Không có lý do biết sổ lệnh đổi từng mili-giây khi mỗi tuần chỉ giao dịch một lần.

#### Thứ đang miễn phí mà bị vứt đi

`OrderbookData` chỉ có `BestBid`/`BestAsk`, **không có khối lượng** — trong khi connector đã parse sẵn rồi vứt:

```
binance.go:30,32   BestBidQty `json:"B"` / BestAskQty `json:"A"`
bybit.go:19,145    Size `json:"v"`
okx.go:23          Size `json:"sz"`
```

Khối lượng đỉnh sổ không thay được độ sâu đầy đủ, nhưng cho **bộ lọc thanh khoản bậc một miễn phí** — không subscribe thêm, không tốn băng thông. Đưa vào GĐ 1 vì hợp đồng JSON ở Bước 1.0 phải có sẵn chỗ, nếu không lại sửa `app.js` thêm lần nữa ở GĐ 2.

#### Lịch trình

| Cần gì | Bước |
|---|---|
| Khối lượng đỉnh sổ (miễn phí) | **1.0** (chỗ trong hợp đồng) + **1.2** (điền 5/9 nguồn báo bằng coin; 4 sàn báo bằng contract chờ instrument registry ở GĐ 2) |
| Độ sâu REST định kỳ để xếp hạng | **2.7** |
| Mô hình slippage từ độ sâu → APR ròng thật | **3.1** |
| WS depth khi đang đặt lệnh | **4.4** |

Đây cũng là lý do Bước 1.3 phải gọi là **"đã trừ phí giao dịch"** chứ không phải "ròng": chưa có độ sâu thì chưa có slippage, chưa có slippage thì chưa phải ròng.

---

## 8. ĐỐI CHIẾU VỚI TÀI LIỆU 5 CHIẾN LƯỢC

Kế hoạch này chia nhỏ hơn tài liệu gốc, vì tài liệu gốc gộp quá nhiều việc vào "Giai đoạn 1".

| Tài liệu gốc | Kế hoạch này |
|---|---|
| GĐ 0 — Chuẩn bị nền tảng | **GĐ 0** (đã xong) |
| GĐ 1 — Funding Rate Arbitrage | **GĐ 1 + 2 + 3 + 4 + 5** (tách làm 5 vì phần execution & risk bị đánh giá thấp trong tài liệu gốc) |
| GĐ 2 — Basis Trade | **Bỏ** (Q11, 2026-09-11) — lý do: biến thể perp ↔ quý đo được chỉ 0,25%/năm ở 5 bp/chiều, âm ở 10/20 bp; slot GĐ 6 dùng cho Crowding Reversal. Có thể đảo ngược nếu đo được cash-and-carry spot ↔ quý sinh lời sau chi phí roll |
| GĐ 3 — CEX-DEX | **GĐ 7** |
| GĐ 4 — Cross-Chain | **GĐ 8** |
| GĐ 5 — Statistical | **GĐ 8** |

**Khác biệt chính so với tài liệu gốc:** thêm hẳn một giai đoạn "Củng cố lõi" (GĐ 1) ở đầu, tách "Execution" khỏi "Risk Management" thành hai giai đoạn độc lập, và kéo instrument registry + ánh xạ spot↔perp từ phần execution lên GĐ 2. Lý do: codebase hiện tại có 4 vấn đề (staleness, trộn spot/perp, không có phí, không có test) sẽ nhân bản thành lỗi tốn tiền nếu xây tiếp lên trên mà không xử lý trước.

---

## PHỤ LỤC — CHECKLIST NHANH

```
[✅] GĐ 0  Nền tảng scanner              5/5 bước
[  ] GĐ 1  Củng cố lõi                   7/7 bước · soak 72h chạy từ 2026-09-03 14:03, hạn 2026-09-06   ← ĐANG LÀM
[✅] GĐ 2  Funding Rate Monitor          7/7 bước
[  ] GĐ 3  Signal, Alert & Backtest      2/5 bước   ← ĐANG LÀM
[  ] GĐ 4  Execution Engine              0/6 bước
[  ] GĐ 5  Risk & Vận hành               0/5 bước
[  ] GĐ 6  Crowding Reversal (thay Basis Trade — Q11)  0/5 bước · 6.1 làm ngay được
[🔒] GĐ 7  CEX-DEX                       khoá
[🔒] GĐ 8  Cross-Chain / Statistical     khoá
```

**Việc tiếp theo cụ thể:** cả 7 bước của GĐ 1 đã xong, **còn đúng một việc để
đóng giai đoạn: phiên chạy 72h không can thiệp** (vế còn lại của tiêu chí Bước
1.5 — xem ghi chú ở đó). Trước khi chạy nên xử lý khoản nợ 🔴 ghi ở Bước 1.6:
subscription bị sàn âm thầm huỷ hiện **không có gì buộc nối lại**, và đó đúng là
dạng hỏng mà 72 giờ sinh ra để phát hiện — chạy mà không sửa thì nhiều khả năng
chỉ chứng minh lại rằng nó tồn tại.

**GĐ 2 đã xong cả 7 bước** (2026-09-04), chạy song song và không đụng tiến trình
soak: funding realtime 7 sàn, instrument registry, ánh xạ spot↔perp, persistence
SQLite với corpus 90.077 mốc settle, và dashboard funding kèm độ sâu sổ lệnh.

**Bước 3.1 xong (2026-09-04)** — `internal/strategy` đã có máy tính APR ròng:
slippage dựng từ đường luỹ kế của sổ ĐO ĐƯỢC, hoa hồng taker 4 lượt khớp, và
cổng chặn cứng khi lệnh vượt độ sâu trong cửa sổ 0,5%. Đo trên corpus thật:
cùng một funding rate, thứ hạng bốn sàn **đổi theo size** và paradex bị loại từ
mốc 60k.

**Bước 3.2 xong (2026-09-04)** — `EvaluateEntry`/`EvaluateExit`, sáu điều kiện
vào và bốn điều kiện thoát, mỗi cái ghi lý do kèm số. Quyết định chạy trên lịch
sử **đã settle** chứ không trên rate đang hình thành, vì chỉ số đã settle mới
tồn tại ở cả hai phía cổng 3.5 và vì `IsEstimated` mang bảy nghĩa khác nhau. Đo
trên corpus thật: ngưỡng chặt cho **0 lệnh vào / 28 bỏ qua**; ngưỡng lỏng cho
**4 lệnh**, APR ròng 5,71–6,97% — trong dải 5–15%. Việc tiếp theo là
**Bước 3.3 — backtest engine**, và nó phải `import internal/strategy` gọi thẳng
hai hàm trên (Q8), cộng khoản nợ index `funding_at_ms` khi đọc cửa sổ
toàn-symbol.

⚠️ GĐ 1 vẫn **chưa đóng**: còn phiên chạy 72h (hạn 2026-09-06) và một phiên khác
ra phán quyết.
