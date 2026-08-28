# YÊU CẦU DỮ LIỆU — Funding Rate Arbitrage Bot

> **Cập nhật:** 2026-08-28
> **Phạm vi:** 7 sàn futures + 2 sàn spot hiện có trong repo
> **Liên quan:** [PLAN.md](PLAN.md) — tài liệu này chi tiết hoá GĐ 2

---

## MỤC LỤC

1. [Nguyên tắc: chia theo tần suất](#1-nguyên-tắc-chia-theo-tần-suất)
2. [Danh mục dữ liệu A–E](#2-danh-mục-dữ-liệu-a--e)
3. [Khảo sát funding rate 7 sàn](#3-khảo-sát-funding-rate-7-sàn)
4. [Thiết kế FundingData](#4-thiết-kế-fundingdata)
5. [Instrument registry](#5-instrument-registry)
6. [Bảng ánh xạ spot ↔ perp](#6-bảng-ánh-xạ-spot--perp)
7. [Các bẫy dữ liệu](#7-các-bẫy-dữ-liệu)
8. [Ngân sách rate limit](#8-ngân-sách-rate-limit)

---

## 1. NGUYÊN TẮC: CHIA THEO TẦN SUẤT

Sai lầm phổ biến là liệt kê endpoint rồi poll tất cả mỗi phút — cháy rate limit mà vẫn thiếu dữ liệu quan trọng. Phân loại theo **tốc độ thay đổi**:

```
A. TĨNH        → 1 lần/ngày, cache        → REST
B. CHẤM ĐIỂM   → mỗi 1–4 giờ              → REST
C. REALTIME    → liên tục                 → WebSocket
D. RECONCILE   → mỗi 5 phút + lúc khởi động → REST (cần key)
E. NGOÀI SÀN   → 1 lần/giờ                → nguồn khác
```

**Nguyên tắc vàng:** WebSocket cho mọi thứ liên tục, REST chỉ cho thứ WS không cấp được. Với thiết kế này, tải REST thực tế chỉ vài chục request/giờ.

**Ranh giới credential:** toàn bộ A, B, C (trừ user data stream) là **public, không cần API key**. Nghĩa là GĐ 2–3 xây và kiểm chứng được hoàn toàn trước khi tạo credential đầu tiên.

---

## 2. DANH MỤC DỮ LIỆU A – E

### A. TĨNH — 1 lần/ngày, cache

| Cần gì | Nguồn (Binance) | Vì sao bắt buộc |
|---|---|---|
| tickSize, **stepSize**, minNotional, trạng thái symbol | `exchangeInfo` — **cả spot và futures** | Sai bước giá → lệnh bị từ chối. `status != TRADING` là dấu hiệu sắp delist |
| **Leverage brackets** | `leverageBracket` | Chứa maintenance margin rate theo bậc notional — thứ quyết định **giá thanh lý**, không phải đòn bẩy bạn chọn. Vị thế lớn bị siết bậc tự động |
| Chu kỳ funding + cap/floor | `fundingInfo` | ⚠️ **Chỉ trả symbol có cấu hình LỆCH mặc định.** Code phải: mặc định 8h → ghi đè nếu symbol có trong danh sách. "Không có trong list" ≠ "không có funding" |
| Bậc phí thật của tài khoản | `commissionRate` (futures), `tradeFee` (spot) | **Đừng hardcode.** Bậc VIP đổi theo volume 30 ngày, và là biến số lớn nhất trong công thức edge |
| **Contract size / multiplier** | mỗi sàn khác nhau — xem §5 | OKX/Gate/Kraken tính theo *contract*, không theo coin. Sai 100× như chơi |
| **Bảng ánh xạ spot ↔ perp** | dựng từ 2 nguồn exchangeInfo | Xem §6 — nơi bot mở vị thế lệch coin |
| **Quy ước dấu funding** | tài liệu từng sàn | Đảo dấu = đảo chiều vị thế |

### B. CHẤM ĐIỂM — mỗi 1–4 giờ, REST

| Cần gì | Nguồn | Dùng để |
|---|---|---|
| Lịch sử funding 90–180 kỳ | `fundingRate` | Tỷ lệ kỳ dương, trung vị, độ lệch chuẩn — **điểm bền vững**, không phải rate tức thời |
| Backfill 6–12 tháng | `fundingRate` (chạy 1 lần, riêng) | Kho dữ liệu backtest ở GĐ 3 |
| Volume 24h | `ticker/24hr` | Lọc thanh khoản sơ bộ |
| **Độ sâu sổ lệnh, spot lẫn perp, `limit ≥ 100`** | `depth` | Tính slippage cho đúng size. Dùng best bid/ask để ước lượng là **nguồn sai số lớn nhất** giữa backtest và thực tế |
| Open interest theo thời gian | `openInterestHist` | Tuỳ chọn — funding cao + OI tăng nhanh = cấu hình dễ đảo chiều |

> Với độ sâu sổ lệnh: kiểm **phía bid của chân spot**, vì đó là chỗ bạn kẹt lúc thoát, không phải phía ask lúc vào.

### C. REALTIME — WebSocket, không poll

| Stream | Trả về | Ghi chú |
|---|---|---|
| `markPrice@1s` | Mark price, **funding rate ước tính đang chạy**, mốc funding kế tiếp, index price | Stream quan trọng nhất. Mark price là thứ tính giá thanh lý **và** tính phí funding — không phải giá last, không phải giá spot |
| `bookTicker` (spot + perp) | Best bid/ask | Theo dõi delta, định giá lệnh maker — **đã có trong repo** |
| **User data stream** | Fill lệnh, đổi vị thế, đổi số dư/ký quỹ | Thay thế hoàn toàn polling. ⚠️ Cần refresh `listenKey` định kỳ + tự reconnect. Mất stream mà không biết là kịch bản nguy hiểm |
| `depth` incremental | Sổ lệnh | **Chỉ bật lúc vào/ra lệnh, tắt khi giữ vị thế** |

> Funding rate ước tính **biến động suốt cửa sổ funding** và chỉ chốt tại thời điểm settle. Ra quyết định dựa trên giá trị đọc ở đầu cửa sổ là sai lệch hệ thống.

### D. RECONCILE — mỗi 5 phút + lúc khởi động (cần key)

| Cần gì | Nguồn | Dùng để |
|---|---|---|
| positionAmt, entryPrice, **liquidationPrice** | `positionRisk` | **Nguồn sự thật.** Đối chiếu state local mỗi 5 phút; lệch → dừng và báo |
| Số dư khả dụng, ký quỹ | `balance`/`account` (futures + spot) | Cảnh báo margin, tính buffer |
| **Funding thực nhận** | `income?incomeType=FUNDING_FEE` | Đối chiếu với funding **dự tính** từng kỳ. Đây là dữ liệu nói cho bạn biết mô hình sai ở đâu — quan trọng hơn mọi thứ khác về mặt học hỏi. **Log cả hai để so** |
| Phí thực trả từng fill | `userTrades` (futures), `myTrades` (spot) | Cost basis thật → lợi suất **trên vốn**, không phải trên notional |

### E. NGOÀI SÀN

| Cần gì | Vì sao |
|---|---|
| **Baseline lợi suất động** (T-bill token, sUSDe…) | Không có thì screener chỉ nói "cặp nào cao nhất", không nói "**có đáng bỏ vốn không**". Đây là hurdle rate |
| `serverTime` mỗi sàn | Lệch đồng hồ làm signature bị từ chối — và lỗi xuất hiện **đúng lúc thị trường biến động mạnh** |

---

## 3. KHẢO SÁT FUNDING RATE 7 SÀN

Khảo sát 2026-08-28. **Kết luận: không có 2 sàn nào giống nhau.**

### 3.1. Bảng tổng hợp

| Sàn | Kênh WS | Field rate | Field mốc kế tiếp | Chu kỳ | Độ tin cậy |
|---|---|---|---|---|---|
| **Binance** | `<sym>@markPrice@1s` | `r` | `T` (ms tuyệt đối) | 8h / 4h, biến động | ✅ Đã xác minh |
| **Bybit** | `tickers.<sym>` (100ms) | `fundingRate` | `nextFundingTime` (ms) | `fundingInterval` **PHÚT** (480) | ✅ Đã xác minh |
| **OKX** | `funding-rate` | `fundingRate` | **`fundingTime`** ⚠️ | 8h | 🟡 Nguồn bên thứ ba |
| **Gate** | `futures.tickers` | `funding_rate` + `funding_rate_indicative` | `funding_next_apply` | `funding_interval` **GIÂY** (28800) | 🟡 Nguồn bên thứ ba |
| **Kraken** | `ticker` | **`relative_funding_rate`** ⚠️ | `next_funding_rate_time` (ms **còn lại**) | settle **mỗi 1h**, realize 8h | ✅ Đã xác minh |
| **Hyperliquid** | `activeAssetCtx` | `funding` | — | **1 GIỜ** | ✅ Đã xác minh |
| **Paradex** | Funding V2 | funding index | **KHÔNG CÓ** | **LIÊN TỤC** (mỗi giây) | 🟡 Nguồn blog, cần verify |

### 3.2. Bốn phát hiện phá vỡ thiết kế ban đầu

**① OKX đảo ngược ngữ nghĩa "next"**

| | Binance | OKX |
|---|---|---|
| Rate kỳ sắp tới | `r` | `fundingRate` |
| Mốc kỳ sắp tới | **`T`** | **`fundingTime`** |
| Rate kỳ sau nữa | — | `nextFundingRate` |
| Mốc kỳ sau nữa | — | `nextFundingTime` |

Map `nextFundingTime` của OKX vào `NextFundingTime` là **sai một kỳ**. Bot sẽ vào lệnh trễ 8 tiếng.

**② Kraken `funding_rate` không phải tỷ lệ**

Giá trị mẫu trong tài liệu: `-6.2604214e-11`. Đây là **giá trị tuyệt đối theo đơn vị giá**, không phải phần trăm. Field so sánh được với các sàn khác là **`relative_funding_rate`**. Dùng nhầm field → annualize ra số vô nghĩa nhưng *trông vẫn hợp lệ* (không crash, không lỗi) — loại bug tệ nhất.

Ngoài ra Kraken settle **mỗi giờ**, nhưng rate được yết cho **cửa sổ realize 8h** (hệ số n=8). Cap `±0.5%/giờ` ứng với biên độ 800bp cho 8h. Nhân `× 24 × 365` là sai gấp 8 lần.

**③ Ba sàn, ba đơn vị cho cùng một khái niệm**

```
Binance  fundingIntervalHours = 8       (GIỜ)
Bybit    fundingInterval      = 480     (PHÚT)
Gate     funding_interval     = 28800   (GIÂY)
```

Đọc thẳng vào cùng một trường `int` là bug chắc chắn xảy ra. **Phải chuẩn hoá về giây ngay tại tầng connector.**

**④ Paradex không có mốc funding rời rạc**

Funding V2 (từ 2026-06-16) tính lại **mỗi giây**, làm mượt bằng EWMA nửa đời 30 phút, tích luỹ liên tục qua *funding index*, và chỉ settle khi vị thế được điều chỉnh on-chain.

→ `NextFundingTime` **không tồn tại** với Paradex. Một struct bắt buộc có field này sẽ hoặc phải điền giá trị giả, hoặc phải loại Paradex — cả hai đều sai.

Hệ quả tương tự với Hyperliquid ở mức nhẹ hơn: chu kỳ 1h, nếu tính APY bằng `rate × 3 × 365` sẽ **sai 8 lần**.

---

## 4. THIẾT KẾ `FundingData`

### 4.1. Ba yêu cầu rút ra từ khảo sát

1. Phải phân biệt **mô hình funding rời rạc** (Binance, Bybit, OKX, Gate, Kraken, Hyperliquid) và **liên tục** (Paradex) → cần enum, không dùng field bắt buộc.
2. Phải giữ **cả rate thô lẫn rate chuẩn hoá**. Rate thô để đối chiếu với web sàn khi debug; rate chuẩn hoá để so sánh và tính APY.
3. Chuẩn hoá đơn vị **tại tầng connector**, không phải ở tầng tính toán. Tầng signal không bao giờ được nhìn thấy phút/giây/giờ lẫn lộn.

### 4.2. Struct đề xuất

```go
// exchanges/types.go

type FundingModel string

const (
    FundingDiscrete   FundingModel = "discrete"   // settle tại mốc cố định
    FundingContinuous FundingModel = "continuous" // tích luỹ liên tục (Paradex)
)

type FundingData struct {
    // --- Định danh ---
    Symbol string       // đã chuẩn hoá: BTCUSDT
    Source string       // binance_futures, okx_futures, ...
    Model  FundingModel

    // --- Rate thô (giữ nguyên như sàn trả, để debug/đối chiếu) ---
    RawRate      float64
    RawRateField string  // tên field đã dùng: "r", "relative_funding_rate", ...

    // --- Rate đã chuẩn hoá (tầng signal CHỈ dùng phần này) ---
    RatePerInterval float64 // rate cho đúng 1 chu kỳ settle của sàn này
    IntervalSec     int64   // chu kỳ settle, GIÂY — luôn chuẩn hoá về giây
    RatePer8h       float64 // quy về 8h để so sánh chéo sàn
    APRAnnualized   float64 // = RatePerInterval × (31_536_000 / IntervalSec)

    // --- Mốc thời gian (chỉ có nghĩa khi Model == FundingDiscrete) ---
    NextFundingTime int64 // ms tuyệt đối; 0 nếu Model == Continuous
    IsEstimated     bool  // true = rate dự kiến đang chạy; false = đã chốt

    // --- Rate kỳ sau nữa (chỉ OKX cung cấp) ---
    FollowingRate     float64
    FollowingTime     int64
    HasFollowingRate  bool

    // --- Giới hạn ---
    RateCap   float64
    RateFloor float64
    HasCap    bool

    // --- Giá liên quan ---
    MarkPrice  float64 // dùng để tính phí funding — KHÔNG dùng entry price
    IndexPrice float64

    // --- Metadata ---
    RateType  string // Binance: "Regular" | "Special" (dividend) — lọc khi backtest
    Timestamp int64  // thời điểm sàn phát
    RecvTime  int64  // thời điểm bot nhận — dùng cho staleness filter
}
```

### 4.3. Quy tắc điền theo từng sàn

| Sàn | `RawRate` từ | `IntervalSec` | `NextFundingTime` | Ghi chú |
|---|---|---|---|---|
| Binance | `r` | `fundingIntervalHours × 3600`, **mặc định 28800** | `T` | Ghi đè interval nếu symbol có trong `fundingInfo` |
| Bybit | `fundingRate` | `fundingInterval × 60` | `nextFundingTime` | ⚠️ Ticker là snapshot+delta — **merge vào cache**, không ghi đè |
| OKX | `fundingRate` | 28800 | **`fundingTime`** | `nextFundingRate`/`nextFundingTime` → `FollowingRate`/`FollowingTime` |
| Gate | `funding_rate` | `funding_interval` (đã là giây) | `funding_next_apply × 1000` | `funding_rate_indicative` → dùng khi cần rate dự kiến |
| Kraken | **`relative_funding_rate`** | 3600 | `now + next_funding_rate_time` | ⚠️ Field là **ms còn lại**, không phải mốc tuyệt đối. Rate yết cho cửa sổ 8h → chia 8 khi quy về 1h |
| Hyperliquid | `funding` | **3600** | mốc giờ tròn kế tiếp | Chu kỳ 1h, không phải 8h |
| Paradex | funding index | — | **0** | `Model = FundingContinuous`; APR suy từ biến thiên index |

### 4.4. Refactor kèm theo

Signature connector hiện nhận 3 channel và được gọi ở 10 chỗ ([main.go:357-372](../main.go#L357-L372)). Thêm channel thứ 4 làm signature phình to. Gom lại:

```go
type Feeds struct {
    Price     chan<- PriceData
    Orderbook chan<- OrderbookData
    Trade     chan<- TradeData
    Funding   chan<- FundingData
}

func ConnectBinanceFutures(symbols []string, f Feeds)
```

---

## 5. INSTRUMENT REGISTRY

Nhóm dữ liệu bị đánh giá thấp nhất, nhưng là **nguyên nhân số 1 khiến vị thế "delta-neutral" thực ra không neutral**.

```go
type Instrument struct {
    Symbol       string
    Source       string
    MarketType   string  // spot | perp | future

    TickSize     float64 // bước giá
    StepSize     float64 // bước khối lượng  ⚠️ SPOT VÀ PERP KHÁC NHAU
    MinQty       float64
    MinNotional  float64

    ContractSize float64 // 1.0 nếu tính theo coin
    IsContract   bool    // true = đặt lệnh theo SỐ CONTRACT, không theo coin

    Status       string  // TRADING | BREAK | ...
    MaxLeverage  float64
}
```

**Nguồn contract size theo sàn:**

| Sàn | Đơn vị đặt lệnh | Field |
|---|---|---|
| Binance | Coin | — (`IsContract = false`) |
| Bybit linear | Coin | — (`IsContract = false`) |
| OKX | **Contract** | `ctVal` × `ctMult` |
| Gate | **Contract** | `quanto_multiplier` (số coin mỗi contract) |
| Kraken | Contract | theo spec từng hợp đồng |
| Hyperliquid | Coin | `szDecimals` |
| Paradex | Coin | — |

**Bẫy delta:** `stepSize` của spot và perp khác nhau. Làm tròn khối lượng 2 chân theo 2 bộ quy tắc mà không xử lý → delta ≠ 0 ngay từ lệnh đầu tiên, và sai số đó tồn tại suốt vòng đời vị thế.

Cách xử lý: tính size mục tiêu → làm tròn **xuống** theo `stepSize` **lớn hơn** của hai phía → kiểm tra vẫn thoả `minNotional` của **cả hai** → mới đặt lệnh.

---

## 6. BẢNG ÁNH XẠ SPOT ↔ PERP

Hiện tại repo dùng `switch` hardcode trong từng file connector. Không mở rộng được và có bug tiềm ẩn.

**Quy ước symbol hiện có:**

| Sàn | Định dạng | Ví dụ BTC |
|---|---|---|
| Binance | `BTCUSDT` | `BTCUSDT` |
| Bybit | `BTCUSDT` | `BTCUSDT` |
| OKX | `BASE-QUOTE-SWAP` | `BTC-USDT-SWAP` |
| Gate | `BASE_QUOTE` | `BTC_USDT` |
| Kraken | `PF_<XBT>USD` | `PF_XBTUSD` — **BTC gọi là XBT**, quote là USD không phải USDT |
| Hyperliquid | chỉ base coin | `BTC` |
| Paradex | `BASE-USD-PERP` | `BTC-USD-PERP` |

**🐛 Bug tiềm ẩn đã phát hiện:** [hyperliquid.go:58](../exchanges/hyperliquid.go#L58) dùng `coin := symbol[:3]` — cắt cứng 3 ký tự đầu. Hiện chạy đúng chỉ vì cả 4 symbol (BTC/ETH/XRP/SOL) đều có base 3 ký tự. Thêm `DOGEUSDT` → `"DOG"`, `AVAXUSDT` → `"AVA"` → subscribe sai coin, im lặng không báo lỗi.

**Yêu cầu bảng ánh xạ:**

1. Dựng **tự động** từ `exchangeInfo` của từng sàn, không hardcode.
2. **Xác thực từng chiều**: perp tồn tại ⟺ spot tương ứng tồn tại.
3. Chú ý khác quote: Kraken `PF_XBTUSD` là **USD**, không phải USDT — basis khác nhau, không hedge được bằng spot USDT nếu không tính chênh USD/USDT.
4. Cặp không ghép được → **bot phải từ chối**, không được đoán. Đây là nơi bot mở vị thế lệch coin.

---

## 7. CÁC BẪY DỮ LIỆU

Xếp theo mức tốn kém:

**1. Ghép cặp spot ↔ perp sai** → mở vị thế lệch coin, không phải hedge mà là hai vị thế đầu cơ. Xem §6.

**2. Làm tròn 2 chân theo 2 `stepSize` khác nhau** → delta ≠ 0 từ lệnh đầu. Xem §5.

**3. Funding là sự kiện RỜI RẠC, không phải dòng chảy liên tục.** Chỉ trả/nhận nếu **đang giữ vị thế đúng timestamp settle**. Giữ 7h59 rồi đóng = nhận 0đ. Mọi công thức `APY × số ngày giữ` đều sai — phải **đếm số mốc settle đã đi qua**. (Ngoại lệ: Paradex tích luỹ liên tục.)

**4. Funding tính trên `mark price × size` tại thời điểm settle**, không phải giá vào lệnh. Số thực nhận luôn lệch dự tính. **Log cả hai để so** — không log thì không biết lệch do mô hình sai hay do sàn tính khác.

**5. Chu kỳ funding không đồng nhất** — Hyperliquid 1h, Kraken settle 1h/realize 8h, Binance 8h hoặc 4h, Paradex liên tục. Hardcode `× 3 × 365` sai từ 1.5× đến 8×.

**6. Dấu và đơn vị của rate khác nhau giữa các sàn.** Kraken `funding_rate` là tuyệt đối. Verify từng sàn bằng cách đối chiếu số hiển thị trên web sàn.

**7. Bybit ticker là snapshot + delta.** Field không xuất hiện trong message = **chưa đổi**, không phải = 0. Phải merge vào state cache. Ghi đè mù → funding rate nhảy về 0 ngẫu nhiên.

**8. `fundingInfo` của Binance chỉ trả symbol lệch mặc định.** Xem §2.A.

**9. `rateType: Special`** (Binance) — rate bất thường do dividend, phải lọc khi backtest.

**10. Không có API nào báo trước delisting.** Với cặp thanh khoản mỏng — đúng loại cơ hội đuôi lợi suất cao — rủi ro thật là symbol chuyển `BREAK` hoặc bị hạ bậc đòn bẩy đột ngột. Cách duy nhất: kiểm `status` trong `exchangeInfo` mỗi ngày + luật thoát tự động khi trạng thái đổi.

---

## 8. NGÂN SÁCH RATE LIMIT

- **WebSocket cho mọi thứ liên tục.** REST chỉ cho thứ WS không cấp được.
- **Đọc header weight sau mỗi request**, tự giảm tần suất khi vượt **70%** hạn mức.
- Binance: `fundingRate` và `fundingInfo` chia sẻ hạn mức **500/5min/IP**.
- Bị ban IP giữa lúc đang có vị thế mở là tình huống không được phép xảy ra.

**Tải REST ước tính với thiết kế trên:**

| Nhóm | Tần suất | Request/giờ (7 sàn) |
|---|---|---|
| A. Tĩnh | 1 lần/ngày | ~2 |
| B. Chấm điểm | mỗi 2h | ~20 |
| D. Reconcile | mỗi 5 phút | ~84 |
| **Tổng** | | **~106/giờ** — rất nhẹ |

---

## PHỤ LỤC — ĐỘ TIN CẬY & VIỆC CẦN XÁC MINH

Tên endpoint và cấu trúc field **có thay đổi**. Cấu trúc dữ liệu cần thì ổn định, tên endpoint thì không. Kiểm tra lại tài liệu hiện hành trước khi code.

| Mục | Trạng thái |
|---|---|
| Binance `fundingRate`, `fundingInfo`, `markPrice` fields | ✅ Xác minh trên docs chính thức |
| Bybit `fundingInterval` (phút), snapshot+delta, lotSizeFilter | ✅ Xác minh trên docs chính thức |
| Kraken settle 1h / realize 8h, `relative_funding_rate` | ✅ Xác minh trên docs chính thức |
| Hyperliquid funding 1h | ✅ Xác minh |
| OKX `fundingTime` vs `nextFundingTime` | 🟡 Nguồn bên thứ ba — **verify trước khi code** |
| Gate `funding_interval` = 28800 giây | 🟡 Nguồn bên thứ ba — **verify** |
| Paradex Funding V2 liên tục | 🟡 Nguồn blog — **verify trên API thật** |
| Kraken `funding_rate` tuyệt đối vs `relative_` | 🟡 Suy từ giá trị mẫu `-6.26e-11` — **verify bằng cách đối chiếu số trên web sàn** |

**Cách verify rẻ nhất:** viết một script đọc funding rate của BTC từ cả 7 sàn, in ra cạnh nhau cùng với số hiển thị trên web từng sàn. Sai đơn vị hay sai field sẽ lộ ra ngay ở bước này — trước khi nó kịp đi vào logic tính toán.
