# YÊU CẦU DỮ LIỆU — Funding Rate Arbitrage Bot

> **Cập nhật:** 2026-08-28 · khảo sát §3 xác minh trên API thật 2026-09-03 (Bước 2.1, `cmd/fundingcheck`)
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
| **Độ sâu sổ lệnh, spot lẫn perp, `limit ≥ 100`** | `depth` | Tính slippage cho đúng size. Dùng best bid/ask để ước lượng là **nguồn sai số lớn nhất** giữa backtest và thực tế. Cũng là dữ liệu bắt buộc để **xếp hạng cơ hội** — thiếu nó, screener đẩy cặp APR cao/sổ mỏng lên đầu |
| Open interest theo thời gian | `openInterestHist` | Tuỳ chọn — funding cao + OI tăng nhanh = cấu hình dễ đảo chiều |

> Với độ sâu sổ lệnh: kiểm **phía bid của chân spot**, vì đó là chỗ bạn kẹt lúc thoát, không phải phía ask lúc vào.
>
> Và độ sâu **lúc vào không dự báo được độ sâu lúc ra**. Bạn vào khi funding hấp dẫn, tức thị trường bình thường; bạn thoát khi funding đảo chiều — điều này tương quan với căng thẳng thị trường, đúng lúc sổ mỏng đi. Đây là bất đối xứng cấu trúc, không phải rủi ro đuôi.

**Khối lượng đỉnh sổ — đang có sẵn miễn phí:** `OrderbookData` hiện chỉ mang giá, nhưng connector đã parse sẵn khối lượng rồi bỏ đi — Binance `B`/`A`, Bybit `v`, OKX `sz`. Không thay được độ sâu đầy đủ, nhưng cho bộ lọc thanh khoản bậc một mà không tốn thêm băng thông. Thu ở **Bước 1.2**.

