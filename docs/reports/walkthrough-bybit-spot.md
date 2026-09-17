# Walkthrough — Động cơ 1 trên Bybit V5 UTA: chân Spot + portal `-broker=bybit` (Bước 4.5j, 2026-09-17)

> **Trạng thái: 🟡 CODE XONG, LỆNH TAY ĐÃ MỞ, AUTO-TRADER BYBIT CÒN CHẶN, CHƯA NGHIỆM THU BẰNG LỆNH.**
> Phần 1 (§1–§7) làm phần đọc và phải để portal `-broker=bybit` CHỈ ĐỌC sau review vòng 2. Phần 2
> (§8, cùng ngày, người vận hành duyệt phạm vi):
> - mua chân spot GỘP PHÍ;
> - xét chân spot theo LẦN KHỚP và SỐ DƯ VÍ ở đường mở, cắt và gỡ;
> - tách id của lệnh cắt;
> - mở lại lệnh tay; bot trên Bybit vẫn bị chặn cho tới khi nghiệm thu bằng tay.
>
> `bybitcheck` thật 19/19 trên ví đã có USDT. Chưa có lệnh nào được gửi tới sàn: nghiệm thu bằng lệnh
> là việc của người vận hành (§8.7).
> Quyết định lộ trình: **Q19** (PLAN §7.1). Không đụng vào 8085 / 8086 / 8087.

## 1. Đã làm gì

| Phần | Tệp | Nội dung |
|---|---|---|
| Broker spot | `internal/broker/bybit/{bybit,order,account,market,endpoints}.go` | Mỗi `Client` phục vụ MỘT thị trường (`spot` hoặc `linear`). `WithMarket` trả client anh em dùng CHUNG transport có ký. Lệnh spot có `isLeverage=0`; lệnh MARKET spot luôn có `marketUnit=baseCoin`. Luật spot, sổ lệnh 200 mức. `GetPosition(spot)` trả `ErrNotSupported`. `GetBalance` là hai góc nhìn của MỘT ví. |
| Năng lực tuỳ chọn | `internal/broker/bybit/realized.go` | Danh sách khớp (phí và tài sản thu phí), funding tài khoản, giá mark, lịch sử funding theo cửa sổ, phí tài khoản, bậc rủi ro. |
| Kiểm kết nối | `cmd/bybitcheck` | Đọc cả spot và linear: luật, sổ, chênh chạm, phí taker, quyền `SpotTrade`. Riêng perp còn đọc mark, funding đã settle, bậc rủi ro và dòng funding của tài khoản. |
| Thực thi | `internal/execution/close.go`, `types.go` | Chân spot bán theo **số dư sàn** khi phí mua spot đã bị trừ bằng coin (xem §3). |
| Portal | `cmd/execportal/{venue,venue_bybit,main,api,autotrade,pnl,cache}.go` | Cờ `-broker binance\|bybit`, hồ sơ sàn, adapter Bybit, thư mục ý định riêng. Ví hợp nhất đọc MỘT lần. |
| Phân bổ vốn | `cmd/execportal/autotrade/capital.go` | Nhánh `UnifiedWallet`: một bể vốn duy nhất, không áp giới hạn từng ví. |
| Trang | `static/js/shell.js`, `static/index.html` | Nhãn theo sàn. Ô "Tổng USDT" không cộng hai góc nhìn của cùng một ví. |

## 2. Lệch khỏi đặc tả, và vì sao

1. **`marketUnit=baseCoin` gửi cả khi BÁN**, không chỉ khi mua. Mặc định của lệnh bán hôm nay đã là
   `baseCoin`, nhưng gửi rõ ràng thì đơn vị không phụ thuộc vào một mặc định sàn có thể đổi.
2. **`FreeQtyCoin = walletBalance − (locked + totalOrderIM + totalPositionIM)`**, không phải
   `walletBalance − locked`. Trên UTA, USDT đang ký quỹ perp không dùng để mua spot được. Công thức
   của đặc tả sẽ báo dư tự do cao hơn thực tế.
3. **Futures chỉ trả dòng USDT**, spot trả mọi coin, như đặc tả. Hai kết quả là CÙNG MỘT ví, và
   portal không cộng chúng.
4. **Không dùng `map[string]any` cho body lệnh.** Body là struct có thứ tự trường cố định, để chuỗi
   byte được ký luôn giống nhau. Trường theo loại thị trường là con trỏ `omitempty`, nên body
   linear giữ nguyên từng byte (test cũ vẫn đạt).
5. **`FetchInstrument` spot đọc thêm `maxMarketOrderQty`.** Trường này không có trên trang tài liệu
   nhưng có trong mọi câu trả lời thật (ETHUSDT: 2706, nhỏ hơn `maxOrderQty` 8118). Mọi lệnh của
   execution là MARKET, nên trần nhỏ hơn mới là trần thật.
6. **Thêm một thay đổi ngoài đặc tả, trong `internal/execution`** (§3). Không sửa thì lần đóng
   đầu tiên trên Bybit để lại một chân spot trần.
7. **Portal trên Bybit dùng thư mục ý định riêng** `.paper/exec-bybit`, không dùng chung với
   `cmd/execcheck`. Id lệnh suy ra từ một ý định Binance, đem hỏi Bybit, sẽ thành "không thấy", và
   portal sẽ hiểu là vị thế chưa từng tồn tại.

## 3. Hai bẫy chỉ có trên Bybit, tìm ra khi đọc code trước khi viết

**(a) Ví hợp nhất bị đếm hai lần.**
- `portalTrader.Account` đọc USDT của thị trường spot và của thị trường futures thành hai ví riêng,
  rồi `planNotional` cộng lại.
- Trên UTA, hai lần đọc trả về CÙNG một số USDT. Có 1.000 USDT thì bot tưởng có 2.000 và chia chỗ theo
  2.000.
- Ô "Tổng USDT · Spot + Futures" trên trang cũng cộng như vậy.
- **Sửa:** `Account.UnifiedWallet`. Ví được đọc một lần, kế hoạch dùng một bể vốn. Một số futures đi
  kèm ví hợp nhất bị từ chối như đếm đôi. Trang hiện số của một ví.
- **Test:** `TestPlanNotional_UnifiedWalletIsOnePool`, `TestPortalTrader_UnifiedWalletIsReadOnce`.

**(b) Phí mua spot thu bằng coin làm kẹt chân spot khi đóng.**
- Theo tài liệu, taker MUA spot trả phí bằng coin gốc ("Side = Buy -> base currency (BTC)").
  Testnet đo được 10 bps.
- Lệnh báo khớp Q, nhưng ví chỉ còn Q × (1 − 0,001).
- `execution.Close` đóng perp TRƯỚC, rồi bán spot đúng Q. Sàn từ chối vì thiếu số dư, lúc đó perp
  đã phẳng, nên chân long spot bị để trần.
- **Tái hiện trước khi sửa:** test đỏ với "chân spot đóng 0 coin, chân perp đóng 0.3333 coin —
  UNWIND INCOMPLETE".
