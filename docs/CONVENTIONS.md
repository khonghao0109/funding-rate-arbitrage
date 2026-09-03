# QUY ƯỚC MÃ NGUỒN

> **Cập nhật:** 2026-08-28 · **Áp dụng cho:** toàn bộ code Go và JavaScript trong repo
> **Nguyên tắc bao trùm:** code đọc như code xung quanh nó. Khi quy ước ở đây mâu thuẫn với `gofmt` hoặc Effective Go, `gofmt` và Effective Go thắng.

---

## 0. NGÔN NGỮ

| Nội dung | Ngôn ngữ |
|---|---|
| Tên package, file, hàm, biến, hằng, struct field | **Tiếng Anh** — không ngoại lệ |
| Doc comment, comment trong code | **Tiếng Anh** |
| Commit message, tên branch, PR title | **Tiếng Anh** |
| Tài liệu trong `docs/`, thảo luận | Tiếng Việt |
| Chuỗi hiển thị cho người dùng (UI) | Tiếng Việt hoặc song ngữ |

Lý do tách bạch: code là thứ công cụ tự động (linter, AI, thư viện) đọc và là thứ tồn tại lâu nhất; tài liệu là thứ người trong nhóm đọc.

---

## 1. QUY TẮC QUAN TRỌNG NHẤT CỦA DỰ ÁN NÀY: HẬU TỐ ĐƠN VỊ

