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

**Tổng: 9 giai đoạn (GĐ 0 → GĐ 8), 41 bước.**

| GĐ | Tên | Số bước | Thời gian | Trạng thái | Kết quả bàn giao |
|---|---|---|---|---|---|
| **0** | Nền tảng scanner | 5 | — | ✅ **90% xong** | Scanner real-time 10 nguồn |
| **1** | Củng cố lõi (Hardening) | 7 | 3–4 tuần | 🔄 **7/7 bước, còn phiên 72h** | Scanner đáng tin, có test, có phí |
| **2** | Funding Rate Monitor | 7 | 4–5 tuần | 🔄 **3/7 bước** | Thu thập + lưu funding rate 24/7 |
| **3** | Signal, Alert & Backtest | 5 | 3–4 tuần | ⬜ Chưa bắt đầu | Tín hiệu có kiểm chứng lịch sử |
| **4** | Execution Engine | 6 | 6–8 tuần | ⬜ Chưa bắt đầu | Bot đặt lệnh được (vốn nhỏ) |
| **5** | Risk & Vận hành | 5 | 4–6 tuần | ⬜ Chưa bắt đầu | Bot chạy production 24/7 |
| **6** | Basis Trade | 4 | 4–6 tuần | ⬜ Chưa bắt đầu | Bot hỗ trợ 2 chiến lược |
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
> - Chu kỳ 1h của Kraken ghim trong `kraken_funding.go` (sàn không công bố
>   interval ở bất kỳ message funding nào — không có gì để đọc). Rủi ro còn
>   lại: sàn đổi cadence giữa hai lần backfill thì lệch 2× không bị kiểm 5×
>   của fundingcheck bắt; Bước 2.6 phải đo lại spacing của
>   `historicalfundingrates` mỗi lần backfill.
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
> - Cadence 1h Kraken: ghi thêm bản ghi đuôi `historicalfundingrates` vào
>   testdata + golden khoảng cách == 3600 để việc re-record tự kiểm hằng số
>   (làm cùng backfill 2.6).
> - `cmd/fundingcheck`: verdict OKX dùng `<` chặt — chạy đúng mốc settle có
>   thể false-FAIL; verdict Gate tin đồng hồ máy (lệch >60s false-FAIL). Sửa
>   khi có dịp đụng công cụ.
> - Fetcher theo-symbol (Bybit/OKX/Gate/Paradex) gọi tuần tự trong nguồn —
>   đủ cho 4 cặp; vượt ~10 cặp thì chuyển sang endpoint danh sách của sàn.
> - `TickSizeQuote = 0` (Hyperliquid, quy tắc 5 chữ số có nghĩa) là sentinel;
>   GĐ 4 đặt lệnh cần mã hoá quy tắc thành dữ liệu (`PxDecimals`/`SigFigs`).

#### Bước 2.4 — Bảng ánh xạ spot ↔ perp
- Dựng **tự động** từ `exchangeInfo`, thay `switch` hardcode hiện tại trong từng connector.
- Xác thực hai chiều; cặp không ghép được → **từ chối, không đoán**.
- Chú ý Kraken `PF_XBTUSD` quote là **USD** không phải USDT → không hedge thẳng bằng spot USDT.
- ~~🐛 Sửa bug `coin := symbol[:3]` ở Hyperliquid~~ → ✅ **đã xong ở Bước 1.4** như hệ quả của việc đưa ánh xạ ký hiệu vào `config.yaml`: Hyperliquid dùng `symbol_format: "{base}"`, và nghiệm thu đã chạy thật với `DOGEUSDT` (base 4 ký tự).
- **Nghiệm thu:** thêm 1 cặp mới → bảng tự dựng đúng trên mọi sàn hỗ trợ, tự loại sàn không hỗ trợ.

#### Bước 2.5 — Thu thập funding rate
- **WebSocket** (ưu tiên): Binance `@markPrice@1s`, Bybit `tickers`, OKX `funding-rate`, Gate `futures.tickers`, Kraken `ticker`, Hyperliquid `activeAssetCtx`.
- ⚠️ Bybit ticker là **snapshot + delta** — field vắng mặt nghĩa là *chưa đổi*, phải merge vào cache, không ghi đè.
- ⚠️ Binance `fundingInfo` **theo docs chỉ trả symbol lệch mặc định** (thực tế 2026-09-03 phủ 100% — vẫn phải mặc định 8h rồi ghi đè, không đọc ngược lại; xem [DATA-REQUIREMENTS §3.3](DATA-REQUIREMENTS.md)).
- **Nghiệm thu:** funding rate 4 cặp × 7 sàn realtime, khớp số `cmd/fundingcheck` đọc qua REST tại cùng thời điểm (và web sàn khi đối chiếu được bằng mắt), đã chuẩn hoá về `RatePer8hFrac` so sánh được chéo sàn.

#### Bước 2.6 — Persistence (SQLite)
- `funding_history(exchange, symbol, raw_rate, rate_per_8h, interval_sec, funding_time, rate_type, recorded_at)`.
- `instruments` (snapshot hằng ngày — để biết `stepSize` đã đổi lúc nào), `price_snapshots` (lấy mẫu 1–5s).
- Backfill lịch sử 6–12 tháng qua REST; lọc `rateType = Special` khi backtest.
- Lưu trữ: 12 tháng funding, 3 tháng price snapshot.
- **Nghiệm thu:** restart không mất dữ liệu; truy vấn được funding 30 ngày; có đủ corpus cho backtest GĐ 3.