**Vì sao funding arb ít nhạy với slippage hơn arbitrage giá:** lợi nhuận đến từ phí funding, còn chi phí vào/ra là chi phí một lần khấu hao theo thời gian giữ. Với Binance taker cả hai chân, riêng phí đã là 0,30% vòng lặp; ở funding 0,01%/8h thì hoà vốn sau ~10 ngày, thêm 0,20% slippage thành ~17 ngày. Slippage **không giết giao dịch mà kéo dài thời gian hoà vốn ~70%** — nó quyết định cơ hội nào đủ tiêu chuẩn. Lập luận đầy đủ ở [PLAN.md §7.4](PLAN.md#74-chiến-lược-độ-sâu-sổ-lệnh).

### C. REALTIME — WebSocket, không poll

| Stream | Trả về | Ghi chú |
|---|---|---|
| `markPrice@1s` | Mark price, **funding rate ước tính đang chạy**, mốc funding kế tiếp, index price | Stream quan trọng nhất. Mark price là thứ tính giá thanh lý **và** tính phí funding — không phải giá last, không phải giá spot |
| `bookTicker` (spot + perp) | Best bid/ask | Theo dõi delta, định giá lệnh maker — **đã có trong repo** |
| **User data stream** | Fill lệnh, đổi vị thế, đổi số dư/ký quỹ | Thay thế hoàn toàn polling. ⚠️ Cần refresh `listenKey` định kỳ + tự reconnect. Mất stream mà không biết là kịch bản nguy hiểm |
| `depth` incremental | Sổ lệnh | **Chỉ bật lúc vào/ra lệnh, tắt khi giữ vị thế.** Funding arb giữ vị thế hàng ngày đến hàng tuần — không có lý do dựng lại sổ lệnh realtime (sequence number, phát hiện gap, resync) khi mỗi tuần chỉ giao dịch một lần |

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
| **Binance** | `<sym>@markPrice@1s` | `r` | `T` (ms tuyệt đối) | 8h / 4h / **1h** theo symbol (§3.3②) | ✅ Đã xác minh |
| **Bybit** | `tickers.<sym>` (100ms) | `fundingRate` | `nextFundingTime` (ms) | `fundingInterval` **PHÚT** (480) | ✅ Đã xác minh |
| **OKX** | `funding-rate` | `fundingRate` | **`fundingTime`** ⚠️ | 8h | ✅ Đo trực tiếp 2026-09-03 |
| **Gate** | `futures.tickers` | `funding_rate` + `funding_rate_indicative` | `funding_next_apply` (epoch **GIÂY**) | `funding_interval` **GIÂY** (28800) | ✅ Đo trực tiếp 2026-09-03 |
| **Kraken** | `ticker` | **`relative_funding_rate`** ⚠️ | `next_funding_rate_time` (**epoch ms TUYỆT ĐỐI** — sửa 2026-09-03, xem §3.3⑥) | settle **mỗi 1h**, rate là **mỗi-1h** (sửa 2026-09-03 — xem ②) | ✅ Probe WS thật 2026-09-03 |
| **Hyperliquid** | `activeAssetCtx` | `funding` | — | **1 GIỜ** (field API đã là rate/1h) | ✅ Đã xác minh |
| **Paradex** | Funding V2 | funding index + `funding_rate_8h` | **KHÔNG CÓ** | **LIÊN TỤC** (điểm mỗi ~5s, rate yết cho cửa sổ 8h) | ✅ Đo trực tiếp 2026-09-03 |

> **Phạm vi của các ✅ 2026-09-03:** `cmd/fundingcheck` xác minh **ngữ nghĩa
> field trên REST** (Bybit là ví dụ tại sao phải tách bạch: hai endpoint của
> cùng một sàn yết interval bằng hai đơn vị). Shape payload **WebSocket** của
> từng kênh trong cột "Kênh WS" xác minh khi viết connector ở Bước 2.5, bằng
> golden test trên frame thật — đúng quy trình Bước 1.6.

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

Xác minh 2026-09-03 trên `GET /derivatives/api/v4/historicalfundingrates?symbol=PF_XBTUSD`
(trả **cả hai** field cho từng mốc giờ): `fundingRate ÷ relativeFundingRate` = 77.802
≈ index price (lệch 0,09%) — đúng định nghĩa *absolute = relative × spot*.

> **⚠️ SỬA 2026-09-03 — bản khảo sát cũ ghi ngược.** Trước đây mục này viết
> "rate được yết cho cửa sổ realize 8h (hệ số n=8), chia 8 khi quy về 1h".
> **Sai.** Spec hợp đồng hiện hành của Kraken viết: *"The absolute rate is the
> amount of funding an account will receive by maintaining a 1 contract unit
> short position for **1 hour**"*, và ví dụ trong spec dùng "relative rate set
> for the 1-hour period"
> ([Linear Multi-Collateral Contract Specifications](https://support.kraken.com/articles/4844359082772-linear-multi-collateral-derivatives-contract-specifications)).
> Đo chéo sàn cùng thời điểm xác nhận: `relative_funding_rate` ≈ 1,07e-5/giờ,
> **×8 = 8,5e-5** rơi đúng giữa cụm rate/8h của 6 sàn kia (4,4e-5…1,0e-4);
> nếu chia 8 như bản cũ, Kraken sẽ hiển thị rẻ hơn thực tế **8 lần**.
> Quy tắc đúng: `RatePerIntervalFrac = relative_funding_rate` (nguyên),
> `IntervalSec = 3600`, `RatePer8hFrac = ×8`.
>
> 🟡 **Mới, chưa xác minh được bằng dữ liệu công khai:** spec nói payout nhân với
> *"the time elapsed within the funding period without position alteration"* và
> "you will immediately begin receiving funding" — tức Kraken có thể **tích luỹ
> pro-rata trong giờ** thay vì chốt rời rạc tại mốc. Không ảnh hưởng tầng monitor
> (rate giống nhau); ảnh hưởng luật "đếm mốc settle" ở GĐ 3–4 → xác minh bằng
> funding payment thật khi có credential (GĐ 4, `income`).

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

### 3.3. Phát hiện mới từ Bước 2.1 (đo trực tiếp 2026-09-03, `cmd/fundingcheck`)

1. **OKX không còn công bố rate kỳ sau nữa.** `nextFundingRate` trả về **chuỗi
   rỗng** khi `method = "current_period"` (kèm `formulaType: "withRate"`).
   → `FollowingRate` trong thiết kế §4 phải là optional (`HasFollowingRate`);
   không được parse "" thành 0. Bù lại OKX công bố cap tường minh:
   `maxFundingRate`/`minFundingRate` = ±0,375%.
2. **Binance `fundingInfo` hiện phủ toàn bộ perp TRADING** — 777 symbol, không
   thiếu symbol TRADING nào, BTCUSDT có mặt (cap ±0,3% của nó tính là "adjusted").
   Docs vẫn chỉ hứa trả symbol lệch mặc định, nên **giữ nguyên pattern mặc-định-8h-
   rồi-ghi-đè**; độ phủ hôm nay là sự kiện đo được, không phải cam kết hợp đồng.
   Phân bố interval 2026-09-03: **4h: 443 · 8h: 331 · 1h: 3** (TUSDT, ONGUSDT,
   SKRUSDT) — 4h chiếm đa số, và **1h đã tồn tại trên Binance**: mọi chỗ nói
   "Binance 8h hoặc 4h" phải đọc là "8h, 4h hoặc 1h, theo `fundingInfo`".
3. **Bybit trả interval bằng hai đơn vị ở hai endpoint.** `instruments-info`:
   `fundingInterval` = 480 (**PHÚT**, docs ghi "Funding interval (minute)");
   REST `tickers`: `fundingIntervalHour` = "8" (**GIỜ**). Đơn vị thứ tư cho cùng
   một khái niệm trong hệ — connector chuẩn hoá về giây, không đọc lẫn.
4. **Paradex tự mô tả cửa sổ yết.** `GET /v1/funding/data` trả `funding_rate_8h`
   và `funding_period_hours: 8` tường minh, điểm dữ liệu mỗi ~5s, `funding_index`
   tăng đơn điệu — không cần "suy APR từ biến thiên index" như dự kiến cũ; đọc
   thẳng `funding_rate`/`funding_rate_8h`, index chỉ dùng đối chiếu.
5. **Hyperliquid: field `funding` của API là rate mỗi-1h đã chia sẵn.** Docs:
   *"The funding rate formula applies to 8 hour funding rate. However, funding is
   paid every hour at one eighth of the computed rate"* — và giá trị đo
   (1,25e-5 ×8 = 1,0e-4) rơi đúng cụm 8h chéo sàn, tức API công bố phần **đã ÷8**.
   Dùng nguyên với `IntervalSec = 3600`.
6. **Kraken WS `next_funding_rate_time` là epoch ms TUYỆT ĐỐI — khảo sát cũ ghi
   ngược lần thứ hai.** (Phát hiện trong review Bước 2.2, xác minh bằng **hai**
   probe WS độc lập 2026-09-03.) Mô tả field trong docs sàn viết *"time until
   next funding rate in milliseconds"*, nhưng sample payload của **chính trang
   đó** và feed thật đều trả mốc tuyệt đối: probe lúc 08:36Z nhận
   `next_funding_rate_time = 1788426000000` = **09:00:00Z tròn giờ**, cách probe
   ~24 phút ([docs ticker](https://docs.kraken.com/api/docs/futures-api/websocket/ticker/)).
   Cộng "now" vào như bản cũ chỉ dẫn sẽ ra mốc settle ở năm ~2083 → tầng đếm
   settle kết luận Kraken không bao giờ settle. Cùng frame đó:
   `relative_funding_rate` khớp đúng giá trị giờ 08:00 đã settle của
   `/v4/historicalfundingrates` (củng cố thêm ②), và ticker WS có field `time`
   (epoch ms) — nguồn venue-time thật cho Kraken mà nợ GĐ 1 đang cần.

---

### 3.4. Phát hiện mới từ Bước 2.5 (đo trực tiếp 2026-09-04, probe WS + REST)

Ba phát hiện dưới đây đến từ việc mở THẬT các kênh funding của 7 sàn, và cả ba
đều đổi thiết kế đã định trong PLAN.

⑦ **Binance: kênh `@markPrice@1s` KHÔNG đẩy dữ liệu tới môi trường này.**
PLAN Bước 2.5 ghi `@markPrice@1s` là nguồn funding của Binance. Đo được:
một socket `wss://fstream.binance.com/ws` subscribe ĐỒNG THỜI
`btcusdt@markPrice@1s` và `btcusdt@bookTicker`, server xác nhận
(`{"result":null,"id":1}`), rồi trong **45 giây** chuyển **4.782 frame
bookTicker và 0 frame markPriceUpdate**. Thử cả ba dạng URL (combined
`/stream?streams=`, raw `/ws/<stream>`, và SUBSCRIBE tường minh) đều như nhau.
Không kết luận được nguyên nhân (chặn theo vùng, theo IP, hay chính sách sàn),
nên **không đoán**: Binance funding chuyển sang REST `premiumIndex`, endpoint
công khai trả đúng `lastFundingRate`, `nextFundingTime`, `markPrice`,
`indexPrice`, `time` và trả lời mọi lần. Poll 15s, gọi **theo từng symbol**
(`?symbol=`): dạng không lọc trả 198.811 byte cho ~780 hợp đồng — 1,1 GB/ngày
để lấy 4 dòng — và tốn request weight 10 so với 1.

⑧ **Hyperliquid: `nextFundingTime` là mốc của kỳ ĐANG CHẠY, không phải kỳ kế.**
Cùng họ với bẫy ① của OKX nhưng lệch theo hướng ngược lại. Đo qua ranh giới giờ
ngày 2026-09-04:

| Lúc đo (UTC) | `HlPerp.nextFundingTime` | Trạng thái |
|---|---|---|
| 02:47:49 | 02:00:00 | đã qua 47 phút |
| 03:01:30 | 03:00:00 | đã qua 90 giây |

Trong CÙNG response, `BinPerp` và `BybitPerp` trả 08:00:00Z — một mốc tương
lai đúng nghĩa. Vậy mốc settlement kế tiếp của Hyperliquid =
`nextFundingTime + fundingIntervalHours`. Lấy nguyên field sẽ công bố một thời
điểm đã trôi qua: đồng hồ đếm ngược chạy ngược, và phép đếm settlement ở GĐ 3
sẽ tính công một kỳ đã trả. Connector cộng thêm một chu kỳ **và** vẫn lọc qua
`futureStampMs`, vì metadata refresh mỗi 5 phút trong khi sàn settle mỗi giờ.

⑨ **Bybit `fundingIntervalHour` là CHUỖI `"8"`, không phải số.** Khai báo
`*int64` làm cả bản tin decode hỏng — và vì handler nào cũng "thử decode rồi bỏ
qua nếu không khớp", frame rơi xuống nhánh orderbook và **Bybit không phát ra
funding nào cả**, im lặng. Golden test bắt được ngay lần chạy đầu. Bybit cũng
chỉ công bố `fundingCap`, **không có** `fundingFloor` — nên `FundingData` tách
`HasCap`/`HasFloor` riêng: gộp một cờ sẽ khiến "chưa biết sàn" đọc thành "sàn
này không bao giờ trả funding âm".

---

## 4. THIẾT KẾ `FundingData`

### 4.1. Ba yêu cầu rút ra từ khảo sát

1. Phải phân biệt **mô hình funding rời rạc** (Binance, Bybit, OKX, Gate, Kraken, Hyperliquid) và **liên tục** (Paradex) → cần enum, không dùng field bắt buộc.
2. Phải giữ **cả rate thô lẫn rate chuẩn hoá**. Rate thô để đối chiếu với web sàn khi debug; rate chuẩn hoá để so sánh và tính APY.
3. Chuẩn hoá đơn vị **tại tầng connector**, không phải ở tầng tính toán. Tầng signal không bao giờ được nhìn thấy phút/giây/giờ lẫn lộn.

### 4.2. Struct — như đã xây ở Bước 2.2 (`exchanges/funding.go`)

> Bản nháp trước 2026-09-03 dùng tên không hậu tố (`RatePerInterval`,
> `NextFundingTime`, `RecvTime int64`). Bản xây thật đổi theo đúng
> [CONVENTIONS.md §1](CONVENTIONS.md): mọi field mang đơn vị nói ra đơn vị,
> mốc tuyệt đối trên wire mang `AtMs`, thời điểm nhận nội bộ là `RecvAt
> time.Time` cùng hợp đồng đồng hồ với `PriceData` (đóng dấu tại socket read).

```go
// exchanges/funding.go

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
    RatePerIntervalFrac float64 // rate cho đúng 1 chu kỳ settle của sàn này
    IntervalSec         int64   // chu kỳ settle, GIÂY — luôn chuẩn hoá về giây
    RatePer8hFrac       float64 // quy về 8h để so sánh chéo sàn
    APRFrac             float64 // = RatePerIntervalFrac × (31_536_000 / IntervalSec)

    // --- Mốc thời gian (chỉ có nghĩa khi Model == FundingDiscrete) ---
    NextFundingAtMs int64 // ms tuyệt đối; 0 = continuous hoặc endpoint không công bố
    IsEstimated     bool  // true = rate dự kiến đang chạy; false = đã chốt

    // --- Rate kỳ sau nữa (chỉ OKX từng cung cấp; hiện trả rỗng — §3.3①) ---
    FollowingRateFrac float64
    FollowingAtMs     int64
    HasFollowingRate  bool

    // --- Giới hạn sàn công bố ---
    RateCapFrac   float64
    RateFloorFrac float64
    HasCap        bool

    // --- Giá liên quan ---
    MarkPrice  float64 // funding tính trên mark price — KHÔNG dùng entry price
    IndexPrice float64

    // --- Metadata ---
    RateType    string    // Binance: "Regular" | "Special" (dividend) — lọc khi backtest
    VenueTimeMs int64     // đồng hồ sàn, 0 nếu sàn không công bố — không đo staleness
    RecvAt      time.Time // đóng dấu tại socket read — cơ sở duy nhất của staleness
}
```

Bảy hàm `normalize<Venue>Funding` (một mỗi sàn, mỗi hàm một file `<venue>_funding.go`) là **nơi duy nhất** giữ quy tắc
đơn vị của §4.3: connector Bước 2.5 chỉ parse JSON rồi đưa số thô theo đơn vị
của sàn vào builder; hạ nguồn chỉ thấy giây và phân số. Interval không dương →
builder từ chối (không cho một message hỏng sinh ra APR Inf/NaN).

### 4.3. Quy tắc điền theo từng sàn

Mỗi hàng dưới đây là một builder `normalize<Venue>Funding` trong
`exchanges/<venue>_funding.go` — connector Bước 2.5 chỉ parse JSON rồi gọi nó.

| Sàn | `RawRate` từ | `IntervalSec` | `NextFundingAtMs` | Ghi chú |
|---|---|---|---|---|
| Binance | `r` | `fundingIntervalHours × 3600`, **mặc định 28800** | `T` | Ghi đè interval nếu symbol có trong `fundingInfo`; 1h/4h/8h đều tồn tại (§3.3②) |
| Bybit | `fundingRate` | `fundingInterval × 60` | `nextFundingTime` | ⚠️ Ticker là snapshot+delta — **merge vào cache**, không ghi đè |
| OKX | `fundingRate` | **suy từ `nextFundingTime − fundingTime`** (sàn không công bố interval — không truyền hằng số) | **`fundingTime`** | `nextFundingRate`/`nextFundingTime` → `FollowingRateFrac`/`FollowingAtMs`, nhưng `nextFundingRate` hiện trả **rỗng** (§3.3①) → `HasFollowingRate = false` |
| Gate | `funding_rate` | `funding_interval` (đã là giây) | `funding_next_apply × 1000` | `funding_rate_indicative` → dùng khi cần rate dự kiến |
| Kraken | **`relative_funding_rate`** | 3600 (ngữ nghĩa sàn, ghim trong builder — xem chú thích tại `kraken_funding.go`) | `next_funding_rate_time` **dùng thẳng — đã là mốc tuyệt đối** (sửa 2026-09-03, §3.3⑥) | Rate là **mỗi-1h, dùng nguyên** — `RatePer8hFrac = ×8` (sửa 2026-09-03, §3.2②) |
| Hyperliquid | `funding` | `fundingIntervalHours × 3600` từ `predictedFundings` (sàn tự khai, =1h) | `nextFundingTime` từ `predictedFundings` | Field API đã là rate/1h (đã ÷8 — §3.3⑤) |
| Paradex | `funding_rate` (+ funding index đối chiếu) | `funding_period_hours × 3600` (=28800) — là **cửa sổ yết**, không phải chu kỳ settle | **0** | `Model = FundingContinuous`; rate yết cho cửa sổ 8h có sẵn — không cần suy từ index (§3.3④) |

### 4.4. Refactor kèm theo — ✅ xong

Dự kiến ban đầu: gom 3 channel thành `Feeds` để channel thứ 4 không phình
signature. Bước 1.5 đã gom (kèm `Ctx` và `Conn`); Bước 2.2 thêm đúng một field
`Funding chan<- FundingData` + `SendFunding` (cùng ngữ nghĩa bỏ-cuộc-khi-huỷ
với `SendPrice`) và **không đổi chữ ký connector nào**. Scanner nhận qua
`fundingChan`, giữ bản ghi mới nhất theo symbol × source làm điểm thu cho
2.6 (persistence) và 2.7 (dashboard).

---

## 5. INSTRUMENT REGISTRY — ✅ xây ở Bước 2.3 (2026-09-03)

Nhóm dữ liệu bị đánh giá thấp nhất, nhưng là **nguyên nhân số 1 khiến vị thế "delta-neutral" thực ra không neutral**.

Như đã xây (`exchanges/instruments.go` + `<venue>_instruments.go`,
`internal/instruments`): **mọi khối lượng quy về COIN ngay ở tầng exchanges**
(hậu tố nói rõ — CONVENTIONS §1), sàn contract giữ thêm `IsContract` +
`ContractSizeCoin` để GĐ 4 đổi ngược ra số contract khi đặt lệnh.

```go
type Instrument struct {
    Symbol       string  // đã chuẩn hoá: BTCUSDT
    NativeSymbol string  // BTC-USDT-SWAP, PF_XBTUSD, ...
    Source       string
    MarketType   string  // spot | perp

    Status       string  // "trading" đã chuẩn hoá (TRADING/Trading/live/tradeable);
                         // từ khác giữ nguyên chữ của sàn để lời từ chối nêu ra

    TickSizeQuote float64 // bước giá (quote); 0 = sàn định bằng QUY TẮC (Hyperliquid: ≤5 chữ số có nghĩa)

    StepSizeCoin     float64 // bước khối lượng, COIN  ⚠️ SPOT VÀ PERP KHÁC NHAU
    MinQtyCoin       float64
    MaxQtyCoin       float64 // 0 = sàn không công bố
    MinNotionalQuote float64 // 0 = sàn không công bố — không phải "không có sàn nào ép"

    IsContract       bool    // true = đơn vị đặt lệnh gốc là SỐ CONTRACT
    ContractSizeCoin float64 // coin mỗi contract; 1.0 khi đặt theo coin

    MaxLeverageX float64 // 0 = không công bố công khai (Binance: leverageBracket cần ký — GĐ 4)
}
```

Registry (`internal/instruments.Registry`): `Run(ctx)` fetch 9 nguồn **song
song** 1 lần/ngày (`RefreshInterval`), lỗi thì thử lại sau 5 phút (khởi động
cache rỗng — một blip không được để nguồn trắng luật 24h); nguồn hỏng HOẶC trả
0 instrument giữ số liệu hôm qua + bị nêu tên trong lỗi (Bybit/OKX gói lỗi
trong HTTP 200 + `retCode`/`code` — parser đọc và báo, không để lỗi sàn giả
dạng "không niêm yết"); refresh thành công thay TRỌN map của nguồn đó (market
bị gỡ biến mất thay vì sống mãi); 404 của endpoint theo-symbol = "sàn không
niêm yết", không phải sự cố nguồn. Kiểm ngày cũng chính là watch delist
(§7.10). MinQty sàn không công bố để 0 = "không nêu" — tầng sizing tự giữ sàn
tối-thiểu-một-bước, không bịa số liệu sàn. Golden test trên response thật ghi
ở `exchanges/testdata/instruments_*.json` (`CAPTURE_TESTDATA=1` để ghi lại).
Chạy thật 2026-09-03: 36 instruments × 9 nguồn, đủ 4 cặp.

**Nguồn contract size theo sàn — SỐ ĐO THẬT 2026-09-03:**

| Sàn | Đơn vị đặt lệnh | Field | Giá trị đo (BTC) |
|---|---|---|---|
| Binance | Coin | — (`IsContract = false`) | step 0.001 BTC (fut) / 0.00001 (spot) |
| Bybit linear | Coin | — (`IsContract = false`) | qtyStep 0.001 BTC |
| OKX | **Contract** (lẻ được, lotSz 0.01 ct) | `ctVal` × `ctMult`, `ctValCcy` = base khi `ctType=linear` | 0.01 × 1 = **0.01 BTC/ct** → bước 0.0001 BTC |
| Gate | **Contract** (NGUYÊN — order_size là số nguyên) | `quanto_multiplier` (số coin mỗi contract) | **0.0001 BTC/ct** → bước 0.0001 BTC |
| Kraken | **Contract** — nhưng 1 contract = **1 đơn vị base** | `contractSize` + bước `10^-contractValueTradePrecision` | ctSize 1 BTC, precision 4 → bước 0.0001 BTC; ⚠️ **PF_XRPUSD precision 0 → XRP NGUYÊN từng con** |
| Hyperliquid | Coin | `szDecimals` (bước `10^-szDecimals`) | BTC 5 → 0.00001; ⚠️ **XRP szDecimals 0 → XRP nguyên** |
| Paradex | Coin | `order_size_increment` | 0.00001 BTC |

> **✅ Mâu thuẫn Kraken từ Bước 1.2 đã giải (2026-09-03).** Khảo sát ghi
> "Kraken đặt theo contract" và phép đo 1.2 thấy khối lượng sổ *trông như
> coin* (PF_XBTUSD 0.0929 khi BTC ≈ $77,5k) — **cả hai cùng đúng**: PF_ đặt
> theo contract nhưng `contractSize = 1` đơn vị base, nên số contract trùng
> số coin về mặt con số. Endpoint `/derivatives/api/v3/instruments` là trọng
> tài. Hệ quả cho khối lượng đỉnh sổ (nợ 1.2): quy đổi contract→coin giờ chỉ
> là `× ContractSizeCoin` từ registry — nối vào pipeline sổ lệnh khi 2.7 dùng
> số này xếp hạng thanh khoản.

**Bẫy delta:** `stepSize` của spot và perp khác nhau. Làm tròn khối lượng 2 chân theo 2 bộ quy tắc mà không xử lý → delta ≠ 0 ngay từ lệnh đầu tiên, và sai số đó tồn tại suốt vòng đời vị thế.

Cách xử lý (đã thành `instruments.SizeDeltaNeutral`, test đủ 9 nguồn trên số
liệu thật): tính size mục tiêu neo theo giá spot → làm tròn **xuống** theo
`StepSizeCoin` **lớn hơn** của hai phía → kiểm size nằm trên lưới của **cả
hai** chân (step không chia hết nhau → từ chối) → kiểm `MinQtyCoin`,
`MaxQtyCoin` và `MinNotionalQuote` của **cả hai** → mới trả size; mọi từ chối
nêu rõ chân nào, luật nào, số nào.

---

## 6. BẢNG ÁNH XẠ SPOT ↔ PERP — ✅ xây ở Bước 2.4 (2026-09-03)

> Bản đặc tả cũ ở đây viết "repo dùng `switch` hardcode trong từng connector"
> — tiền đề đó đã chết từ Bước 1.4 (ánh xạ ký hiệu nằm trong `config.yaml`,
> bug `symbol[:3]` chết cùng lúc). Cái Bước 2.4 thật sự phải xây là tầng
> XÁC THỰC: chứng minh hai chân cùng base, cùng quote bằng dữ liệu **sàn tự
> khai**, và từ chối có tên mọi thứ không chứng minh được.

**Như đã xây** — [internal/instruments/mapping.go](../internal/instruments/mapping.go),
hàm thuần `BuildHedgeMapping(instruments, pairs, claims)`; `cmd/scanner` dựng
lại sau mỗi lần registry refresh và log khi bảng ĐỔI:

- `PairAssets{Symbol, BaseAsset}` — config khai symbol chuẩn nghĩa là base
  nào. **Cố ý không có quote**: BTCUSDT trên kraken_futures trỏ tới chợ quote
  USD là chủ đích; quote phải khớp là quote của NGUỒN (`SourceClaim`), không
  phải hậu tố symbol.
- `SourceClaim{Source, MarketType, QuoteAsset, Tradable}` — config khai gì về
  nguồn thì sàn phải xác nhận nấy.
- Xác thực **hai chiều**: (a) config → sàn: base sàn khai phải trùng base
  config gán cho symbol (R11 — ghép lệch coin), quote sàn khai phải trùng
  `quote_asset` của nguồn, market type phải trùng; (b) sàn → config: một chợ
  native chỉ được đúng MỘT symbol chuẩn nhận, và chân nào không tìm được đối
  tác (perp USD giữa toàn spot USDT) bị nêu tên trong `Rejections` — cả hai
  hướng perp-tìm-spot lẫn spot-tìm-perp.
- Sàn không niêm yết cặp → instrument **vắng mặt** trong registry → tự loại,
  không phải rejection.
- Chỉ instrument `status = "trading"` được ghép; `Rejections` là output hạng
  nhất, mỗi cái mang lý do đầy đủ.

**Base/quote lấy từ đâu — SÀN TỰ KHAI, không bao giờ cắt chuỗi symbol:**

| Sàn | Field base/quote | Ghi chú |
|---|---|---|
| Binance (fut+spot) | `baseAsset` / `quoteAsset` | tường minh |
| Bybit (fut+spot) | `baseCoin` / `quoteCoin` | tường minh |
| OKX | `ctValCcy` / `settleCcy` | ⚠️ với SWAP, `baseCcy`/`quoteCcy` RỖNG (đó là field spot); linear swap: ctValCcy = base, settleCcy = quote. Đo 2026-09-03 |
| Gate | tách `name` tại `_` | sàn không có field riêng; tên hợp đồng `{base}_{quote}` là cấu trúc sàn khai, khác về bản chất với cắt cứng độ dài |
| Kraken | `base` / `quote` | **sàn tự khai `base: "BTC"` cho PF_XBTUSD** → toàn codebase không cần bảng alias XBT nào |
| Hyperliquid | `name` / hằng `"USD"` | meta không có quote; "mọi perp quote bằng USD" là chính sách toàn sàn có tài liệu (trang contract specifications), cùng kiểu hằng $10 min notional |
| Paradex | `base_currency` / `quote_currency` | lưu ý `settlement_currency` là USDC — GĐ 4 quan tâm, còn ghép cặp so QUOTE |

⚠️ **Giữ NGUYÊN VĂN chữ hoa/thường của sàn, chuẩn hoá ở chỗ SO SÁNH.** Bản
đầu của bước này `strings.ToUpper` tại từng fetcher; review bắt được là sai:
recording Hyperliquid có **7 thị trường chữ lẫn** — `kPEPE`, `kSHIB`, `kBONK`,
`kLUNC`, `kFLOKI`, `kDOGS`, `kNEIRO` — mà tiền tố `k` nghĩa là **1000×**, nên
viết hoa thành "KPEPE" là bịa ra một tài sản sàn chưa từng công bố. Đồng thời
`config.yaml` cũng không thể viết hoa `base:` vì với `symbol_format: "{base}"`
nó CHÍNH LÀ định danh sàn (`internal/config` chuẩn hoá `quote` nhưng không
chuẩn hoá `base`). Lời giải: cả hai phía giữ nguyên văn, `sameAsset`
(`strings.EqualFold`) trong `mapping.go` sở hữu luật hoa-thường. Nhờ vậy
`kPEPE` ghép được với chính nó nhưng **không** ghép với `PEPE` — khác mệnh giá
1000 lần.

**Ba hình dạng "không niêm yết" trên endpoint theo-symbol (đo 2026-09-03,
đều đã xử lý thành "vắng mặt" — một cặp không hỗ trợ KHÔNG được giết cả
nguồn):**

| Sàn | Hình dạng | Xử lý |
|---|---|---|
| Paradex | HTTP **404** | `errInstrumentNotListed` → bỏ qua symbol đó (trước đây làm hỏng cả nguồn — nghiệm thu 2.4 bắt được khi thêm XLMUSDT) |
| OKX | HTTP 200 + `code 51001` | riêng 51001 = vắng mặt; mọi code khác vẫn là lỗi to |
| Bybit | linear: HTTP 200 + `retCode 10001` "symbol invalid" · spot: `retCode 0` + list rỗng | chỉ 10001 **kèm** dấu hiệu symbol = vắng mặt; 10001 khác (category sai…) vẫn là lỗi |

`retMsg`/`msg` là **văn xuôi cho người đọc, không phải hợp đồng** — Bybit viết
"Symbol Is Invalid" ở endpoint v5 khác — nên phép khớp gấp chữ hoa-thường và
tìm hai từ riêng lẻ; sai lệch câu chữ vẫn rơi về nhánh **lỗi to**, hướng an
toàn. Cả bốn fetcher theo-symbol (Bybit, OKX, Gate, Paradex) đều tôn trọng
sentinel 404 dù hôm nay chỉ Paradex trả 404: sàn đổi hình dạng thì cái giá là
một instrument vắng mặt, không phải cả nguồn trắng.

**Vắng mặt phải NHÌN THẤY.** Vì "không niêm yết" giờ im lặng ở mọi tầng dưới,
`Registry.Refresh` — tầng duy nhất biết đã HỎI gì — nêu tên symbol không quay
về: `paradex_futures does not list XLMUSDT — absent from the hedge mapping
(check symbol_map if the venue does list it)`. Đó là thứ làm một lỗi gõ nhầm
`symbol_map` hiện hình thay vì lặng lẽ mất cặp. Và khi một nguồn trả về 0
instrument, thông điệp nêu **cả hai** cách đọc (sàn trả rỗng ↔ sàn không niêm
yết cặp nào trong danh sách, tức nguồn không nên nằm trong config).

**Nghiệm thu đã chạy sống 2026-09-03** (cổng 8085, config thêm cặp mới):
thêm `DOGEUSDT` → 8 hedge pairs (2 spot × 4 perp USDT) tự dựng, 3 perp USD bị
từ chối đúng lý do; thêm `XLMUSDT` (Paradex không niêm yết — catalog 62 perp)
→ 8 pairs, Kraken/Hyperliquid bị từ chối vì quote USD, Paradex **tự loại**
bằng vắng mặt: 53 instrument = 6 cặp × 9 nguồn − 1.

**Quy ước symbol per-sàn** (nằm ở `config.yaml` `symbol_format`/`symbol_map`
từ Bước 1.4, giữ đây để tra nhanh):

| Sàn | Định dạng | Ví dụ BTC |
|---|---|---|
| Binance | `BTCUSDT` | `BTCUSDT` |
| Bybit | `BTCUSDT` | `BTCUSDT` |
| OKX | `BASE-QUOTE-SWAP` | `BTC-USDT-SWAP` |
| Gate | `BASE_QUOTE` | `BTC_USDT` |
| Kraken | `PF_<XBT>USD` | `PF_XBTUSD` — BTC gọi là XBT trong TÊN chợ (nhưng field `base` vẫn khai "BTC"), quote là USD không phải USDT |
| Hyperliquid | chỉ base coin | `BTC` |
| Paradex | `BASE-USD-PERP` | `BTC-USD-PERP` |

---

## 7. CÁC BẪY DỮ LIỆU

Xếp theo mức tốn kém:

**1. Ghép cặp spot ↔ perp sai** → mở vị thế lệch coin, không phải hedge mà là hai vị thế đầu cơ. Xem §6.

**2. Làm tròn 2 chân theo 2 `stepSize` khác nhau** → delta ≠ 0 từ lệnh đầu. Xem §5.

**3. Funding là sự kiện RỜI RẠC, không phải dòng chảy liên tục.** Chỉ trả/nhận nếu **đang giữ vị thế đúng timestamp settle**. Giữ 7h59 rồi đóng = nhận 0đ. Mọi công thức `APY × số ngày giữ` đều sai — phải **đếm số mốc settle đã đi qua**. (Ngoại lệ: Paradex tích luỹ liên tục.)

**4. Funding tính trên `mark price × size` tại thời điểm settle**, không phải giá vào lệnh. Số thực nhận luôn lệch dự tính. **Log cả hai để so** — không log thì không biết lệch do mô hình sai hay do sàn tính khác.

**5. Chu kỳ funding không đồng nhất** — Hyperliquid 1h, Kraken 1h (rate mỗi-1h, §3.2②), Binance 8h/4h/1h theo symbol (§3.3), Paradex liên tục. Hardcode `× 3 × 365` sai từ 1.5× đến 8×.

**6. Dấu và đơn vị của rate khác nhau giữa các sàn.** Kraken `funding_rate` là tuyệt đối. Verify từng sàn bằng `cmd/fundingcheck` (hai endpoint độc lập mỗi sàn + kiểm biên độ chéo sàn — xem phụ lục), đối chiếu thêm web sàn khi nhìn được bằng mắt.

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
| Binance `fundingRate`, `fundingInfo`, `markPrice` fields | ✅ Docs chính thức + đo 2026-09-03 (spacing settle khớp `fundingInfo`) |
| Bybit `fundingInterval` (phút), snapshot+delta, lotSizeFilter | ✅ Docs chính thức + đo 2026-09-03 (480 phút = `fundingIntervalHour` 8h) |
| Kraken settle mỗi 1h, `relative_funding_rate` là rate mỗi-1h | ✅ Spec hợp đồng + đo 2026-09-03 — **bản cũ ghi "realize 8h, chia 8" là SAI**, xem §3.2② |
| Kraken WS `next_funding_rate_time` là epoch ms tuyệt đối | ✅ Hai probe WS độc lập 2026-09-03 — **bản cũ ghi "ms còn lại" là SAI** (chép theo mô tả field của docs sàn, mâu thuẫn với chính sample của nó), xem §3.3⑥ |
| Hyperliquid funding 1h, field API đã ÷8 | ✅ Sàn tự khai interval qua `predictedFundings` (`fundingIntervalHours: 1`, đo 2026-09-03) + docs; vế "đã ÷8" canh gác bởi kiểm biên độ ≥5× |
| OKX `fundingTime` vs `nextFundingTime` | ✅ Đo 2026-09-03: `ts < fundingTime < nextFundingTime`, cách nhau đúng 8h |
| Gate `funding_interval` = 28800 giây | ✅ Đo 2026-09-03: 28800, `funding_next_apply` epoch giây tròn giờ |
| Paradex Funding V2 liên tục | ✅ Đo 2026-09-03: điểm index mỗi ~5s, `funding_period_hours: 8` tường minh |
| Kraken `funding_rate` tuyệt đối vs `relative_` | ✅ Đo 2026-09-03: absolute ÷ relative ≈ index price (lệch 0,09%) |
| Kraken tích luỹ pro-rata trong giờ? (câu chữ spec, §3.2②) | 🟡 **Mới** — chỉ xác minh được bằng funding payment thật → GĐ 4 (`income`) |
| Binance `fundingInfo` phủ 100% perp TRADING | 🟡 Sự kiện đo được 2026-09-03, docs không cam kết — pattern mặc-định-rồi-ghi-đè vẫn bắt buộc |

**Cách verify rẻ nhất** (đã thành `cmd/fundingcheck`, Bước 2.1): đọc funding rate
BTC từ cả 7 sàn song song, in raw cạnh chuẩn hoá, chấm verdict tự động. Đối chiếu
web sàn bằng mắt không tự động hoá được (mọi trang venue là JS app) — thay bằng
2 endpoint độc lập mỗi sàn + docs nêu field web hiển thị (spec Kraken nói thẳng
relative rate là số web hiển thị) + kiểm biên độ chéo sàn: |rate/8h| của mỗi sàn
phải nằm trong **5×** trung vị — bắt được lệch ÷8/×8, 60× (phút↔giây),
absolute-vs-relative (~10⁵×), chấp nhận funding âm/lẫn dấu, và tự kiềm khi cả
cụm sát 0 (không phân định được thì nói rõ, không FAIL bừa).

Chạy lại bất cứ lúc nào: `go run ./cmd/fundingcheck` — exit ≠ 0 khi có verdict
FAIL. Verdict chỉ khẳng định **đơn vị/ngữ nghĩa**, không ghim giá trị cấu hình,
nên sàn đổi interval hay cap vẫn PASS; một FAIL nghĩa là "sàn đã đổi ngữ nghĩa
field, hoặc regime thị trường vượt khả năng phân định của kiểm" — đi xem lại,
đừng mặc định code sai và cũng đừng lờ đi.
