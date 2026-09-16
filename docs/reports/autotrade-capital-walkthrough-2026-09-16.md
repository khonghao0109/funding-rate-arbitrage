# Walkthrough — Vốn theo slot có đệm & cân bằng định kỳ cho Auto-Trader

**Ngày:** 2026-09-16 · **Phạm vi:** `cmd/execportal/autotrade` (Q18, CHỈ TESTNET)
**Trạng thái:** 🟡 mã đã kiểm bằng test, **chưa nghiệm thu trên testnet**
**Đặc tả nguồn:** yêu cầu "Buffered-Slot & Periodic Rebalancing" của người vận hành
**Chi tiết đầy đủ:** [PLAN.md → "Công cụ vận hành 4.5g"](../PLAN.md)

Tài liệu này viết cho người vận hành và người review: nó nói cái gì đổi, đổi ở đâu,
chỗ nào lệch đặc tả và vì sao, và phải nhìn gì khi chạy thật.

---

## 1. Vấn đề

Bot cấp cho mọi vị thế đúng một con số người vận hành gõ một lần: **65 quote mỗi chân**.
Hệ quả:

- **Lãi không được tái đầu tư.** Tài khoản gấp đôi thì bot vẫn mở 65.
- **Lỗ không được thu hẹp.** Tài khoản còn một nửa thì bot vẫn mở 65, và ví ký quỹ cạn
  dần cho tới khi một lệnh mở bị sàn từ chối — mà một lệnh mở bị từ chối là một lần
  "giao dịch hỏng", và **5 lần liên tiếp thì cặp DỪNG BẢO VỆ**.
- **65 quote BTC vốn dĩ đã sai.** Bước nhảy của BTCUSDT futures trên testnet này là
  0,0001 BTC ≈ 7,70 quote ở giá 77.000. Đặt 65 thì 0,0008 lên sàn, tức 61,6 — **5,2% của
  slot không bao giờ vào thị trường**.

---

## 2. Cái gì đổi

### 2.1. Đọc vốn: `Trader.Account` (mới)

```go
type Account struct {
    QuoteAsset        string
    SpotQuoteTotal    float64  // free + locked trên tài khoản SPOT
    FuturesQuoteTotal float64  // số dư VÍ futures (đã gồm ký quỹ đang khoá)
    ReadAtMs          int64
}
```

Đọc từ SÀN (quy tắc 7), nhiều nhất **một lần mỗi chu kỳ cân bằng** — không theo nhịp
quét 10 giây. Tốn weight 20 (spot) + 5 (futures).

Tài sản định giá lấy từ cái **perp KHAI** cho symbol đầu của run, không phải chuỗi
`"USDT"`, và hai sàn phải khai giống nhau (bẫy *assets* của CLAUDE.md).

### 2.2. Cấp quy mô: `planNotional` (mới, `autotrade/capital.go`)

Công thức của đặc tả, nguyên văn:

```
tradable = equity × (1 − buffer)
slot     = tradable / N
notional = slot / (1 + marginFrac)
```

**rồi lấy số NHỎ NHẤT giữa nó và bốn giới hạn khác:**

| giới hạn | công thức |
|---|---|
| ví spot tự gánh nổi | `spotPool / N` |
| ví futures tự gánh nổi | `futuresPool × (1 − buffer) / (N × marginFrac)` |
| hạn mức vốn của run | `capQuote / (N × (1 + marginFrac))` |
| trần notional mỗi chân của portal | `maxNotionalQuote` |

`notionalPlan.BoundByVI` nêu tên cái nào đã chặn — **thứ đầu tiên phải đọc khi một quy mô
không như bạn nghĩ.**

### 2.3. Lịch: `Engine.rebalance` (mới)

Chạy ở ĐẦU mỗi `Step`, ba phần:

1. **Quyết** dưới khoá `e.mu` (rẻ, không gọi sàn).
2. **Đọc** số dư **không giữ khoá** (CONVENTIONS §9: khoá không bao giờ giữ qua một lần
   gọi sàn).