Khảo sát 7 sàn ([DATA-REQUIREMENTS.md §3](DATA-REQUIREMENTS.md#3-khảo-sát-funding-rate-7-sàn)) cho thấy cùng một khái niệm được ba sàn trả về bằng ba đơn vị khác nhau (giờ / phút / giây), và một sàn trả giá trị tuyệt đối trong khi các sàn khác trả tỷ lệ. Đây là loại bug **không crash, không báo lỗi, chỉ ra số sai**.

**Quy tắc: mọi biến số mang đơn vị phải nói ra đơn vị trong chính tên của nó.**

### 1.1. Thời gian

| ✅ Đúng | ❌ Sai | Ghi chú |
|---|---|---|
| `IntervalSec int64` | `Interval` | Luôn chuẩn hoá về **giây** ở tầng connector |
| `TimeoutMs int64` | `Timeout` | Trừ khi kiểu là `time.Duration` |
| `staleThreshold time.Duration` | `staleSec time.Duration` | Kiểu `time.Duration` **không** cần hậu tố — kiểu đã nói lên đơn vị |
| `NextFundingAtMs int64` | `NextFundingTime` | Mốc tuyệt đối trên wire: hậu tố `AtMs` |
| `recvAt time.Time` | `recvTime` | Nội bộ dùng `time.Time`, không hậu tố |

**Ranh giới:** `int64` mili-giây chỉ tồn tại ở tầng wire (JSON của sàn, JSON gửi frontend). Vào trong hệ thống, chuyển sang `time.Time` và `time.Duration` ngay.

### 1.2. Tỷ lệ — phần trăm hay phân số?

Đây là nguồn bug số hai. Funding rate từ sàn về là **phân số** (`0.0001`), còn code hiện tại hiển thị **phần trăm** (`ProfitPct = x * 100`).

| Hậu tố | Ý nghĩa | Ví dụ giá trị |
|---|---|---|
| `...Frac` | Phân số, 0.0001 = 0,01% | `fundingRateFrac = 0.0001` |
| `...Pct` | Phần trăm, 0.01 = 0,01% | `profitPct = 0.05` |
| `...Bps` | Điểm cơ bản, 1 = 0,01% | `takerFeeBps = 4` |

**Không bao giờ** để một biến tên là `rate` trần trụi đi qua nhiều hàm. Trong dự án này, phí niêm yết nên dùng `Bps` (số nguyên, không sai số dấu phẩy động), rate từ sàn dùng `Frac`, chỉ đổi sang `Pct` ở tầng hiển thị.

### 1.3. Rate có chu kỳ

Funding rate vô nghĩa nếu không kèm chu kỳ. Tên phải nói ra chu kỳ:

```go
RatePerIntervalFrac float64 // đúng 1 chu kỳ settle của sàn đó
RatePer8hFrac       float64 // quy về 8h — đơn vị so sánh chéo sàn
APRFrac             float64 // annualized
```

❌ `FundingRate float64` — chu kỳ nào? Hyperliquid 1h hay Binance 8h? Sai 8 lần.

### 1.4. Tiền và khối lượng

| ✅ Đúng | Ý nghĩa |
|---|---|
| `NotionalUSD float64` | Giá trị danh nghĩa quy USD |
| `QtyCoin float64` | Khối lượng tính theo coin |
| `QtyContracts float64` | Khối lượng tính theo contract (OKX, Gate, Kraken) |

❌ `size`, `amount`, `qty` trần trụi — đặc biệt nguy hiểm vì OKX/Gate/Kraken đặt lệnh theo contract còn Binance/Bybit theo coin.

---

## 2. TỪ ĐIỂN THUẬT NGỮ

Một khái niệm — một từ. Dùng đúng từ này trong toàn bộ codebase, kể cả tên biến cục bộ.

| Từ chuẩn | Nghĩa | Không dùng |
|---|---|---|
| `Venue` | Định danh sàn: `binance`, `bybit`, `okx` | `exchange`, `platform`, `cex` |
| `MarketType` | `spot` \| `perp` \| `future` \| `oracle` | `type`, `kind`, `category` |
| `Symbol` | Ký hiệu **đã chuẩn hoá**: `BTCUSDT` | `pair`, `ticker`, `instrument` |
| `NativeSymbol` | Ký hiệu **theo sàn**: `BTC-USDT-SWAP`, `PF_XBTUSD` | `exchangeSymbol`, `rawSymbol` |
| `Source` | Chuỗi wire ghép: `binance_futures` | — (giữ cho tương thích frontend) |
| `Leg` | Một chân của vị thế: `spotLeg`, `perpLeg` | `side1`, `orderA` |
| `Side` | `buy` \| `sell` | `direction`, `action` |
| `Notional` | Giá trị danh nghĩa | `value`, `size` |
| `Basis` | `perpPrice - spotPrice` | `diff`, `gap` |
| `Spread` | Chênh lệch giá giữa hai **venue** | `difference` |
| `Settlement` | Thời điểm funding được chốt | `payout`, `charge` |
| `Stale` | Dữ liệu quá cũ để dùng | `old`, `expired` |
| `Registry` | Kho tra cứu instrument | `catalog`, `db`, `cache` |

`Source` là ngoại lệ có chủ đích: nó đã nằm trong hợp đồng JSON với frontend. Quy ước là **`Source = Venue + "_" + MarketType`**, và code mới phải mang cả `Venue` lẫn `MarketType` dưới dạng field riêng thay vì tách chuỗi.

---

## 3. PACKAGE

- Một từ, viết thường, không gạch dưới, không viết hoa: `instruments`, `broker`, `backtest`.
- Tên package là một phần của tên định danh khi gọi từ ngoài → **tránh lặp (stutter)**:

```go
✅ instruments.Registry      ❌ instruments.InstrumentRegistry
✅ fees.Table                ❌ fees.FeeTable
✅ store.Open()              ❌ store.OpenStore()
```

- Mỗi package có `doc.go` mở đầu bằng `// Package <tên> ...`, nêu **trách nhiệm** và **ranh giới**, không kể lể cách dùng.
- `internal/` cho mọi thứ không phải API công khai — hiện tại là tất cả.

---

## 4. FILE

| Loại | Quy tắc | Ví dụ |
|---|---|---|
| Thường | `snake_case.go`, viết thường | `funding_binance.go` |
| Test | Cùng tên + `_test.go` | `funding_binance_test.go` |
| Doc package | `doc.go` | `internal/risk/doc.go` |

**Tổ chức trong `exchanges/`:** một sàn — một file cho dữ liệu giá, một file riêng cho funding.

```
exchanges/
├── types.go              # kiểu dùng chung, Feeds
├── binance.go            # bookTicker + aggTrade (spot & futures)
├── binance_funding.go    # markPrice stream + fundingInfo
├── okx.go
├── okx_funding.go
└── ...
```

Lý do tách: logic funding của mỗi sàn có bẫy riêng (đơn vị, ngữ nghĩa field, snapshot/delta) và cần test riêng. Nhét chung vào file 250 dòng sẵn có sẽ khiến không ai đọc lại.

---

## 5. ĐỊNH DANH

### 5.1. Cơ bản

| Loại | Quy tắc | Ví dụ |
|---|---|---|
| Exported | `PascalCase` | `FundingData`, `NetAPRFrac` |
| Unexported | `camelCase` | `staleThreshold`, `perpLeg` |
| Hằng | `PascalCase` (không `SCREAMING_CASE`) | `FundingDiscrete`, `DefaultIntervalSec` |
| Interface | Hậu tố `-er` khi là một hành vi | `Notifier`, `Broker`, `PriceSource` |
| Sentinel error | Tiền tố `Err` | `ErrSymbolNotMapped`, `ErrStaleData` |
| Error type | Hậu tố `Error` | `ValidationError` |

### 5.2. Viết tắt giữ nguyên hoa

Đây là quy tắc Go, không phải sở thích:

```go
✅ APIKey, HTTPClient, WSURL, ID, URL, APRFrac, USDNotional
❌ ApiKey, HttpClient, WsUrl, Id, Url, AprFrac
```

Khi viết tắt đứng đầu định danh unexported thì viết thường toàn bộ: `apiKey`, `wsURL`.

### 5.3. Receiver

Ngắn (1–3 ký tự), nhất quán trong toàn bộ type, không dùng `this` / `self`:

```go
✅ func (r *Registry) Lookup(...)     func (s *Scanner) checkArbitrage(...)
❌ func (registry *Registry) ...      func (this *Scanner) ...
```

### 5.4. Biến cục bộ

- Phạm vi càng ngắn, tên càng ngắn được phép: `i`, `err`, `ok`, `n` trong vòng lặp là chuẩn.
- Phạm vi càng dài, tên càng phải đầy đủ. Biến sống qua 30 dòng thì không được đặt là `d`.
- **Không viết tắt tuỳ hứng:** `fundingRate` chứ không phải `fndRt`, `instrument` chứ không phải `inst`.
- Boolean đọc như một mệnh đề: `isStale`, `hasFundingCap`, `canHedge` — không phải `stale`, `flag`, `check`.

---

## 6. HÀM

### 6.1. Tiền tố theo hành vi

Đây là nơi hay tuỳ tiện nhất. Quy ước cố định:

| Tiền tố | Dùng khi | Ví dụ |
|---|---|---|
| `New...` | Constructor, trả về giá trị mới | `NewRegistry()` |
| `Open...` | Constructor có tài nguyên cần đóng | `store.Open()` |
| `Fetch...` | Gọi **mạng**, có thể chậm, có thể lỗi | `FetchInstruments(ctx)` |
| `Load...` | Đọc từ **đĩa / config** | `LoadConfig()` |
| `Connect...` | Vòng lặp WebSocket dài hạn, tự reconnect | `ConnectBinanceFutures()` |
| `Parse...` | Chuỗi / bytes → struct, thuần tuý | `ParseFundingPayload()` |
| `Normalize...` | Dạng của sàn → dạng chuẩn | `NormalizeFundingRate()` |
| `Calc...` | Tính toán thuần tuý, không side effect | `CalcNetAPR()` |
| `Must...` | Panic khi lỗi — **chỉ dùng lúc khởi động** | `MustLoadConfig()` |

**Không dùng tiền tố `Get` cho accessor** (quy ước Go): `cfg.Port()` chứ không phải `cfg.GetPort()`. `Get` chỉ chấp nhận được khi nó ánh xạ 1-1 với một HTTP verb của sàn, và ngay cả khi đó `Fetch` vẫn được ưu tiên.

### 6.2. Chữ ký

- `ctx context.Context` **luôn là tham số đầu tiên** với mọi hàm có I/O.
- `error` **luôn là giá trị trả về cuối cùng**.
- Quá 4 tham số → gom thành struct. Đây là lý do `Feeds` thay cho 3 channel rời (xem [PLAN.md](PLAN.md) Bước 2.2).
- Trả về nhiều hơn 3 giá trị → gom thành struct kết quả có tên.

---

## 7. STRUCT

- Nhóm field theo chủ đề, ngăn bằng dòng trống + comment nhóm — xem `FundingData` trong [DATA-REQUIREMENTS.md §4.2](DATA-REQUIREMENTS.md#42-struct-đề-xuất).
- Field boolean đi kèm field tuỳ chọn phải đặt cạnh nhau: `FollowingRateFrac` + `HasFollowingRate`.
- Struct tag JSON dùng `snake_case` để khớp hợp đồng frontend hiện có:

```go
ProfitPct float64 `json:"profit_pct"`
```

- **Không nhúng con trỏ mutex vào struct dữ liệu.** Dữ liệu đi qua channel phải là giá trị bất biến, sao chép được.

---

## 8. LỖI

```go
// Sentinel — so sánh bằng errors.Is
var ErrSymbolNotMapped = errors.New("symbol has no spot counterpart")

// Bọc lỗi, luôn dùng %w, thêm ngữ cảnh không lặp lại lỗi gốc
if err != nil {
    return fmt.Errorf("fetch instruments for %s: %w", venue, err)
}
```

- Thông điệp lỗi: **chữ thường, không dấu chấm cuối, không "failed to"** (`fetch instruments: ...` chứ không phải `Failed to fetch instruments.`).
- **Không `panic` trong code thư viện.** Chỉ được panic ở giai đoạn khởi động khi cấu hình sai đến mức không thể chạy.
- Connector không bao giờ được làm sập tiến trình: log, đóng kết nối, backoff, thử lại.
- **Không bao giờ nuốt lỗi im lặng.** Code hiện tại có nhiều chỗ `continue` sau khi parse lỗi — chấp nhận được với message rác, nhưng phải có counter để biết tỷ lệ rớt.

---

## 9. ĐỒNG THỜI

- **Channel để truyền dữ liệu, mutex để bảo vệ trạng thái chia sẻ.** Không trộn hai vai trò.
- Mọi goroutine phải có **điều kiện thoát được ghi rõ**. Goroutine `for {}` vô hạn cần `ctx` hoặc channel dừng.
- Giữ lock ở phạm vi hẹp nhất; **không gọi hàm có I/O khi đang giữ lock**.
- Tên mutex nói rõ nó bảo vệ cái gì: `pricesMu` bảo vệ `prices` — quy ước Go dùng hậu tố `Mu`, không phải `Mutex`.
- Mỗi lần thêm goroutine mới, chạy `go test -race`.

---

## 10. COMMENT

- Mọi định danh **exported** phải có doc comment, **bắt đầu bằng chính tên đó**:

```go
// NormalizeFundingRate converts a venue's raw funding value into the canonical
// per-8h fraction used across the system.
func NormalizeFundingRate(...)
```

- Comment giải thích **tại sao**, không phải **cái gì**. Code đã nói cái gì rồi.
- Mỗi bẫy đã biết của sàn phải có comment tại đúng chỗ xử lý nó, kèm liên kết tài liệu:

```go
// Kraken quotes funding_rate as an absolute price amount (e.g. -6.26e-11),
// not a rate. relative_funding_rate is the comparable figure.
// See docs/DATA-REQUIREMENTS.md §3.2.
```

- **Không comment lịch sử** (`// đã sửa ngày 28/8`) — đó là việc của git.

---

## 11. TEST

| Loại | Tên file | Tên hàm |
|---|---|---|
| Unit | `x_test.go` | `TestNormalizeFundingRate_KrakenUsesRelative` |
| Table-driven | như trên | tên case viết thường, mô tả điều kiện |
| Golden | `testdata/binance_markprice.json` | nạp payload thật của sàn |

- Mẫu tên: `Test<Hàm>_<TìnhHuống>`.
- **Mọi connector phải có golden test** nạp payload JSON thật đã lưu — đây là lưới an toàn duy nhất khi sàn đổi định dạng.
- **Mọi hàm tính toán tài chính phải có unit test trước khi dùng vốn thật.** Không thương lượng.
- Dữ liệu mẫu đặt trong `testdata/` (Go bỏ qua thư mục này khi build).

---

## 12. CẤU TRÚC THƯ MỤC

```
crypto-futures-arbitrage-scanner/
├── cmd/
│   └── scanner/               # entrypoint — chỉ wiring, không logic
├── config.yaml                # [GĐ 1.4]
├── docs/
│   ├── PLAN.md                # lộ trình 9 giai đoạn
│   ├── WORKFLOW.md            # quy trình code & review
│   ├── DATA-REQUIREMENTS.md   # dữ liệu cần từ sàn
│   └── CONVENTIONS.md         # file này
├── exchanges/                 # ĐỌC-ONLY, dữ liệu công khai, KHÔNG credential
│   ├── types.go
│   ├── <venue>.go
│   └── <venue>_funding.go
├── internal/
│   ├── scanner/               # engine + tầng wire — một package vì cùng thi hành WS-CONTRACT.md
│   ├── instruments/           # registry + ánh xạ spot↔perp + sizing
│   ├── fees/                  # bảng phí, lợi nhuận ròng
│   ├── store/                 # SQLite
│   ├── strategy/              # APR, tín hiệu vào/ra
│   ├── backtest/              # replay lịch sử
│   ├── notify/                # Telegram/Discord
│   ├── broker/                # ⚠️ CREDENTIAL — REST có ký
│   ├── execution/             # vị thế delta-neutral
│   └── risk/                  # margin, kill switch, giới hạn
└── static/                    # dashboard
```

### 12.1. Luật phụ thuộc (bắt buộc, kiểm tra khi review)

```
exchanges/  ──►  KHÔNG được import bất kỳ internal/ nào
internal/broker/  ──►  KHÔNG được import từ exchanges/ hay luồng ingest
strategy, backtest  ──►  chỉ nhận giá trị ĐÃ CHUẨN HOÁ, không thấy payload thô
```

Luật thứ hai là ranh giới an toàn của cả dự án: dữ liệu công khai và credential nằm hai phía khác nhau. Vi phạm luật này là lỗi chặn merge, không phải góp ý.

### 12.2. `cmd/` — đã chuyển (2026-09-03)

Toàn bộ code gốc repo đã dời: entrypoint về `cmd/scanner/main.go` (chỉ wiring — godotenv, nối connector, HTTP server), engine về `internal/scanner/` (state giá, staleness, tầng wire, cùng toàn bộ test).

`wire.go` và phần scanner ở **cùng một package** có chủ đích: chúng phụ thuộc hai chiều (`wire` đọc `PricePoint`; scanner gọi các builder `newWire*`) và cùng thi hành một hợp đồng ([WS-CONTRACT.md](WS-CONTRACT.md)). Tách hai package nghĩa là thiết kế lại API giữa chúng — chỉ làm khi có lý do thật, không làm để "trông sạch".

Tên trong package tránh lặp: `scanner.Scanner`, `scanner.New()` — không phải `scanner.FuturesScanner` (§3).

Binary mới thêm vào `cmd/<tên>/main.go` — Bước 2.1 sẽ thêm `cmd/fundingcheck`. Chạy từ gốc repo để `./static/` resolve đúng: `go run ./cmd/scanner`.

---

## 13. THI HÀNH

Trước mỗi commit:

```bash
gofmt -l .        # phải không in ra gì
go vet ./...      # phải sạch
go test ./...     # phải xanh
go test -race ./... # khi có thay đổi liên quan goroutine
```

Khuyến nghị thêm `golangci-lint` với các linter: `errcheck`, `govet`, `staticcheck`, `revive`, `ineffassign`, `unconvert`.

**Commit message:** tiếng Anh, thức mệnh lệnh, dòng đầu ≤ 72 ký tự. Định dạng đầy đủ và quy tắc *một bước = một commit* ở [WORKFLOW.md §P8](WORKFLOW.md#p8--sync--commit).

```
✅ add funding rate stream for bybit
✅ fix hyperliquid symbol truncation for 4-char bases
❌ Added funding.       ❌ update code       ❌ sửa bug funding
```

---

## 14. NỢ QUY ƯỚC TRONG CODE HIỆN TẠI

Ghi nhận, không cần sửa ngay — xử lý dần theo giai đoạn tương ứng:

| Vấn đề | Vị trí | Xử lý ở |
|---|---|---|
| `convertToOKXSymbol` v.v. hardcode `switch` trong từng connector | `okx.go`, `gate.go`, `kraken.go`, `paradex.go` | GĐ 2.4 → `instruments` |
| `coin := symbol[:3]` cắt cứng 3 ký tự | `hyperliquid.go:58` | GĐ 2.4 (bug) |
| ~~`ProfitPct` không nói rõ Pct hay Frac~~ — ✅ trả xong ở GĐ 1.0: trên wire là `spread_gross_pct` / `spread_after_fees_pct`, FE dùng `minSpreadFilterPct`. Không còn định danh nào gọi số thô là "profit" | ~~`main.go`~~ | ✅ GĐ 1.0 |
| Không có `ctx`, goroutine không có điều kiện thoát | tất cả connector | GĐ 1.5 |
| `PriceData.Timestamp` không phân biệt thời điểm sàn phát và thời điểm nhận | `types.go` | GĐ 1.1 |
| Struct trùng lặp `BinanceFuturesTrade` / `BinanceSpotTrade` giống hệt nhau | `binance.go` | GĐ 1.6 |