#### Bước 2.7 — Dashboard funding
- Bảng funding hiện tại theo sàn × cặp, **quy về cùng đơn vị 8h** để so sánh công bằng, tô màu theo mức hấp dẫn.
- ⚠️ Đo độ tươi mỗi reading từ `RecvAt` trước khi hiển thị — map funding của scanner giữ bản ghi cuối **mãi mãi** (ghi nhận ở Bước 2.2); subscription chết mà bảng vẫn tô rate cũ như mới là lặp lại lỗi 1.6 ở tầng UI.
- Đếm ngược mốc funding kế tiếp (ẩn với sàn `continuous`).
- Biểu đồ lịch sử funding + basis spot↔perp cùng sàn (thành quả Bước 1.2).
- **Lấy độ sâu sổ lệnh qua REST `depth?limit=100`** cho các cặp ứng viên, mỗi 1–4h — cả spot lẫn perp. Xếp hạng cơ hội **phải** kèm thanh khoản, nếu không screener sẽ đẩy cặp APR cao/sổ mỏng lên đầu ([§7.4](#74-chiến-lược-độ-sâu-sổ-lệnh)).
- ⚠️ Kiểm **phía bid của chân spot** — đó là chỗ kẹt lúc thoát, không phải phía ask lúc vào.
- ⚠️ Nếu mở rộng quá ~20 symbol, tầng broadcast phải sửa trước — xem [§7.3](#73-ngưỡng-mở-rộng-của-tầng-broadcast).
- **Nghiệm thu:** nhìn dashboard biết ngay nên vào cặp nào, sàn nào — và biết con số đang so sánh là cùng đơn vị.

---

### GIAI ĐOẠN 3 — SIGNAL, ALERT & BACKTEST

**Mục tiêu:** Từ dữ liệu thô ra tín hiệu có kiểm chứng, chưa đặt lệnh.
**Thời gian:** 3–4 tuần · **5 bước**

#### Bước 3.1 — Máy tính APR
- `APR = RatePerInterval × (31.536.000 / IntervalSec)` — chuẩn hoá theo chu kỳ thật của từng sàn.
- Tính **APR ròng** = APR thô − phí vào/ra − **slippage ước tính từ độ sâu** (thành quả Bước 2.7), khấu hao theo thời gian giữ dự kiến.
- Đây là chỗ đầu tiên trong lộ trình được phép dùng chữ "ròng" — trước đó chưa có độ sâu nên chưa có slippage ([§7.4](#74-chiến-lược-độ-sâu-sổ-lệnh)).
- Slippage phải tính cho **đúng size dự kiến**, không phải cho size tối thiểu.
- **Nghiệm thu:** unit test đối chiếu với tính tay trên nhiều chu kỳ; APR ròng của một cặp sổ mỏng phải thấp hơn rõ rệt so với khi bỏ qua slippage.

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
├── cmd/
│   └── scanner/               # entrypoint (Bước 2.1 thêm cmd/fundingcheck)
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
│   ├── scanner/               # engine + hợp đồng wire (đã tách khỏi gốc 2026-09-03)
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
| GĐ 2 — Basis Trade | **GĐ 6** |
| GĐ 3 — CEX-DEX | **GĐ 7** |
| GĐ 4 — Cross-Chain | **GĐ 8** |
| GĐ 5 — Statistical | **GĐ 8** |

**Khác biệt chính so với tài liệu gốc:** thêm hẳn một giai đoạn "Củng cố lõi" (GĐ 1) ở đầu, tách "Execution" khỏi "Risk Management" thành hai giai đoạn độc lập, và kéo instrument registry + ánh xạ spot↔perp từ phần execution lên GĐ 2. Lý do: codebase hiện tại có 4 vấn đề (staleness, trộn spot/perp, không có phí, không có test) sẽ nhân bản thành lỗi tốn tiền nếu xây tiếp lên trên mà không xử lý trước.

---

## PHỤ LỤC — CHECKLIST NHANH

```
[✅] GĐ 0  Nền tảng scanner              5/5 bước
[  ] GĐ 1  Củng cố lõi                   7/7 bước · soak 72h chạy từ 2026-09-03 14:03, hạn 2026-09-06   ← ĐANG LÀM
[  ] GĐ 2  Funding Rate Monitor          3/7 bước   ← ĐANG LÀM song song với soak
[  ] GĐ 3  Signal, Alert & Backtest      0/5 bước
[  ] GĐ 4  Execution Engine              0/6 bước
[  ] GĐ 5  Risk & Vận hành               0/5 bước
[  ] GĐ 6  Basis Trade                   0/4 bước
[🔒] GĐ 7  CEX-DEX                       khoá
[🔒] GĐ 8  Cross-Chain / Statistical     khoá
```

**Việc tiếp theo cụ thể:** cả 7 bước của GĐ 1 đã xong, **còn đúng một việc để
đóng giai đoạn: phiên chạy 72h không can thiệp** (vế còn lại của tiêu chí Bước
1.5 — xem ghi chú ở đó). Trước khi chạy nên xử lý khoản nợ 🔴 ghi ở Bước 1.6:
subscription bị sàn âm thầm huỷ hiện **không có gì buộc nối lại**, và đó đúng là
dạng hỏng mà 72 giờ sinh ra để phát hiện — chạy mà không sửa thì nhiều khả năng
chỉ chứng minh lại rằng nó tồn tại.

GĐ 2 đã bắt đầu song song (không đụng tiến trình soak): Bước 2.1 và 2.2 xong
2026-09-03. Tiếp theo là **Bước 2.3** — instrument registry
(`internal/instruments`): `exchangeInfo` cache 1 lần/ngày, contract size 4 sàn,
sizing delta-neutral.