3. **Áp** lại dưới khoá.

Vì nó chạy TRƯỚC khi dựng job, quy mô mới áp cho **chính lượt quét đó**, không trễ một lượt.

### 2.4. Tham số mới trên `PortfolioConfig`

| tham số | mặc định | chặn |
|---|---|---|
| `AutoRebalance` | **BẬT** | — |
| `MarginBufferPct` | `0.30` | `[0.10, 0.50]` |
| `RebalanceIntervalHours` | `168` (7 ngày) | `[1, 8760]` |
| `LastRebalancedAtMs` | `0` = chưa lần nào ⇒ **đến hạn ngay** | ≥ 0 |

Cả hai giá trị được `Validate` kiểm **dù công tắc đang TẮT**: một run bật lên sau đó với
đệm vô nghĩa sẽ cấp quy mô sai ngay lúc bật.

### 2.5. Điều kiện vào mới: `size_fits` (huy hiệu **Q** trên radar)

Quy mô phải ≥ cái **chặt nhất** trong ba thứ, đọc từ `exchangeInfo` của chính hai testnet:

- notional tối thiểu của sàn,
- lượng tối thiểu × giá,
- **bước nhảy × giá ÷ 5%**.

**Luật CHƯA ĐỌC ĐƯỢC không phải "không có giới hạn"**: check báo không đánh giá được,
và một điều kiện vào không đánh giá được thì không đạt.

### 2.6. Danh sách cặp mặc định: 12 → **7**

**Giữ:** BTC, ETH, LINK, UNI, LTC, SUI, AAVE
**Bỏ:** SOL (funding âm gần như quanh năm), XRP (funding ≈ 0), NEAR (188 lần đảo dấu/năm),
BNB và DOGE (đáy bảng xếp hạng funding 3 năm — CLAUDE.md, quy luật 3).

> ⚠️ Đặc tả nêu đích danh 3 cặp phải bỏ và 7 cặp giữ lại; BNB và DOGE không nằm trong danh
> sách nào. Tôi đọc "Giữ lại …" theo nghĩa đen — danh sách mới **đúng bằng 7 cặp đó**.
> Nếu ý bạn là giữ BNB/DOGE, đổi một dòng trong `cmd/execportal/main.go`.

### 2.7. Trang

- Thẻ mới **"Phân bổ vốn theo slot"**: notional mỗi chân + vốn mỗi slot, số slot, đệm,
  chu kỳ + lần gần nhất, vốn đang dùng, và một dòng nói rõ cách tính.
- Huy hiệu **Q** trên radar (điều kiện quy mô), hộp gợi ý cập nhật 5 → 6 điều kiện.
- Ba ô nhập mới: **đệm %**, **chu kỳ giờ**, **công tắc tự động cân bằng**.
- Hộp xác nhận BẬT nói rõ notional trên form **chỉ là mức khởi tạo**.
- `autotrade.js` vẫn **không gửi gì** — mọi lệnh ghi vẫn ở `execution.js`.

---

## 3. Bốn chỗ lệch đặc tả, và vì sao

### 3.1. Một pool vốn không tồn tại → chặn thêm theo từng ví

Đặc tả tính `E_total` như một túi tiền. Trên testnet này spot và futures là **hai đăng ký
tách biệt** — khoá futures gửi tới host spot bị từ chối `-2015` (PLAN 4.1) — nên **không
gì chuyển được một quote từ ví này sang ví kia.**

Ví dụ cụ thể, 4 slot, đệm 30%, ký quỹ 0,5:

| | công thức một-pool | luật nhỏ-nhất |
|---|---|---|
| ví spot 10.000, futures 1.000 | **1.283** mỗi chân | **466** mỗi chân |
| ký quỹ cần | 3.850 từ một ví có 1.000 ❌ | 933 từ 700 khả dụng ✅ |

Đây **chính là mục tiêu đặc tả tự nêu** — "không… cạn tiền ký quỹ ví Futures".