- **Sửa (trước khi gửi bất kỳ lệnh nào):**
  - Đọc số dư coin từ sàn.
  - Thiếu hụt vượt `MaxSpotBaseFeeFrac` (mặc định 0,2%, gấp đôi phí spot cao nhất đo được) cộng một
    bước: TỪ CHỐI cả lần đóng. Đó là coin bị ai khác dịch chuyển, không phải phí.
  - Thiếu hụt trong ngưỡng: bán đúng số ví giữ, làm tròn xuống theo lưới, ghi `SpotSellCappedByBalance`
    và nêu lý do. Phần chênh do phí được chấp nhận ở kiểm tra lệch.
- **Test:** `TestClose_SpotFeeTakenInTheBaseCoinDoesNotStrandTheSpotLeg`,
  `TestClose_SpotWalletShortByMoreThanAFeeIsRefusedBeforeSending`.
- **Trên Binance testnet:** phí spot bằng 0 và ví đã có sẵn 1 BTC, nên nhánh này không kích hoạt;
  đường cũ giữ nguyên.

## 4. Đo trên testnet thật (2026-09-17)

`go run ./cmd/bybitcheck` (chỉ GET, không lệnh, không in số dư): **18/19 mục đạt.**

| Mục | Kết quả |
|---|---|
| Đồng hồ | lệch 29–72 ms, vòng 76–168 ms |
| Quyền key | ContractTrade [Order, Position], Spot [SpotTrade], KHÔNG có Withdraw |
| SPOT BTCUSDT | bước 0,000001 BTC · tick 0,1 · tối thiểu 5 USDT · sổ 200/200 mức · chênh chạm 0,03–0,07 bps · **phí taker 10,00 bps** |
| LINEAR BTCUSDT | bước 0,001 BTC · tick 0,1 · tối thiểu 5 USDT · sổ 500/500 mức · chênh chạm 0,03 bps · **phí taker 5,50 bps** |
| Perp | mark 76.002,6 · funding đang hình thành 1,00 bps/kỳ · 21 mốc đã settle trong 7 ngày · bậc rủi ro 1: duy trì 0,33%, 150× · 0 dòng funding của tài khoản |
| Hai chân | cùng khai BTC/USDT |
| **HỎNG** | ví UTA chưa có USDT (chưa nhận Faucet), giống lần chạy 4.5i |

- **Vòng khứ hồi trên Bybit:** 2 × 10 + 2 × 5,5 = **31 bps**. Con số này đắt hơn Binance testnet
  (spot 0, futures 4 bps), và gần mức mainnet PLAN §7.4 đã dùng.
- **Chênh perp − spot đo được:** khoảng +11 bps.

**Portal `-broker=bybit` trên cổng phụ 8088** (`-autotrade=false`, chạy từ gốc repo):
- `/api/status`, `/api/account`, `/api/market`, `/api/positions`, `/api/orders`, `/api/funding`,
  `/api/intents` đều trả **HTTP 200**.
- Hai thị trường dùng chung một ngân sách request (12/600 và 17/600 hiện giống nhau ở hai ô).
- Ảnh chụp headless ở 1440 px và 400 px: nhãn "Bybit Spot", "Bybit USDT Perp · CHUNG VÍ UTA",
  huy hiệu "[BYBIT TESTNET DEMO]", ba ô USDT ghi rõ là một ví, ô tổng không cộng.
- Tiến trình phụ đã tắt sau khi đo. Chỉ còn 8085 / 8086 / 8087 gốc.

**Bẫy đo được lúc chạy thật:** phiên bản đầu từ chối `maintenanceMargin ≥ 0,5` vì cho rằng đó là
phần trăm. Testnet trả bậc cuối "0.6" / "1" / 1×, đúng là phân số, và lần kiểm kết nối đã HỎNG vì
chính rào này. Nay đơn vị được kiểm bằng đẳng thức `initialMargin × maxLeverage ≈ 1` trên mọi bậc.
Một sàn trả phần trăm vẫn bị từ chối.

## 5. Kiểm thử

- `go test -count=1 -race ./...`: **38/38 package đạt**, `gofmt -l .` rỗng, `go vet ./...` sạch.
- Test mới:
  - `internal/broker/bybit/spot_test.go` (12 test);
  - `TestResolveMode`;
  - hai test đóng lệnh trong `internal/execution`;
  - test kế hoạch vốn ví hợp nhất;
  - `venue_bybit_test.go` (adapter giữ đủ `TradeReader` / `FundingReader` / `MarkPriceReader`, và ví
    hợp nhất đọc một lần);
  - `TestExecportal_BybitMarketsResolveToANonProductionHost` (cả testnet lẫn demo, một transport, thư
    mục ý định riêng, không có `BYBIT_MODE` thì không chọn host).

## 6. Review

**Vòng 1 (ngữ cảnh sạch): 0 chặn, 3 lớn, 5 nhỏ.** Tự tìm thêm 1 lớn khi đọc lại. Đã sửa hết kèm test.
Mỗi bản sửa được kiểm bằng đột biến: gỡ bản sửa thì test tương ứng đỏ.

| # | Mức | Phát hiện | Sửa |
|---|---|---|---|
| M1 | lớn | Ngưỡng phí của lần đóng tính trên chân CÒN LẠI nên co lại sau một lần đóng một phần, trong khi khoảng hụt do phí giữ nguyên. Đóng bị giới hạn theo ví mà perp chỉ khớp một phần thì báo "vẫn phòng hộ ở cỡ nhỏ hơn", thực ra chỉ còn perp short. | Ngưỡng neo vào lần mua spot GỐC (`CloseRequest.EntrySpotFilledQtyCoin`, portal truyền vào). Ví đã bán hết mà perp còn dư thì báo `ErrUnwindIncomplete` (một chân). |
| M2 | lớn | `Open` báo `both_open`, dư 0 trong khi ví spot thiếu Q × phí, vượt dung sai khi Q × 0,001 lớn hơn bước perp (DOGE > 1.000, SOL > 100, ETH > 10). | `Intent.SpotBuyFeeInBaseFrac` lấy từ phí của chính tài khoản. `Open` từ chối trước khi gửi (`ErrSpotFeeUnhedged`) nếu khoảng hụt vượt dung sai hoặc phí vượt `MaxSpotBaseFeeFrac`. |
| M3 | lớn | Điều kiện "ví đã bán hết" trước khi chấp nhận khoảng hụt không có test: rút gọn nó thì một lệnh bán spot khớp một nửa bị báo `both_flat`. | Test mới với chân spot khớp 50%. |
| tự tìm | lớn | Đường GỠ khi mở (perp hỏng sau khi spot đã khớp) bán đúng lượng spot đã khớp. Trên Bybit sàn từ chối, chân long bị để trần, và bằng chứng phẳng thứ hai báo xung đột giả. | Gỡ bán số ví thực nhận: đã khớp × (1 − phí), không quá phần số dư sàn tăng thêm. Bằng chứng phẳng so với đúng số đó. Có test cho cả khi không đọc được ví. |
| n1 | nhỏ | Ngưỡng dùng bước perp: với chân nhỏ nhất, một bước perp bằng cả chân. | Dùng bước SPOT. |
| n2 | nhỏ | Lãi/lỗ trên Bybit thiếu phí mua spot thu bằng coin, khoản phí lớn nhất của vòng khứ hồi. | Trang lãi/lỗ ghi rõ khoản bị loại. Không quy đổi, vì không có giá đo tại thời điểm đó. |
| n3 | nhỏ | Huy hiệu hiện "[BYBIT TESTNET DEMO]". | Bỏ chữ "DEMO" gắn cứng. |
| n4 | nhỏ | `OrderTrades` chỉ đối chiếu danh sách rỗng với `cumExecQty`. | Luôn đối chiếu tổng `execQty` với `cumExecQty`. |
| n5 | nhỏ | `FundingIncome` chỉ tin trường `funding`. | Giữ nguyên: dấu khớp tài liệu và ví dụ. |

