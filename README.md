# ⚡ Crypto Arbitrage Scanner

**Scanner chênh lệch giá crypto real-time** — kết nối trực tiếp tầng WebSocket của 7 sàn giao dịch, chuẩn hoá dữ liệu sổ lệnh, và hiển thị khe hở giá ngay khi nó xuất hiện.

Viết bằng Go và JavaScript thuần. Không framework, không phụ thuộc nặng.

> ### ⚠️ Trạng thái hiện tại: **CÔNG CỤ QUAN SÁT, CHƯA PHẢI BOT GIAO DỊCH**
>
> Dự án hiện **chỉ đọc dữ liệu công khai**. Nó không giữ API key, không đặt lệnh, không lưu trữ gì — mọi con số đều tính từ dữ liệu trong RAM.
>
> Mục tiêu dài hạn là phát triển thành **bot Funding Rate Arbitrage**. Lộ trình đầy đủ: [docs/PLAN.md](docs/PLAN.md).

---

## Mục lục

- [Bối cảnh](#bối-cảnh)
- [Hiện tại làm được gì](#hiện-tại-làm-được-gì)
- [Chưa làm được gì](#chưa-làm-được-gì)
- [Mục tiêu dự án](#mục-tiêu-dự-án)
- [Sàn và cặp hỗ trợ](#sàn-và-cặp-hỗ-trợ)
- [Cách hoạt động](#cách-hoạt-động)
- [Cài đặt và chạy](#cài-đặt-và-chạy)
- [Cấu trúc thư mục](#cấu-trúc-thư-mục)
- [Tài liệu](#tài-liệu)
- [Đóng góp](#đóng-góp)
- [Cảnh báo rủi ro](#cảnh-báo-rủi-ro)

---

## Bối cảnh

Vi cấu trúc thị trường (*market microstructure*) là ngành nghiên cứu cách giá hình thành trong những thị trường phân mảnh và hỗn độn. Một tài sản không bao giờ có **một** mức giá duy nhất. Mỗi sàn có sổ lệnh riêng, dòng lệnh riêng, những đặc tính riêng. Vì thế giá luôn trôi lệch nhau — đôi khi rất nhiều, thường thì chỉ trong vài mili-giây.

Trong crypto, dữ liệu này **không nằm sau các feed chuyên nghiệp đắt tiền**. Bạn hoàn toàn có thể tự nhìn thấy những khe hở đó nếu có đúng công cụ.

Đó là việc dự án này làm: đưa quá trình khám phá giá (*price discovery*) ra trước mắt bạn, theo thời gian thực.

---

## Hiện tại làm được gì

| Tính năng | Mô tả |
|---|---|
| **10 luồng dữ liệu real-time** | 7 sàn futures + 2 sàn spot + 1 oracle, mỗi luồng một goroutine riêng |
| **Ma trận spread tách nhóm** | Ba khối riêng — perpetual quote USDT, perpetual quote USD, spot quote USDT. Hai sàn chỉ được so với nhau khi **cùng loại thị trường và cùng đồng quote** |
| **Cảnh báo cơ hội** | Phát hiện khoảng cách giá bất thường, có throttle chống spam. Chỉ sinh **trong một nhóm**, nên không bao giờ nêu một cặp không thực thi được |
| **Mid-price chuẩn** | `(best bid + best ask) / 2` thay vì giá khớp lệnh cuối |
| **Biểu đồ live** | TradingView Lightweight Charts, nhiều sàn trên cùng một khung |
| **Tự điều chỉnh số thập phân** | Theo từng tài sản và vùng giá |
| **Kết nối bền bỉ** | Backoff luỹ thừa 2s→60s (không quay số dồn dập vào sàn đang chết), read deadline phát hiện socket còn mở nhưng ngừng đẩy dữ liệu, keepalive theo đúng tài liệu từng sàn — đã **đo lại** chứ không đoán. Dashboard hiện `uptime` và **số lần nối lại** của từng sàn |
| **Tắt sạch** | Ctrl-C dừng mọi connector và mọi goroutine nội bộ trước khi thoát — đo được **369µs**, ngân sách 5s |
| **Nói rõ số đang hiển thị là gì** | Dashboard ghi thẳng chi phí nào đã trừ và chưa trừ. Nguồn bị loại khỏi so sánh đều kèm lý do (oracle, dữ liệu cũ, không có nguồn cùng loại để so) thay vì lặng lẽ biến mất |
| **Lọc dữ liệu cũ** | Sàn ngừng gửi quá ngưỡng riêng của nó bị loại khỏi so sánh và hiện nhãn **CŨ** / **MẤT KẾT NỐI**. Ngưỡng đo từ dữ liệu thật, 10–20s tuỳ sàn |
| **Dashboard tự dựng theo máy chủ** | Danh sách nguồn, màu, nhãn và danh sách cặp đến từ message `meta`, không hardcode trong JavaScript |
| **Cấu hình bằng `config.yaml`** | Cặp giao dịch, danh sách sàn, ngưỡng, biểu phí và ánh xạ ký hiệu từng sàn nằm trong một file YAML. Thêm một cặp hoặc một sàn dùng connector sẵn có **không cần sửa Go lẫn JavaScript** |
| **Basis spot ↔ perp** | Chênh lệch spot với perpetual **cùng một sàn** — nguyên liệu của chiến lược funding, tính riêng chứ không trộn vào ma trận chéo sàn |
| **Đối chiếu oracle** | Độ lệch từng sàn so với Pyth, chỉ tham chiếu, không bao giờ sinh cảnh báo. Dòng nào lệch đồng quote đều được gắn nhãn |
| **Khối lượng đỉnh sổ** | Thu ở 5/9 nguồn báo bằng coin. Bốn sàn báo bằng contract để `0` — nghĩa là **chưa biết**, không phải không có thanh khoản |
| **Số đã trừ phí giao dịch** | Trừ phí taker cả bốn lượt khớp (mở và đóng cả hai chân). Gọi đúng là **"đã trừ phí giao dịch"**, KHÔNG phải lợi nhuận ròng — chưa trừ trượt giá và funding. Sàn chưa xác minh được biểu phí thì không có số, không phải miễn phí |
| **Funding rate realtime 7 sàn** | 6 sàn qua WebSocket, Binance qua REST `premiumIndex` (luồng mark-price của sàn này không đẩy gì tới môi trường đo). Mọi rate quy về `RatePer8hFrac` để so sánh cùng đơn vị — không sàn nào bị giả định chu kỳ 8h |
| **Kho lịch sử funding (SQLite)** | `cmd/backfill` nạp rate **đã settle thật** từ 7 sàn; scanner tự bổ sung mỗi giờ. Restart không mất gì — mỗi dòng khoá theo mốc settle nên chạy lại chỉ ghi phần thiếu. Kho **không đều nhau giữa các sàn** và báo cáo nói rõ độ sâu từng sàn |
| **Lấy mẫu giá và ảnh chụp quy tắc giao dịch** | Cross-section top-of-book mỗi 30s và quy tắc giao dịch từng ngày, để backtest sau này không diễn giải dữ liệu cũ bằng luật mới |

---

## Chưa làm được gì

Liệt kê thẳng để không ai hiểu nhầm về năng lực hiện tại:

| Chưa có | Hệ quả |
|---|---|
| **Trượt giá (slippage)** | Số sau phí mới trừ phí giao dịch, **chưa trừ trượt giá** — cần độ sâu sổ lệnh, phải tới GĐ 2. Vẫn chưa dùng để ra quyết định vốn được |
| **Dashboard funding** | Funding đã thu thật (Bước 2.5) nhưng mới chỉ in ra log mỗi phút; bảng trên dashboard là Bước 2.7 |
| **Đặt lệnh** | Không có REST có ký, không có quản lý credential |
| **Test tự động** | 250 test (100% ở `internal/fees`, 96,9% ở `internal/instruments`, 86,5% ở `internal/scanner`, 84,2% ở `internal/store`, 80,6% ở `internal/config`, 76,5% ở `internal/history`, 58,4% ở `exchanges`). `exchanges/testdata/` chứa payload **thật** ghi lại từ 9 sàn; golden test cho chúng chạy qua đúng handler production. Pyth không ghi được (sàn trả 401) nên fixture của nó là tổng hợp và được ghi rõ. Vòng phân trang của 7 fetcher lịch sử funding chỉ chạy được với sàn thật nên không nằm trong phần trăm — parser của chúng thì có |
| **Biểu phí 4/9 sàn** | Bybit (×2), OKX và Gate không đọc được biểu phí từ tài liệu công khai, nên mọi cặp có các sàn đó **không có số sau phí**. Nhập biểu phí tài khoản của bạn vào `config.yaml` và đặt `verified: true` |
| **Quy đổi contract → coin** | OKX, Gate và Kraken báo khối lượng bằng contract; chưa có instrument registry để nhân `ctVal`/`quanto_multiplier`, nên bốn sàn chưa có số thanh khoản. Xếp hạng cơ hội theo độ sâu phải chờ GĐ 2 |

Toàn bộ các mục trên đều đã có kế hoạch xử lý theo giai đoạn trong [docs/PLAN.md](docs/PLAN.md).

> **Đo được ở Bước 1.3:** một vòng mở–đóng cả hai chân tốn khoảng **0,19%** phí taker,
> trong khi chênh lệch chéo sàn ở các cặp lớn chỉ vài phần nghìn phần trăm. Nghĩa là
> **mọi cảnh báo scanner đang phát đều âm sau phí**. Đây không phải lỗi hiển thị — đó
> là câu trả lời thật cho "arbitrage chéo sàn có ăn được không" ở quy mô lẻ. Biên thật
> nằm ở funding rate (5–15% APR), là thứ Giai đoạn 2 đi thu thập.

> Về staleness: ngưỡng hiện đo trên **4 cặp lớn trong giờ hoạt động**. Cặp thanh khoản mỏng hoặc giờ đêm có thể vượt ngưỡng một cách hợp lệ và bị đánh dấu CŨ nhầm — ngưỡng thích ứng nằm ở giai đoạn sau.

---

## Mục tiêu dự án

Phát triển scanner này thành **bot Funding Rate Arbitrage**: giữ đồng thời vị thế spot long và perpetual short cùng giá trị danh nghĩa, thu phí funding mỗi chu kỳ, duy trì trung hoà rủi ro giá (delta ≈ 0).

**Kỳ vọng lợi nhuận: 5–15%/năm.** Đây là chiến lược thu phí ổn định, không phải chiến lược lợi suất cao.

Lộ trình chia **9 giai đoạn / 41 bước**:

| GĐ | Nội dung | Trạng thái |
|---|---|---|
| 0 | Nền tảng scanner | ✅ Xong ~90% |
| 1 | Củng cố lõi — staleness, phí, tách spot/perp, test | 🔄 Đang làm (7/7 bước, còn phiên 72h) |
| 2 | Funding Rate Monitor — thu thập, lưu trữ, instrument registry | ⬜ |
| 3 | Signal, Alert & Backtest | ⬜ |
| 4 | Execution Engine — đặt lệnh, xử lý khớp một phần | ⬜ |
| 5 | Risk & Vận hành production | ⬜ |
| 6 | Basis Trade | ⬜ |
| 7–8 | CEX-DEX, Cross-Chain, Statistical | 🔒 Khoá |

Chi tiết từng bước kèm **tiêu chí nghiệm thu**: [docs/PLAN.md](docs/PLAN.md).

---

## Sàn và cặp hỗ trợ

**Cặp:** `BTCUSDT` · `ETHUSDT` · `XRPUSDT` · `SOLUSDT`

| Nguồn | Loại | Ghi chú |
|---|---|---|
| Binance | Futures + Spot | |
| Bybit | Futures + Spot | |
| OKX | Futures | Ký hiệu `BTC-USDT-SWAP` |
| Gate.io | Futures | Đặt lệnh theo **contract** |
| Kraken | Futures | Ký hiệu `PF_XBTUSD`, quote là **USD** không phải USDT |
| Hyperliquid | Futures (DEX) | Funding chu kỳ **1 giờ** |
| Paradex | Futures (DEX) | Funding **liên tục**, không có mốc rời rạc |
| Pyth | Oracle | Chỉ tham chiếu, không giao dịch |

> Bảy sàn này bất đồng với nhau nhiều hơn vẻ ngoài — khảo sát chi tiết ở [docs/DATA-REQUIREMENTS.md](docs/DATA-REQUIREMENTS.md#3-khảo-sát-funding-rate-7-sàn).

---

## Cách hoạt động

**Backend (Go)**

- Mỗi sàn chạy trong một goroutine độc lập, nhận sổ lệnh qua WebSocket
- Tính mid-price `(best bid + best ask) / 2`
- Dữ liệu truyền qua channel, không tranh chấp lock trên đường đi
- Tính spread và cơ hội, broadcast qua một WebSocket duy nhất tới mọi frontend

**Frontend**

- JavaScript thuần, không framework
- TradingView Lightweight Charts

### Quyết định kỹ thuật

| Quyết định | Lý do ngắn gọn |
|---|---|
| **Toàn bộ backend là Go** — kể cả REST khi thêm sau | REST và WS dùng chung bộ kiểu dữ liệu và tầng chuẩn hoá đơn vị. Tách ngôn ngữ nghĩa là viết logic chuẩn hoá từng sàn hai lần |
| **Backtest cũng là Go**, import thẳng `internal/strategy` | Backtest và production phải chạy cùng một đoạn code, nếu không thì cổng kiểm chứng ở Bước 3.5 mất giá trị chẩn đoán |
| **Python chỉ đọc SQLite** để vẽ và khám phá dữ liệu | Không bao giờ nằm trong vòng lặp giao dịch. Trở thành bắt buộc ở GĐ 8 (thống kê/ML) |
| **Không dùng CCXT** | Nó chuẩn hoá đi đúng những khác biệt giữa các sàn mà dự án này cần nhìn thấy |
| **Giữ vanilla JS** | FE không phải nút thắt — lãng phí nằm ở tầng broadcast phía server |

Lập luận đầy đủ: [docs/PLAN.md §7](docs/PLAN.md#7-quyết-định).

---

## Cài đặt và chạy

**Yêu cầu:** Go 1.23.5 trở lên.

```bash
git clone <repo>
cd crypto-futures-arbitrage-scanner
go run ./cmd/scanner
```

Mở trình duyệt tại **http://localhost:8082**

### Cấu hình

Mọi thứ vận hành nằm trong **`config.yaml`** ở gốc repo: cặp giao dịch, danh sách
sàn, ngưỡng cảnh báo, ngưỡng dữ liệu cũ theo từng sàn, biểu phí, và cách mỗi sàn
đặt tên cho cùng một thị trường.

**Thêm một cặp** — một dòng, và mỗi sàn tự dựng ký hiệu của nó:

```yaml
symbols:
  - { symbol: DOGEUSDT, base: DOGE, quote: USDT }
```

**Thêm một sàn** dùng connector sẵn có — một khối trong `sources:`. Sàn hoàn toàn
mới thì cần viết connector Go; `connector:` chọn trong số đã có.

**Nhập biểu phí của chính bạn** — phí phụ thuộc bậc VIP và khối lượng 30 ngày,
nên biểu phí tài khoản bạn mới là con số đúng:

```yaml
    fee:
      maker_bps: 2.0
      taker_bps: 5.0
      verified: true
      doc_url: https://…
```

`verified: false` nghĩa là **chưa tra được**, không phải miễn phí — cặp nào có
một sàn như vậy thì không có số sau phí.

**Lưu trữ (Bước 2.6)** — khối `storage:` bật/tắt toàn bộ phần ghi SQLite.
`enabled: false` là scanner chạy y như trước, không mở file, không ghi gì.

```yaml
storage:
  enabled: true
  path: "data/scanner.db"
  price_sample_every_sec: 30      # ĐO: 133,3 byte/dòng → 1,24 GB cho 90 ngày
  retain_funding_days: 365        # 0 = giữ vĩnh viễn
  retain_price_days: 90
```

Nạp kho lịch sử funding cho backtest (chạy một lần, vài phút, mở socket thật):

```bash
go run ./cmd/backfill                      # 12 tháng, mọi cặp trong config
go run ./cmd/backfill -months 6 -symbol BTCUSDT
```

Chạy lại lúc nào cũng an toàn: mỗi dòng khoá theo (sàn, cặp, mốc settle) nên
lượt thứ hai không ghi thêm gì. Báo cáo cuối in **độ sâu thật của từng chuỗi** —
đọc nó, đừng giả định 7 sàn sâu như nhau.

Dùng file khác: `go run ./cmd/scanner -config /đường/dẫn/khác.yaml`.
Biến môi trường `PORT` (hoặc `.env`) vẫn được ưu tiên hơn `server.port`.

> ⚠️ Ngưỡng này áp lên **spread thô**, chưa trừ phí. Dashboard ghi rõ điều này ngay dưới ma trận. Với phí taker thông thường của cả hai chân, chi phí vòng lặp đã vượt xa 0,05% — nên con số hiển thị hiện tại phản ánh *cấu trúc thị trường*, không phải *cơ hội có thể thực thi*. Mô hình phí nằm ở Bước 1.3 trong lộ trình.

Symbol, danh sách sàn và mọi ngưỡng đều nằm trong `config.yaml` từ Bước 1.4 —
không còn hardcode ở Go hay JavaScript.

---

## Cấu trúc thư mục

```
.
├── cmd/scanner/               # entrypoint — wiring connector, HTTP server
├── cmd/fundingcheck/          # kiểm chéo field funding của 7 sàn qua REST
├── cmd/backfill/              # nạp lịch sử funding vào SQLite (Bước 2.6)
├── CLAUDE.md                  # tổng quan cho AI agent
├── exchanges/                 # connector — CHỈ dữ liệu công khai, không credential
│   ├── types.go               # kiểu dùng chung
│   └── <venue>.go             # binance, bybit, okx, gate, kraken, hyperliquid, paradex, pyth
├── wire.go                    # hợp đồng JSON với dashboard — xem docs/WS-CONTRACT.md
├── internal/                  # các package đang xây dựng theo lộ trình
│   ├── scanner/               # engine: state giá, staleness, hợp đồng wire (WS-CONTRACT.md)
│   ├── instruments/           # registry, ánh xạ spot↔perp, sizing delta-neutral
│   ├── fees/                  # bảng phí, lợi nhuận ròng
│   ├── history/               # REST sàn → store (dùng chung scanner & backfill)
│   ├── store/                 # SQLite: funding_history, price_snapshots, instrument_snapshots
│   ├── strategy/              # APR, tín hiệu vào/ra
│   ├── backtest/              # replay lịch sử
│   ├── notify/                # Telegram, Discord
│   ├── broker/                # ⚠️ package DUY NHẤT giữ credential
│   ├── execution/             # vị thế delta-neutral
│   └── risk/                  # margin, kill switch, giới hạn vốn
├── static/                    # dashboard
└── docs/                      # PLAN, WORKFLOW, DATA-REQUIREMENTS, CONVENTIONS, WS-CONTRACT
```

Các package `internal/` chưa tới lượt xây thì hiện chỉ chứa `doc.go` mô tả trách nhiệm và ranh giới. **Đọc `doc.go` trước khi thêm code vào package đó.**

**Luật phụ thuộc — vi phạm là lỗi chặn merge:**

```
exchanges/        KHÔNG import bất kỳ internal/ nào
internal/broker/  KHÔNG reachable từ luồng nhận dữ liệu
```

Dữ liệu công khai và credential nằm hai phía khác nhau của ranh giới này.

---

## Tài liệu

| File | Nội dung |
|---|---|
| [docs/PLAN.md](docs/PLAN.md) | Lộ trình 9 giai đoạn / 40 bước, tiêu chí nghiệm thu, sổ rủi ro, quyết định cần chốt |
| [docs/WORKFLOW.md](docs/WORKFLOW.md) | Quy trình code & review 9 pha, checklist review, quy tắc commit |
| [docs/DATA-REQUIREMENTS.md](docs/DATA-REQUIREMENTS.md) | Dữ liệu cần từ sàn, khảo sát funding 7 sàn, thiết kế `FundingData`, các bẫy dữ liệu |
| [docs/CONVENTIONS.md](docs/CONVENTIONS.md) | Quy ước đặt tên, cấu trúc, lỗi, đồng thời, test, luật phụ thuộc |
| [docs/WS-CONTRACT.md](docs/WS-CONTRACT.md) | Hợp đồng JSON backend ↔ dashboard, trường nào mang dữ liệu thật ở bước nào |
| [CLAUDE.md](CLAUDE.md) | Tổng quan cho AI agent — luật, bẫy đã biết, điểm yếu hiện tại |

---

## Đóng góp

Mọi thay đổi đi theo vòng lặp 9 pha trong [docs/WORKFLOW.md](docs/WORKFLOW.md) — đối chiếu hiện trạng trước khi code, review trước khi commit, **một bước trong lộ trình = một commit**.

Trước khi viết code, đọc [docs/CONVENTIONS.md](docs/CONVENTIONS.md). Vài điểm quan trọng nhất:

- Tên định danh, comment, commit message: **tiếng Anh**. Tài liệu: tiếng Việt.
- **Mọi biến mang đơn vị phải nói ra đơn vị:** `IntervalSec`, `RatePer8hFrac`, `NotionalUSD`, `TakerFeeBps`, `QtyContracts`. Một biến tên `rate` hay `size` trần trụi đi qua ranh giới hàm là **lỗi**, không phải vấn đề thẩm mỹ.
- Không hardcode chu kỳ funding. Không bao giờ.

Trước mỗi commit:

```bash
gofmt -l .          # phải không in ra gì
go vet ./...
go test ./...
go test -race ./... # khi đụng tới goroutine
```

---

## Cảnh báo rủi ro

Đây là **phần mềm thử nghiệm**, không phải lời khuyên đầu tư.

- Số liệu hiển thị **chưa trừ phí giao dịch, phí rút và slippage**. Spread trông có lãi trên màn hình thường không còn lãi sau chi phí.
- Arbitrage crypto mang rủi ro thật: rủi ro thực thi, rủi ro thanh lý, rủi ro đối tác (sàn), rủi ro kỹ thuật.
- Không có chiến lược nào ở đây được kiểm chứng bằng vốn thật. Cho tới khi hoàn thành Giai đoạn 3, **chưa có bằng chứng nào cho thấy chiến lược sinh lời**.
- Nếu và khi dự án đến giai đoạn giao dịch: bắt đầu với vốn tối thiểu, tắt quyền rút tiền trên API key, và backtest trước khi bỏ vốn thật.

---

## Ghi công

Phần nền scanner (kiến trúc connector đa sàn, ma trận spread, dashboard) khởi nguồn từ dự án mã nguồn mở của [jose-donato](https://github.com/jose-donato). Phần lộ trình funding rate arbitrage, tài liệu dữ liệu và cấu trúc `internal/` là phát triển riêng của repo này.

> 📄 Repo hiện **chưa có file LICENSE**. Nếu có ý định chia sẻ công khai, nên bổ sung giấy phép và đối chiếu với giấy phép của dự án gốc.