### 3.2. "Sai số hedge ≤ 5%" đo nhầm thứ

`internal/execution` đã khử rủi ro đó **bằng cấu trúc**: nó làm tròn mỗi chân trên lưới
của sàn đó, lấy số **nhỏ hơn**, làm tròn lại trên sàn kia, rồi mở — nên hai chân mở **bằng
nhau** hoặc cả ý định bị từ chối (`execution/doc.go`, "Sizing, before anything is sent").
Sai số hedge sau một lần mở thành công là **0**.

Cái 5% thật sự chặn là **vốn không vào được thị trường**. Điều đó vẫn đáng chặn — cả cơ
chế slot dựa trên việc mỗi slot triển khai đúng phần của nó, và một slot lặng lẽ chỉ dùng
90% làm cái đệm thật **to hơn** cái đã cấu hình.

**Ngưỡng, số học và hệ quả giữ đúng đặc tả** (BTC cần ~154 quote); chỉ cái **tên** được
sửa cho đúng thứ nó đo: `SizeErrorPct`, không phải `HedgeErrorPct`.

### 3.3. `OpenSpotValueQuote` phải được cộng vào, nếu không là bánh cóc

Vị thế đang mở là vốn spot đang nằm dưới dạng **coin**. Bỏ nó ra thì mỗi lần mở một cặp,
"tổng vốn" tụt và slot sau nhỏ đi; mỗi lần đóng lại phình ra.

Engine cộng từ **chính vị thế của nó**, đánh dấu theo giá giữa mới nhất. Coin **không**
thuộc vị thế nào của bot thì không tính — bot không được cấp quy mô trên vốn nó không
điều khiển.

### 3.4. 65 quote không còn là một quy mô

Với `AutoRebalance` bật, `LastRebalancedAtMs = 0` nghĩa là **đến hạn ngay**, nên lượt quét
đầu tiên của một run cấp quy mô **trước khi bất kỳ điều kiện vào nào được chấm**.

> ⚠️ **Một run TẮT cân bằng mà để 65 thì BTC không vào lệnh nữa** — nó trượt `size_fits`.
> Đây là hành vi đúng theo mục 3.2, nhưng nó là một thay đổi hành vi thấy được ngay.

---

## 4. Quy tắc bất di bất dịch, và chỗ nó được kiểm

> **Cân bằng KHÔNG BAO GIỜ đóng, thu nhỏ hay định giá lại một vị thế đang mở.**

Nó ghi đúng **một** trường: `PortfolioConfig.DefaultPairConfig.NotionalQuote` (và `p.cfg`
của các cặp không có cấu hình riêng). Một cặp đang giữ **giữ nguyên quy mô nó mở** cho tới
khi tự thoát, và **được định giá theo quy mô đó**, không theo slot mới.

Test `TestEngine_ARebalanceNeverClosesOrResizesAnOpenPosition` chứng minh bằng cách đọc
**sàn giả**, không đọc niềm tin của engine (bài học 4.4a): sau một tuần và tài khoản gấp
đôi — **0 lệnh đóng**, vị thế perp không đổi một chữ số, `OpenedAtMs`/`QtyCoin`/
`NotionalQuote` y nguyên, còn quy mô mặc định của run thì đã đổi.

---

## 5. Nghiệm thu

### ✅ Đã đạt

| | |
|---|---|
| `go test -count=1 -race ./...` | **37 package ok, exit 0**, 0 data race |
| `gofmt -l .` / `go vet ./...` | sạch |
| Guard & parity | `TestAutotrade_DecidesOnTheTestnetAndTradesOnlyThroughThePortal`, `TestExecportal_IntentStateHasExeccheckShape`, `TestExecportal_EveryOrderPathHasFixedCallers`, `TestUI_OnlyTheExecutionTabWrites` — không gãy |
| Phủ | `cmd/execportal/autotrade` **93,9%**, `cmd/execportal` **80,9%** |