**Vòng 2 (ngữ cảnh sạch, trên các bản sửa vòng 1): 3 lớn, 8 nhỏ.** Cả ba lớn đều có chung một gốc:
`execution` vẫn xét chân spot theo lượng LỆNH khớp ở một số đường.

| # | Mức | Phát hiện | Xử lý |
|---|---|---|---|
| M1 | lớn | Đường mở THÀNH CÔNG không đọc lại ví. Hai trường hợp báo `both_open` trong khi ví lệch perp quá dung sai: spot khớp một phần ở chế độ song song (kể cả khi `reduceToMatch` đã cắt cặp), và sàn thu phí CAO hơn mức công bố. | **Chưa sửa.** Bằng cổng chỉ đọc bên dưới, không lệnh nào tới được đường này trên Bybit. |
| M2 | lớn | Perp đóng một phần rơi đúng mức giới hạn, nên không có khoảng hụt nào được chấp nhận, rồi `Close` trả nil trong khi ví trống và perp còn một bước short. | Đã sửa: ví đã bán hết mà perp còn BẤT KỲ phần dư nào là một chân (`ErrUnwindIncomplete`). Test mới, kiểm bằng đột biến. |
| M3 | lớn | Từ chối theo cỡ (Q × phí > bước) sẽ dừng bảo vệ auto-trader trên các cặp có bước thô, vì mỗi lượt quét tính là một lần giao dịch hỏng. | **Chưa sửa.** Cách đúng là mua spot Q / (1 − phí) và bỏ lệnh từ chối. |
| n1 | nhỏ | Bằng chứng phẳng của lệnh gỡ so với mức phí công bố, không so với số đã giới hạn theo ví. | Đã sửa. |
| n2 | nhỏ | `reduceToMatch` và lệnh gỡ dùng chung ClientOrderID (có từ trước thay đổi này). | Nợ có tên. |
| n3–n6 | nhỏ | Phí công bố cao hơn thực thu; trong ví đã có sẵn coin; giới hạn tính cả coin đang khoá; `planEntry` bỏ qua điều kiện "phần dư dưới mức tối thiểu". | Nợ có tên, cùng gốc với M1 và M3. |
| n7 | nhỏ | Khi phí bằng 0 hành vi không giữ nguyên từng byte: ví thiếu thì bị giới hạn hoặc từ chối, và mở thủ công từ chối khi không đọc được phí. | Chủ ý: cả hai đều an toàn hơn. |
| n8 | nhỏ | Chú thích nói Binance đo được 10 bps. | Đã sửa: Binance spot testnet là 0. |

Các lỗ hổng test được nêu tên (trần phí, khoảng giá trị hợp lệ của phí, dải của `NewOpener`) đã có test.

**Quyết định sau vòng 2 — portal Bybit CHỈ ĐỌC** (`venueProfile.OrdersBlockedVI`):
- Mọi endpoint ghi (mở, đóng, làm phẳng, bot) trả **403 `venue_read_only`** trước khi handler chạy.
- `-autotrade` bị từ chối lúc khởi động, và thông báo trạng thái nêu lý do.
- Các endpoint đọc vẫn trả lời.
- Test ghim cổng này, kiểm bằng đột biến.

## 7. Còn mở

0. **Thiết kế lại chân spot cho sàn thu phí bằng coin** (điều kiện để bỏ cổng chỉ đọc):
   - mua spot `ceil(Q / (1 − phí))` theo lưới, để ví giữ Q;
   - xét bất biến, `reduceToMatch`, lệnh gỡ và lệnh đóng theo số dư VÍ (đọc lại sau khi khớp), không
     theo lượng lệnh khớp;
   - ClientOrderID riêng cho `reduceToMatch`.
   - Đây là thay đổi máy trạng thái đã nghiệm thu của 4.4/4.5, nên cần người vận hành đồng ý phạm vi.
1. **Nghiệm thu bằng lệnh:**
   - nhận Faucet USDT;
   - `go run ./cmd/execportal -broker bybit -port 8088 -autotrade=false`;
   - mở rồi đóng một cặp BTCUSDT nhỏ nhất qua trang;
   - đo lệch, cửa sổ trần, phí spot thu bằng coin và nhánh `SpotSellCappedByBalance` trên sàn thật.
2. **Giới hạn cỡ do phí thu bằng coin:** `Open` TỪ CHỐI cỡ có Q × phí vượt dung sai (BTC trên Bybit:
   tối đa ~1 BTC; DOGE ~1.000; SOL ~100). Mục 0 thay nó bằng lệnh mua gộp phí.
3. **Từ chối của Bybit là HTTP 200 + retCode**, nên `definiteRejection` vẫn đọc là MƠ HỒ (nợ từ
   4.5i), và một lệnh bị từ chối sẽ được hỏi lại chứ không bị bỏ ngay.
4. **Portal Bybit mặc định `-autotrade=true`** như bản Binance, nhưng lúc này bị cổng chỉ đọc từ
   chối. Khi bỏ cổng, bật portal mà không kèm `-autotrade=false` nghĩa là bot tự đặt lệnh trên
   testnet (Q18 + Q19).
5. **Góc spot của ví hợp nhất chỉ liệt kê coin khác 0**, nên khi ví trống trang hiện "chưa đọc
   được" cho spot. Với execution, "không liệt kê" nghĩa là 0; nhãn trên trang còn chưa phân biệt.

## 8. Phần 2 — mua gộp phí, xét chân spot theo lần khớp và ví, mở lệnh tay (2026-09-17)

Người vận hành duyệt phạm vi mục §7.0. Mục §7.2 (giới hạn cỡ) đã được thay. Mục §7.4 vẫn chưa có hiệu
lực: trên Bybit bot bị chặn riêng (§8.1).

### 8.1 Đã sửa những gì

