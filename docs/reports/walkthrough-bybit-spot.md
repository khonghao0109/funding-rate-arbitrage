# Walkthrough — Động cơ 1 trên Bybit V5 UTA: chân Spot + portal `-broker=bybit` (Bước 4.5j, 2026-09-17)

> **Trạng thái: 🟡 ĐÃ LÀM PHẦN ĐỌC; PORTAL BYBIT ĐANG CHỈ ĐỌC.** Mọi đường đọc đã chạy thật trên
> `api-testnet.bybit.com`. Chưa có lệnh nào được gửi: ví testnet chưa có USDT, và sau review vòng 2
> portal `-broker=bybit` từ chối mọi lệnh ghi cho tới khi chân spot được mua gộp phí và được xét
> theo số dư ví ở mọi đường (§6, §7).
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