**15 test mới**, mỗi trụ cột một cái: số học phân bổ từng số hạng · ví nào chặn thì nêu
tên **và** kiểm lại rằng `N × notional` vừa cả hai ví · không vượt hạn mức vốn hay trần
notional · 10 đầu vào hỏng đều **từ chối thay vì đoán** · lịch (1 lần đọc ở lượt đầu, 0
trong 3 lượt tiếp dù số dư gấp 5, đúng 1 lần nữa sau 168 giờ) · **không đụng vị thế đang
mở** · đọc hỏng thì giữ quy mô, **giữ đồng hồ**, thử lại ngay lượt sau · lỗi lặp = 1 dòng
log · cặp có cấu hình riêng không bị đổi · luật sàn (154 và 200 qua, **65 trượt**) · và
toàn đường qua API.

### ❌ Chưa làm

| | |
|---|---|
| **Giao diện, chạy thật** | Máy phiên này **không có Chrome/Chromium** — không dựng lại được cú bấm chuột headless của 4.5d/4.5e. Chưa có bằng chứng 0 lỗi console ở 1440/1024/400 px cho ba ô nhập mới, thẻ phân bổ vốn và huy hiệu **Q**. |
| **Chạy thật trên testnet** | Một `cmd/execportal` khác nghe `127.0.0.1:8087` suốt phiên và **không bị đụng tới**. Chưa có: bot đọc số dư thật, cấp quy mô thật, hay giữ một vị thế qua một lần cân bằng thật. |

**Vì hai tiêu chí này là hai tiêu chí 4.5d/4.5e phải đạt mới được đánh dấu ✅, 4.5g ở
trạng thái 🟡.**

---

## 6. Phải nhìn gì khi chạy thật

1. **Dòng `REBALANCE` đầu tiên.** Nó in toàn bộ số học:
   `tổng vốn X (ví spot A + ví futures B), đệm 30% = C, còn D chia N chỗ ⇒ E mỗi chỗ ⇒ F
   quote notional mỗi chân (chặn bởi …)`. Đọc `chặn bởi` trước tiên.
2. **Thẻ "Phân bổ vốn theo slot"** phải khớp dòng log đó.
3. **Huy hiệu Q trên radar.** Nếu cả 7 cặp đỏ, vốn không đủ cấp một slot qua sàn quy mô
   của sàn — giảm `MaxConcurrentPositions` để mỗi slot to lên.
4. **Sau một lần cân bằng có vị thế đang mở:** `execcheck -status` trên ý định đó phải cho
   đúng khối lượng cũ, và trang phải hiện `Vị thế ĐANG MỞ giữ nguyên quy mô cũ`.
5. **Weight.** Mỗi lần cân bằng tốn 25 weight, một tuần một lần — không đáng kể so với
   bức tường nửa ngân sách đọc.

---

## 7. Nợ có tên

- 🔴 *(từ 4.5c, chưa đổi)* JS feed và thư viện biểu đồ cùng origin với API lệnh — tách
  trước 4.6.
- `OpenSpotValueQuote` đo theo giá giữa của lượt quét **trước**, nên lệch nhiều nhất một
  lượt. Cặp chưa có giá giữa đóng góp 0 ⇒ cấp quy mô **nhỏ đi**, không to lên.
- Coin không thuộc vị thế nào của bot **không được tính**, kể cả 1 BTC testnet gieo sẵn.
- **Slot không được dồn.** Đặc tả gợi ý dồn slot cho cặp khác khi một cặp không đủ; ở đây
  `MaxConcurrentPositions` vẫn là N và cặp trượt `size_fits` chỉ đơn giản không vào. Thấy
  rõ trên radar, nhưng không có dòng nào nói "hãy giảm số slot".
- Cân bằng đọc số dư mỗi 168 giờ, nên **một lần rút tiền giữa chu kỳ không được thấy** cho
  tới lần đọc sau; hạn mức vốn và số slot vẫn chặn, nhưng quy mô thì cũ.