| Tệp | Thay đổi |
|---|---|
| `internal/broker/rounding.go` | `CeilToStep(v, step)`: làm tròn LÊN lưới, cùng dung sai theo số bước (1e-6) với `RoundOrder`, lượng tử hoá theo số chữ số của bước. Giá trị không phải số dương hữu hạn → 0, để `RoundOrder` từ chối theo tên. |
| `internal/execution/sizing.go` | Phí vượt `MaxSpotBaseFeeFrac` bị từ chối trước khi tính cỡ. Chân spot mua `CeilToStep(Qperp ÷ (1 − phí), bước spot)`, qua `RoundOrder` (minNotional, minQty, maxQty); `entryPlan.SpotTargetQtyCoin`; sổ lệnh chân spot định giá ở cỡ gộp phí. **Bỏ** lệnh từ chối "Q × phí > dung sai" (M3 vòng 2). Bất biến kiểm trên `lệnh spot × (1 − phí)`. |
| `internal/execution/open.go` — chân spot | Mục tiêu khớp chân spot là `SpotTargetQtyCoin` (tuần tự và song song). `spotHeldQtyCoin` — phí = 0: trả đúng số khớp, không đọc gì. Phí > 0: hai bằng chứng từ sàn. (a) **Lần khớp**: phí coin gốc do chính các lần khớp của lệnh mua khai (`spotCreditQtyCoin`, đọc lại tới khi tổng khớp đủ); chỉ khi không đọc được mới dùng phí công bố, và nói rõ. (b) **Ví**: số dư coin gốc tăng thêm, đọc lại mỗi `PollEvery` tới khi khớp (a) trong một bước spot hoặc hết `OrderSettleTimeout`. Trong dung sai của cặp → chân spot là số NHỎ hơn. Quá dung sai khi hết hạn → **`ErrSpotEvidenceConflict`** (bọc `ErrFlatEvidenceConflict`): hai chân để nguyên, không cắt, không gỡ, portal báo động, bot dừng cặp. |
| `internal/execution/open.go` — cắt và gỡ | Ví thiếu quá dung sai so với perp → `reduceToMatch` (dù cả hai lệnh đã "đủ"), không cắt được → gỡ. Gỡ bán `số lần khớp khai ví nhận − phần đã cắt`, rồi giới hạn thêm theo ví. `ReduceClientOrderID` (`|reduce`); `closeLeg` nhận id (n2). `Result.SpotHeldQtyCoin`, `SpotHeldSourceVI`, `SpotBuyBaseFeeQtyCoin`, `SpotBuyBaseFeeStated`. |
| `internal/execution/types.go`, `doc.go` | `ErrSpotEvidenceConflict`; tài liệu của `SpotBuyFeeInBaseFrac`, `ErrSpotFeeUnhedged`, bất biến. |
| `cmd/execportal/actions.go`, `cache.go`, `cmd/execcheck/main.go` | Phí lần khớp khai lúc mở được LƯU vào file ý định: `spot_buy_base_fee_qty_coin`, `spot_buy_base_fee_stated`. `execcheck` giữ cùng hai trường để lưu lại không làm mất chúng (test hình dạng file). |
| `cmd/execportal/hedge.go` | `hedgeFees`: lệnh MUA mở của một ý định trừ phí đã lưu, **không đọc lại sàn**, trên mọi sàn. Ý định không có số lưu, trên sàn thu phí bằng coin (`SpotBuyFeeInBaseCoin`), mới đọc lần khớp từ sàn (nhớ theo lệnh đã xong). Không đọc được, phí không nêu đồng tiền, tổng khớp lệch lệnh → chân spot là ẩn số. Id cắt nằm trong danh sách id của ý định; coin gốc lấy từ luật spot của sàn. |
| `cmd/execportal/hedge.go`, `api.go` — id chưa từng gửi | `GetOrder` của Bybit trả `ErrOrderNotVisible` (cố ý KHÔNG bọc `ErrOrderNotFound`) cho id mà cả `/v5/order/realtime` lẫn `/v5/order/history` không liệt kê. Trên tài khoản UTA, lịch sử giữ lệnh CÓ khớp 730 ngày và lệnh không khớp 24 giờ ("Get Order History"). Vậy khi đã qua độ trễ tạo lệnh, id không thấy ở đâu là id không khớp gì: chưa từng gửi, hoặc huỷ rỗng. Portal (chỉ portal, execution giữ nguyên cách đọc mơ hồ) đọc nó là VẮNG cho một ý định khi không có lệnh ghi nào đang chạy và file ý định cùng lần ghi gần nhất đã cũ hơn `notVisibleGrace` (30 s); trước đó ý định là ẩn số. Lượt đọc bên trong một lệnh ghi, trước khi gửi gì (`beforeSending`), không tính chính lệnh ghi đó là đang gửi. **Không bao giờ** áp cho lệnh MỞ của ý định (đã khớp): lịch sử được hỏi không kèm khung thời gian, mặc định 7 ngày, nên lệnh mở không thấy là bất thường → ẩn số (R1). Lượt đọc lại sau lệnh cân yêu cầu thấy chính lệnh cân (R2). Chỉ lệnh ghi CÓ THỂ gửi lệnh (`afterOrderWrite`) mới đóng dấu thời điểm; chạy thử reconcile thì không. |
| `cmd/execportal/venue.go`, `autotrade.go`, `main.go` | Bybit: `SpotBuyFeeInBaseCoin` true, `OrdersBlockedVI` trống (lệnh tay mở), **`AutotradeBlockedVI`**: `/api/autotrade/start` trả 403 `autotrade_blocked` trước khi đọc thân yêu cầu, `-autotrade` bị từ chối. Dừng, KILL và điều khiển cặp vẫn trả lời. |
| `cmd/execcheck/reconcile.go` | Cộng thêm id cắt khi tính lệch của một ý định. |
| `internal/broker/brokertest/fake.go` | Lệnh mua spot dưới `SpotBuyFeeInBaseFrac` khai phí bằng coin gốc trong danh sách khớp, khớp với số dư nó ghi có. |

**Lệch khỏi đặc tả, và vì sao:**

1. **Chân spot không phải "ví tăng thêm nếu ≥ 0".** Nó là hai bằng chứng — lần khớp và ví — và số
   NHỎ hơn trong dung sai; lệch quá dung sai thì báo động và không giao dịch tiếp. Ví tăng nhiều hơn
   lần khớp giải thích là coin từ nơi khác hoặc phí không khai. Ví tăng ít hơn là ví còn trễ hoặc coin
   đã đi đâu. Cắt perp theo một số dư còn trễ, hay bán coin không ai quy được, đều là giao dịch trên
   một trong hai con số mâu thuẫn nhau (review phần 2, M2).
2. **Phí lấy từ lần khớp, không phải phí công bố** (review phần 2, M1). Portal truyền phí TAKER, nhưng
   `Open` gửi LIMIT GTC. Phần nằm chờ khớp dạng maker, và phí maker có thể thấp hơn hoặc bằng 0 — khi
   đó ví nhận thêm Q × phí mà phí công bố không thấy.
3. **Đọc lại ví khi còn trễ** (không có trong đặc tả).
4. **Sửa thêm trạng thái phòng hộ của portal** (bắt buộc). Portal cộng chân spot từ LỆNH. Với lệnh mua
   gộp phí, cặp DOGE 20.000 coin sẽ hiện UNHEDGED (lệch 20 coin, dung sai 1). LÀM PHẲNG sẽ bán 20 DOGE
   ví không nhận, và lệnh ĐÓNG của trang từ chối "cặp LỆCH".
5. **Phí được lưu lúc mở** (review phần 2, B1). `/v5/execution/list` mặc định chỉ 7 ngày, và một cặp
   được giữ lâu hơn thế. Nếu đọc lại mỗi lần, sau 7 ngày chân spot thành ẩn số: trang không đóng được,
   5 lần đọc hỏng làm bot dừng cặp, và cả lệnh dừng lỗ basis lẫn thoát funding đều không chạy.
6. **Bot trên Bybit vẫn bị chặn** (review phần 2, B2). Gỡ cổng chỉ đọc cũng bật `-autotrade`, trong khi
   chân spot gộp phí mới chạy trên sàn giả. Nghiệm thu bằng tay trước; bỏ chặn là một dòng trong
   `profileFor`.
7. **`ErrSpotFeeUnhedged` vẫn còn**, cho phí vượt trần và cho trường hợp (về lý thuyết) cỡ gộp phí vẫn
   làm hỏng bất biến.
8. **Id không thấy ở đâu được đọc là vắng sau 30 s** (review phần 3, N1). Nếu đọc là ẩn số mãi, mọi vị
   thế Bybit trên trang đều không đọc được và không đóng được, vì bốn trong năm id của một ý định đang
   giữ chưa bao giờ được gửi. Hệ quả chấp nhận: 30 s sau mỗi lệnh ghi, cặp đó hiện CHƯA XÁC ĐỊNH và
   trang từ chối đóng nó.
9. **`reduceToMatch` KHÔNG được sửa miễn trừ reduceOnly** khi kiểm trước lệnh cắt perp. Lệnh cắt perp
   dưới minNotional bị coi là không đặt được, nên cặp GỠ về phẳng thay vì cắt. Có từ 4.4; sửa nó đổi
   hành vi Binance đã nghiệm thu. An toàn (phẳng), nhưng tốn một vòng khứ hồi — nợ có tên.

### 8.2 Kiểm thử

- `go test -count=1 -race ./...`: **38/38 package đạt, 0 data race**; `gofmt -l .` rỗng; `go vet ./...` sạch.
- Test mới trong `internal/execution/grossup_test.go`. Mọi khẳng định đọc VÍ và VỊ THẾ PERP của sàn giả:

| Test | Tình huống |
|---|---|
| `GrossesUpTheSpotBuySoTheWalletHoldsThePerp` (tuần tự + song song) | 0,3333 BTC — cỡ phần 1 từ chối — mở được: lệnh spot 0,33364, ví ≥ perp và dư < 1 bước. |
| `ACoarseStepPairOpensInsteadOfBeingRefused` | DOGE 20.000 (spot bước 0,1, perp bước 1): lệnh spot 20.020,1. |
| `AFeeChargedAboveThePublishedRateIsCaughtByTheWallet` | Công bố 10 bps, sàn thu 30 bps: cắt perp về theo ví bằng id CẮT; cỡ nhỏ không cắt được → gỡ phẳng. |
| `AFeeLowerThanPublishedIsReadFromTheFills` | Khớp maker, phí 0: lần khớp khai 0, spot dư được cắt bằng id CẮT (M1 review). |
| `WithoutFillsASurchargeIsAConflict` | Sàn không liệt kê lần khớp và thu cao hơn công bố: báo động, nói rõ đang dùng phí công bố. |
| `ParallelPartialSpotFillWithABaseCoinFeeShrinksThePerpToTheWallet` | Song song, spot khớp 50%, perp 100%. |
| `AnUnreadableWalletFallsBackToThePublishedFeeAndSaysSo` | Không đọc được ví: một bằng chứng, và nói rõ như vậy. |
| `AWalletThatTrailsTheFillIsReadAgainBeforeJudging` | Ví trả số cũ ba lần rồi đúng. |
| `AWalletStillBehindAtTheDeadlineIsAConflictNotACut` | Ví không bao giờ theo kịp: báo động, chỉ còn hai lệnh mở, perp nguyên (M2 review). |
| `CoinCreditedFromElsewhereIsAConflictNotTheSpotLeg` | Ví được cộng thêm 0,05 BTC từ nơi khác: báo động, không bán spot nào. |
| `ASmallExtraInTheWalletKeepsTheFillsFigure` | Dư thêm trong dung sai: chân spot vẫn là số lần khớp khai. |
| `ASpotFillShortOfTheGrossedUpOrderIsAShortLegOne` | Spot khớp đúng Q, thiếu một phần phí: chân 1 thiếu, perp không được gửi. |
| `AnUnwindAfterAMakerFillIsFlatWithoutAFalseConflict` | Khớp maker (phí 0), perp bị từ chối: gỡ về phẳng, không báo xung đột giả (M-a review phần 3). |
| `TheUnwindAfterAPartialCutChargesTheFeeOnTheOriginalBuy` | Lệnh cắt spot khớp một nửa rồi gỡ, ví không đọc được. |
| `InvariantHoldsOnTheWalletUnderABaseCoinFee` | Thuộc tính, 132 lần: 11 kiểu hành vi mỗi chân, hai thứ tự, phí sàn 10 và 14 bps. 0 vi phạm. |

- Portal:
  - `TestActions_BybitOpenStatusCloseWithTheSpotFeeInTheBaseCoin`, trên sàn giả trả lời id lạ như
    adapter Bybit (`ErrOrderNotVisible`):
    - mở 20k qua trang → phí lần khớp lưu trong file ý định;
    - danh sách khớp của sàn thành không đọc được (cửa sổ 7 ngày);
    - trong 30 s đầu cặp là CHƯA XÁC ĐỊNH, sau đó HEDGED;
    - đóng → phẳng, ví dư < 1 bước.
  - `TestHedge_ABaseCoinFeeThatCannotBeReadLeavesTheSpotLegUnknown`.
  - `TestBybitPortal_AcceptsWritesNowTheSpotLegIsJudgedByTheWallet`: lệnh tay tới handler, bot bị 403
    `autotrade_blocked`, cổng chỉ đọc vẫn chặn khi hồ sơ đặt nó.
- `broker`: hai test `CeilToStep`. `execution`: `TestReduceClientOrderID_…` (khác mọi id, ≤ 36 ký tự).
- Hai test cũ đổi vì hành vi đổi có chủ ý:
  - `TestOpen_RefusesASizeWhoseBaseCoinFeeUnbalancesThePair` bị bỏ (chính là M3).
  - `TestClose_PerpRemainderOnTheCapBesideAnEmptyWalletIsOneLeg` nay mở vị thế KHÔNG gộp phí, như mọi
    vị thế mở trước phần 2.
  - Hai số đếm lượt tra của `TestDoneOrders_ReadsAFinishedOrderOnce` tăng vì có thêm id cắt.

### 8.3 Tự phản biện — năm câu hỏi

**1. Bẫy bụi coin.** Lệnh mua gộp phí để lại trong ví `Qg × (1 − phí) − Q`, luôn ≥ 0 và **dưới một
bước spot** (đo trong test: 0,3333 BTC → ví 0,33330636, dư 0,00000636 < 0,00001). Lệnh đóng bán đúng Q
(vị thế perp), nên phần dư ở lại.
- *Lệch delta:* không. Phần dư < bước spot ≤ dung sai, và portal tính nó vào chính ý định đó. Sau khi
  đóng: spot +0,00000636, perp 0 → PHẲNG trong dung sai.
- *Tích tụ làm sai kiểm tra phẳng:* không.
  - Mọi bằng chứng phẳng và chân spot của `Open` đều so **trước/sau** trong cùng lần gọi, nên coin có
    sẵn trong ví không vào phép so.
  - Nhánh giới hạn theo ví của lệnh đóng chỉ bật khi ví **ít** hơn lượng bán; bụi chỉ làm ví nhiều
    hơn.
  - Kiểm "MỘT vị thế mỗi symbol" đọc vị thế perp và lệnh của các ý định, không đọc số dư ví.
- *Chi phí:* tối đa một bước mỗi vòng: BTC 1e-6 ≈ 0,076 USDT, DOGE 0,1 ≈ 0,015 USDT. Không gộp về 0
  được, vì phí lấy một lượng không nằm trên lưới và lệnh bán luôn làm tròn XUỐNG. Khoảng 66 vòng BTC
  thì bụi vượt minNotional 5 USDT và bán tay được.

**2. Mở song song, spot khớp 50%, perp khớp 100%.** Spot không đạt mục tiêu gộp phí →
`reduceToMatch` với chân spot lấy từ lần khớp và ví.
- Perp là chân lớn hơn → **mua lại perp** (reduceOnly) tới trong một bước của ví. Không có lệnh bán
  spot nào, nên không thể bị từ chối vì thiếu số dư.
- Nếu chân lớn hơn là spot, lượng bán tính từ số VÍ nên không vượt ví.
- Không cắt được → lệnh gỡ bán `số lần khớp khai − phần đã cắt`, giới hạn thêm theo ví tăng → không
  vượt ví.
- Chứng minh: test song song 50%, `TheUnwindAfterAPartialCut…` (sàn giả từ chối bán quá số dư), và 132
  lần chạy thuộc tính có `RefuseSpotSellBeyondBalance`.

**3. Sàn thu phí cao hơn công bố (12 bps thay vì 10).** Lần khớp khai 12 bps nên số ví nhận là đúng
ngay từ đầu, và ví xác nhận.
- Khoảng hụt so với perp ≤ dung sai (BTC 0,3333 trên Bybit: 0,0000067 BTC, bước perp 0,001) →
  `both_open`, và đó là SỰ THẬT theo bất biến.
- Khoảng hụt > dung sai → cắt perp về theo ví, hoặc gỡ về phẳng nếu lệnh cắt không đặt được. Test 30 bps
  đi cả hai nhánh.
- Sàn **không** liệt kê lần khớp: dùng phí công bố; ví sẽ lệch khỏi nó → nếu quá dung sai thì
  `ErrSpotEvidenceConflict`, không giao dịch tiếp (test `WithoutFillsASurchargeIsAConflict`).
- Không đọc được VÍ: chỉ còn lần khớp. Với Bybit đó vẫn là phí thực thu, và kết quả nói "chỉ có một
  bằng chứng".
- Chiều ngược lại — phí THẤP hơn công bố (khớp maker) — cũng được bắt: lần khớp khai 0, spot dư được cắt.

**4. Không hồi quy trên Binance (phí = 0).** Đo, không chỉ lập luận. Một test tạm chạy **200 lệnh mở
(10 × 10 kiểu hành vi mỗi chân × 2 thứ tự đặt lệnh) và 104 lệnh đóng**, trên commit `522ed35` (git
worktree riêng) và trên code cuối cùng (chạy lại SAU các sửa của review). Nó ghi mọi lệnh — thị trường,
phía, loại, khối lượng, giá, số khớp, trạng thái, id — cùng kết quả, phần dư, mục tiêu và **số lần đọc
số dư ví**.
- Bỏ cột id: hai file **giống hệt từng byte**.
- Khác biệt duy nhất: id của **51 lệnh cắt** (33 spot, 18 perp) đổi từ id GỠ sang id CẮT — đúng sửa lỗi
  n2.
- Vì sao:
  - với phí 0, `x / (1 − 0)` và `x × (1 − 0)` đúng bằng x theo IEEE 754;
  - các nhánh gộp phí, đọc lần khớp, đọc ví, báo động và `walletShort` đều có điều kiện phí > 0;
  - `0 > MaxSpotBaseFeeFrac` luôn sai.
- Portal Binance: file ý định không có phí lưu và `SpotBuyFeeInBaseCoin` là false, nên `readLegNet`
  không đọc lần khớp nào. Mỗi lượt quét tra thêm 2 id (id cắt), mỗi id một lần tra đơn lệnh.
- Test tạm và worktree đã xoá, không commit.

**5. Kiểm đột biến.** 24 đột biến; mỗi lần chạy lại gói bị ảnh hưởng. Mọi đột biến đều làm ít nhất một
test đỏ. Ba đột biến sống sót ở lượt đầu (M5, M7, M9), và M7 sống sót lần nữa sau khi thiết kế lại theo
review. Mỗi lần như vậy được thêm một test.

| # | Đột biến | Test đỏ |
|---|---|---|
| M1 | Bỏ gộp phí (`CeilToStep`) | 15 |
| M2 | Không đọc ví (chỉ lần khớp) | 4 |
| M3 | Không đọc lại ví khi còn trễ | 1 |
| M4 | Lệnh cắt dùng lại id gỡ | 5 |
| M5 | Mục tiêu chân spot = cỡ phòng hộ | 1 (thêm `ASpotFillShortOfTheGrossedUpOrderIsAShortLegOne`) |
| M6 | Ví thiếu không kích hoạt cắt | 4 |
| M7 | Trong dung sai lấy số ví, bỏ min | 1 (thêm `ASmallExtraInTheWalletKeepsTheFillsFigure`) |
| M8 | `classify` trên số lệnh khớp, không trên ví | 9 |
| M9 | Gỡ tính phí trên phần còn lại (công thức cũ) | 1 (thêm `TheUnwindAfterAPartialCut…`) |
| M10 | Portal Bybit không đọc lần khớp khi thiếu phí lưu | 1 |
| M11 | Portal quên id cắt | 1 |
| M12 | Portal bỏ qua phí không đọc được | 1 |
| M13 | Khôi phục cổng chỉ đọc Bybit | 2 |
| M14 | Portal bỏ qua phí lưu trong file ý định | 1 |
| M15 | Không chặn bật bot trên Bybit | 1 |
| M16 | Lần khớp và ví lệch quá dung sai mà không báo động | 3 |
| M17 | Không đọc lần khớp, chỉ dùng phí công bố | 5 |
| M18 | Portal đọc id không thấy ở đâu là ẩn số mãi (N1) | 1 |
| M19 | Không có khoảng chờ sau lệnh ghi | 1 |
| M20 | Lượt đọc trước khi gửi của lệnh ghi bị tính là đang gửi | 1 |
| M21 | Bằng chứng phẳng của lệnh gỡ theo phí công bố (M-a) | 1 |
| M22 | Lệnh mở đã khớp mà không thấy bị đọc là vắng (R1) | 1 |
| M23 | Lượt đọc sau lệnh cân coi chính lệnh ghi là đang gửi (R2) | 1 |
| M24 | Lượt đọc sau lệnh cân không bắt buộc thấy lệnh cân | 0 — **tương đương**: lệnh cân không thấy mà bị đọc là vắng thì phần dư vẫn là phần chưa cân, kết quả vẫn "VẪN LỆCH", đúng chiều an toàn |

Hai câu hỏi cụ thể:
- Bỏ `CeilToStep` → **15** test đỏ.
- Bỏ phần đọc ví của `effectiveSpotQtyCoin` → **4** test đỏ (M2); bỏ phần đọc lại khi ví trễ → **1**
  (M3); bỏ báo động khi lệch → **3** (M16).

### 8.4 Đo trên testnet thật (chỉ GET, 2026-09-17)

- `bybitcheck`: **19/19 đạt**, ví UTA có USDT; lệch giờ 216 ms; quyền `SpotTrade` + `ContractTrade`,
  không `Withdraw`.
- Luật: spot BTCUSDT bước 1e-6, tối thiểu 5 USDT, **trần 10 BTC**; linear bước 0,001, trần 500. Phí
  taker spot 10 bps, linear 5,5 bps.
- Trên testnet giá spot 75.939 và perp 77.260, chênh **+174 bps** — lệch riêng của testnet, không phải
  thị trường.
- Portal `-broker bybit -port 8088 -autotrade=false`, chạy trước các sửa của review: `/api/status`,
  `/api/positions`, `/api/market`, `/api/autotrade/status`, `/api/autotrade/pnl` đều HTTP 200. Thông
  báo không còn tiền tố chỉ đọc; vị thế `both_flat`. Đã tắt sau khi đo. 8085 / 8086 / 8087 giữ nguyên
  PID.

### 8.5 Còn mở

1. **Nghiệm thu bằng lệnh** qua trang (§8.7), rồi bỏ `AutotradeBlockedVI`. Đo:
   - lệnh spot gộp phí, ví nhận so với perp;
   - `SpotHeldSourceVI`, phí thực thu trong `/v5/execution/list` (maker hay taker);
   - thời gian ví và danh sách khớp trễ;
   - cửa sổ trần;
   - sau khi đóng: bụi.
2. Lệnh cắt perp dưới minNotional không dùng miễn trừ reduceOnly (§8.1 lệch 8).
3. `definiteRejection` với retCode Bybit (nợ 4.5i).
4. Binance mainnet không trả phí bằng BNB cũng thu phí bằng coin gốc. `openAs` truyền phí taker của tài
   khoản, và execution sẽ gộp phí rồi lưu phí lần khớp khai. Hồ sơ Binance chưa bật
   `SpotBuyFeeInBaseCoin` — cần xác nhận trước 4.6.
5. Ý định mở TRƯỚC phần 2 không có phí lưu. Trên Bybit không có ý định nào như vậy, vì portal chỉ đọc
   từ trước tới nay.
6. Nếu lần khớp không được liệt kê trong 5 s lúc mở, file ý định không có phí lưu; ý định đó đọc lại
   `/v5/execution/list` mỗi lần làm mới, và sau 7 ngày chân spot thành ẩn số (review phần 3, M-b — an
   toàn, nhưng chặn đóng qua trang). Execution đã đọc lại tới hết hạn; bước tiếp là ghi phí vào file ở
   lần đọc thành công đầu tiên.
7. Lệnh gỡ đọc ví một lần, không đọc lại khi ví còn trễ (review phần 3, m1): giới hạn theo ví có thể
   bán ít hơn số coin đang về; bằng chứng phẳng thường vẫn bắt được, nhưng không chắc.
8. **Vị thế Bybit giữ quá 7 ngày có thể không đọc được trên trang** (R1): `GetOrder` hỏi
   `/v5/order/history` không kèm khung thời gian. Nếu tra theo `orderLinkId` không vượt được 7 ngày mặc
   định, lệnh MỞ biến mất khỏi danh sách và cặp thành CHƯA XÁC ĐỊNH — an toàn, nhưng khi đó phải đóng
   bằng giao diện Bybit. Cần đo trên testnet với một lệnh cũ hơn 7 ngày, hoặc cho adapter hỏi theo
   cửa sổ neo vào `OpenedAtMs`.
9. Lãi/lỗ: phần coin mua thêm để gộp phí, và bụi mỗi vòng, chưa được nêu tên trên trang lãi/lỗ (review
   m5). Trang đã nói phí coin gốc bị loại khỏi phí đã trừ.

### 8.6 Review phần 2 (ngữ cảnh sạch, trước các sửa dưới đây)

**2 chặn, 2 lớn, 6 nhỏ.** Chặn và lớn đã sửa, mỗi cái có test và đột biến.

| # | Mức | Phát hiện | Xử lý |
|---|---|---|---|
| B1 | chặn | Portal đọc lần khớp của lệnh mua spot mỗi lần; `/v5/execution/list` mặc định 7 ngày, bộ nhớ đệm bị xoá mỗi lần ghi. Cặp giữ quá 7 ngày → chân spot ẩn số → không đóng được, bot dừng cặp, dừng lỗ không chạy. | Phí lần khớp khai lúc mở được lưu vào file ý định và dùng mà không đọc lại (M14, test lifecycle làm hỏng danh sách khớp). |
| B2 | chặn | Gỡ cổng chỉ đọc bật luôn `-autotrade` và `/api/autotrade/start` trên Bybit trước khi có lệnh thật nào. | `AutotradeBlockedVI` cho Bybit; lệnh tay mở (M15). |
| M1 | lớn | Phí truyền vào là phí TAKER công bố; khớp maker phí thấp hơn → `min(ước tính, ví)` bỏ đi phần coin thật, `Open` báo `both_open` trong khi ví dư quá dung sai, và portal lại báo UNHEDGED. | Phí lấy từ lần khớp; lệch lần khớp–ví quá dung sai là báo động (M16, M17). |
| M2 | lớn | Ví vẫn trễ khi hết 5 s → perp bị cắt về số dư cũ; nếu ví chưa tăng gì thì gỡ có thể báo phẳng khi spot còn long. | Hết hạn mà còn lệch quá dung sai → `ErrSpotEvidenceConflict`, không cắt, không gỡ (M16). |
| m1 | nhỏ | Phí thu bằng đồng thứ ba bị bỏ qua. | Giữ nguyên: phí trả bằng quote hay BNB thì coin về ví đủ, bỏ qua là đúng. |
| m2 | nhỏ | Danh sách khớp có thể trễ ngay sau khi mở. | Execution đọc lại tới khi tổng khớp đủ; portal dùng phí đã lưu. |
| m3 | nhỏ | Id cắt thêm một lần tra mỗi chân mỗi ý định. | Nợ có tên (§8.3 câu 4). |
| m4 | nhỏ | Binance mainnet có phí: execution gộp phí nhưng portal cộng gộp. | Portal dùng phí lưu trên MỌI sàn, nên hai bên cùng một nguồn; §8.5 mục 4. |
| m5 | nhỏ | Coin mua thêm và bụi không có trên trang lãi/lỗ. | Nợ có tên (§8.5 mục 6). |
| m6 | nhỏ | Lệnh gỡ ghi đè `UnwoundQtyCoin` của lệnh cắt. | Giữ nguyên: bằng chứng phẳng B so với đúng số đó, và đổi nó làm lệch hành vi Binance đã nghiệm thu. |

**Review phần 3 (ngữ cảnh sạch, kiểm các sửa trên): 1 chặn, 2 lớn, 3 nhỏ.** B1, B2, M2 được xác nhận đã
đóng ở đường mở; M1 đóng ở đường mở.

| # | Mức | Phát hiện | Xử lý |
|---|---|---|---|
| N1 | chặn | Adapter Bybit trả `ErrOrderNotVisible` cho id chưa từng gửi; portal đọc nó là ẩn số → mọi ý định Bybit đang giữ không đọc được, không đóng được. Test không bắt được vì sàn giả trả `ErrOrderNotFound`. | Quy tắc id vắng sau 30 s của portal (§8.1); sàn giả có chế độ trả `ErrOrderNotVisible`; M18–M20. |
| M-a | lớn | Gỡ sau khớp maker (phí khai 0) báo `ErrFlatEvidenceConflict` giả. | Bằng chứng phẳng lấy đúng số gỡ bán khi phí thu bằng coin; M21. |
| M-b | lớn | Phí không khai kịp lúc mở → đọc lại lần khớp mỗi lần → sau 7 ngày ẩn số. | Nợ có tên (§8.5 mục 6). |
| m1 | nhỏ | Lệnh gỡ đọc ví một lần. | Nợ có tên (§8.5 mục 7). |
| m2 | nhỏ | Phí có thể bị đánh dấu "đã khai" trên lần khớp chưa cuối ở đường báo động. | Chấp nhận: sai lệch tối đa phí × phần khớp thêm, chỉ trên đường đã báo động. |
| m3 | nhỏ | Thời gian mở bị chặn trên nhưng có thể dài (5 s + 1–2 lệnh gọi 20 s) dưới khoá ghi; gỡ khi chân 1 thiếu có thể chờ đọc lần khớp tới 5 s trước khi bán. | Nợ có tên: cửa sổ trần của đường gỡ dài thêm tối đa `OrderSettleTimeout`. |

**Review phần 4 (ngữ cảnh sạch, kiểm N1 và M-a): M-a đã đóng; N1 đóng cho ý định dưới 7 ngày; 2 lớn mới.**

| # | Mức | Phát hiện | Xử lý |
|---|---|---|---|
| R1 | lớn | Quy tắc "vắng" áp cả cho lệnh MỞ đã khớp. `/v5/order/history` được hỏi không kèm khung thời gian — mặc định 7 ngày — nên sau 7 ngày lệnh mở có thể không thấy → chân đọc là 0 → trang báo phẳng hoặc LÀM PHẲNG mua lại perp khỏi một spot long thật. | Lệnh MỞ không bao giờ đọc là vắng: không thấy → ẩn số (M22). Việc lịch sử theo `orderLinkId` có vượt 7 ngày không vẫn phải đo trên sàn (§8.5 mục 8). |
| R2 | lớn | Lượt đọc lại ngay sau lệnh cân chạy trong lúc giữ khoá ghi → mọi id chưa gửi là ẩn số → mọi lần cân thành công báo "VẪN LỆCH". | Lượt đọc đó coi các id khác của ý định là có trước lệnh ghi và bắt buộc thấy lệnh cân vừa gửi (M23); chạy thử reconcile không còn đóng dấu thời điểm ghi. |
| R3 | nhỏ | Tiến trình chết giữa lúc gửi và lúc lưu file, khởi động lại trong 30 s: lệnh đang bay có thể đọc là vắng. | Chấp nhận có tên: tạo lệnh tính bằng mili giây, khởi động lại tính bằng giây. |

Sau review phần 4 không chạy thêm vòng review nào; các sửa R1 và R2 được kiểm bằng test và đột biến.

### 8.7 Hướng dẫn nghiệm thu cho người vận hành

```bash
# Từ gốc repo. .env có BYBIT_API_KEY / BYBIT_API_SECRET / BYBIT_MODE=testnet.
go run ./cmd/bybitcheck                                   # phải 19/19, không lệnh nào
go run ./cmd/execportal -broker bybit -port 8088 -autotrade=false
# → http://127.0.0.1:8088
# Trên Bybit bot bị chặn (-autotrade và nút bật bot đều bị từ chối); -autotrade=false chỉ để log sạch.
```

1. Tab Thực thi thủ công: mở BTCUSDT cỡ nhỏ nhất (perp tối thiểu 0,001 BTC ≈ 77 USDT).
2. Kiểm:
   - lệnh spot = `CeilToStep(Q ÷ 0,999)` (0,001 → 0,001002);
   - `outcome both_open`, trạng thái HEDGED, phần dư < 0,001;
   - không có báo động.
3. `cat .paper/exec-bybit/<id>.json` — `spot_buy_base_fee_qty_coin` và `spot_buy_base_fee_stated: true`.
4. `curl -s -H 'X-Execportal-Action: read' 'http://127.0.0.1:8088/api/positions?symbol=BTCUSDT'` —
   dòng "mở BUY … − phí coin gốc … = ví nhận …".
5. **Chờ 30 s sau khi mở** (trước đó cặp là CHƯA XÁC ĐỊNH và trang từ chối đóng — §8.1 lệch 8), rồi
   đóng qua trang: `both_flat`; ví BTC dư < 0,000001 so với trước khi mở.
6. Nếu có báo động `ErrSpotEvidenceConflict`: KHÔNG làm phẳng vội. Đọc `SpotHeldSourceVI` (lần khớp và
   ví), đối chiếu trên giao diện Bybit testnet, rồi quyết định bằng tay.
7. Dừng portal (Ctrl+C). Không chạm 8085 / 8086 / 8087.
8. Khi các bước trên đạt: xoá `AutotradeBlockedVI` trong `profileFor` (một dòng), rồi review và commit
   riêng.
