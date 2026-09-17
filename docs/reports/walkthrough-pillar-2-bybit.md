# Trụ cột 2 (chéo sàn perp–perp) — Bước 4.5i, phần Bybit V5: báo cáo thực hiện

> **Ngày:** 2026-09-17 · **Trạng thái:** 🟡 client + công cụ kiểm tra đã xong, **chưa nghiệm thu**
> · **Phạm vi:** Giai đoạn 1 (client Bybit V5) và Giai đoạn 2 (`cmd/bybitcheck`) của yêu cầu.
> Giai đoạn 3 (máy thực thi chéo sàn) và 4 (radar chênh funding) **chưa làm** — xem §6.

---

## 1. Tóm tắt

| Hạng mục | Kết quả |
|---|---|
| Client `internal/broker/bybit` hiện thực `broker.Broker` (perp tuyến tính USDT, một chiều) | ✅ xong |
| Rào host: chỉ `api-testnet.bybit.com`, `api-demo.bybit.com`; production bị từ chối | ✅ có test hai chiều |
| `go test -count=1 -race ./internal/broker/bybit/...` | ✅ đạt, 0 data race |
| `go test -count=1 -race ./...` | ✅ **38/38 package đạt** |
| `go run ./cmd/bybitcheck` trên testnet thật | 🟡 **4/5 mục đạt** — ví USDT = 0 (chưa nhận Faucet) |
| Lệnh chéo sàn 10/10 (tiêu chí 3 của `BYBIT-BROKER-PLAN.md §7`) | ⛔ chưa làm |
| Kiểm chứng tiền đề "lệch funding 15–45% APR" | ❌ **không đúng** — trung vị 3,2% APR (§5) |

Không lệnh nào được đặt trong phiên này. Không key, secret hay số dư nào được in ra.

---

## 2. Mã đã tạo / sửa

### 2.1. `internal/broker` — mở rộng, không nhân bản

Không có HTTP client thứ hai. Mọi bức tường đã được chứng minh ở 4.1–4.5b (https, không
redirect, ghim host từng request, transport riêng, `Secret`, lọc thân lỗi, từ chối ký khi lệch
giờ ≥ recv_window) được dùng lại bằng cách thêm **một cách ký thứ hai**.

| Tệp | Thay đổi |
|---|---|
| `client.go` | `Config.Scheme`; `GetSignedV5`, `PostSignedV5JSON` (body marshal MỘT lần, ký đúng byte gửi); `requireScheme` chặn động từ của sàn kia; header Bybit đóng dấu thời gian SAU khi chờ ngân sách; `ScrubVenueText` cho `retMsg` đến trên HTTP 200 |
| `hosts.go` | `SigningScheme`; allow-list **theo scheme**; `TestnetHostsFor` |
| `clock.go` | Nhánh Bybit của `SyncClock` (đọc `result.timeNano`) |
| `bybit_hosts.go` *(mới)* | Hai host không-production kèm trích dẫn; `BybitRequestsPerMin = 600` (quy đổi thận trọng từ "600 request / 5 giây / IP") |
| `bybit_sign.go` *(mới)* | `SignBybitV5`, bốn header `X-BAPI-*` |
| `bybit_sign_test.go` *(mới)* | Vector HMAC tính bằng `openssl` (không tự khớp với chính mã); host hai chiều; chữ ký nằm ở header, URL sạch |
| `boundary_test.go`, `redirect_test.go` | Thêm `bybitcheck` vào danh sách lệnh được link broker; cập nhật danh sách trường `Config` đã ghim |

### 2.2. `internal/broker/bybit` *(mới)*

| Tệp | Nội dung |
|---|---|
| `bybit.go` | `Mode` (testnet/demo), `DefaultConfig`, `New`, phong bì `retCode/retMsg/result`, đọc số dạng chuỗi (`""` → 0, sai định dạng → lỗi) |
| `endpoints.go` | 10 endpoint V5, mỗi cái có URL tài liệu và giới hạn theo UID |
| `errors.go` | Bảng mã: 10002/10003/10004/10005/10006/10010/10028, 110001, 110008/110010 (chỉ khi huỷ), 110017, 110072, 110079, 110094; HTTP 401/403 |
| `order.go` | `PlaceOrder`, `CancelOrder`, `GetOrder`, `OpenOrders`, kiểm `orderLinkId` (≤ 36 ký tự, `[A-Za-z0-9_-]`, từ chối chứ không cắt) |
| `account.go` | `GetPosition`, `FetchWallet`, `GetBalance` (suy ra, có ghi rõ), `FetchAPIKeyInfo` |
| `market.go` | `FetchInstrument`, `FetchDepthBook` (500 mức), `FundingRateHistory`, `SyncClock` |
| `bybit_test.go` | Máy chủ V5 giả **kiểm chữ ký** mọi request |

### 2.3. `cmd/bybitcheck` *(mới)*

Chỉ GET: đồng hồ → ví UTA (hỏi rõ `coin=USDT` để số 0 hiện là 0) → vị thế một symbol → quyền
key. In host, vòng RTT, độ lệch, **tên** coin, "USDT có số dư: có/không", khối lượng vị thế có
dấu, tên quyền. Mã thoát: 0 đạt · 1 hỏng · 2 không có key. `BYBIT_MODE` mâu thuẫn với
`BYBIT_TESTNET` bị từ chối thay vì tự chọn host.

---

## 3. Những gì Bybit không nói — và thiết kế đã theo

Đọc từ nhánh **`master`** của `github.com/bybit-exchange/docs` ngày 2026-09-17 (nhánh `main` đã
cũ: thời gian cấm IP, nghĩa của 10009 và các trường bị bỏ khác với `master`).

1. **Đặt lệnh và huỷ lệnh chỉ là xác nhận bất đồng bộ** (`orderId`, `orderLinkId`).
   - `PlaceOrder` trả NEW, khớp 0 — không bao giờ coi xác nhận là đã khớp.
   - `CancelOrder` **chờ trạng thái kết thúc** (≤ 3 s, deadline bao cả thời gian request) và gửi
     huỷ lại tối đa 2 lần cho lệnh còn thấy đang sống.
   - Không xác nhận được → `ErrCancelNotConfirmed` (mơ hồ) kèm lần đọc cuối. Không bao giờ trả
     lệnh còn sống với lỗi nil.
2. **`/v5/order/realtime` giữ 500 lệnh đóng gần nhất, mất sau khi Bybit khởi động lại.**
   - `GetOrder` hỏi realtime rồi history.
   - Cả hai rỗng → **`ErrOrderNotVisible`, MƠ HỒ**, không bao giờ `ErrOrderNotFound`. Tạo lệnh là
     bất đồng bộ và history "may delay", nên một lệnh đã tới — và đã khớp — có thể chưa hiện.
     `internal/execution` đọc `ErrOrderNotFound` là "chưa khớp gì" và "gửi lại an toàn".
   - Danh sách có lệnh nhưng sai id cũng là mơ hồ.
3. **110072 (trùng `orderLinkId`) là bằng chứng lệnh ĐÃ tồn tại**, không phải từ chối.
   - `PlaceOrder` đọc lại và trả lệnh đó cùng lỗi.
   - Lỗi khi đọc lại ghi bằng `%v`, không bọc, để một 4xx lúc đọc lại không làm bên gọi tưởng sàn
     đã từ chối chắc chắn.
4. **110001 / 110008 / 110010 nghĩa là "không còn" chỉ khi huỷ**, và chỉ sau khi đọc lại thấy lệnh
   đã kết thúc. Một lệnh huỷ có thể tới trước khi lệnh tạo được xử lý. Nếu một lần huỷ trước đó đã
   được nhận, câu "không còn" ở lần gửi lại chỉ là lần huỷ đầu hoàn tất → thành công.
5. **`Cancelled` ở phái sinh có thể đã khớp một phần** — đọc `FilledQtyCoin`, không đọc trạng thái.
6. **Vị thế.**
   - Có symbol thì luôn có dòng → danh sách rỗng là lỗi, không phải phẳng.
   - `side` và `size` mâu thuẫn, hoặc `size` trống → lỗi.
   - Chế độ hedge → từ chối (không cộng hai phía thành một số).
   - Tương tự, `qty` hay `cumExecQty` trống ở lệnh là lỗi, không phải 0.
7. **Ví UTA không còn `free`, `availableToWithdraw` bị bỏ.**
   - `broker.Balance` là số **suy ra**: Locked = locked + totalOrderIM + totalPositionIM,
     Free = walletBalance − Locked, không chặn âm.
   - Số dư chi tiêu thật: `Wallet.TotalAvailableBalanceUSD` (đơn vị **USD**, như tài liệu ghi).
   - `accountMMRate` trống → `RatesPublished = false`, không đọc thành "an toàn".
8. **HTTP 403** ("access too frequent" → chờ ít nhất 10 phút) bật một **lệnh ngừng theo IP dùng
   chung cho mọi client Bybit trong tiến trình**.
   - Kiểm trước và sau khi chờ ngân sách; request bị từ chối ngay (`ErrIPCoolingDown`), không chờ.
   - Client dựng trên transport test có lệnh ngừng riêng.
9. **Demo phục vụ toàn bộ `/v5/market/*`** → quy tắc, sổ lệnh, đồng hồ đều đọc từ host giao dịch;
   không request nào cần `api.bybit.com`.
10. **Từ chối của Bybit là HTTP 200 + retCode, không phải 4xx** (ngoại lệ: 110017 và 110094 ánh xạ
    sang `ErrBelowMin*`). `definiteRejection` của execution đọc các từ chối khác thành mơ hồ. An
    toàn, nhưng là nợ (§4).

---

## 4. Kiểm thử và review

| Lệnh | Kết quả |
|---|---|
| `gofmt -l .` | rỗng |
| `go vet ./...` | sạch |
| `go test -count=1 -race ./internal/broker/...` | 4/4 package đạt |
| `go test -count=1 -race ./...` | **38/38 package đạt**, 0 FAIL, 0 data race |

**15 đột biến có chủ đích**, mỗi cái chạy test rồi hoàn nguyên; tất cả làm test đỏ. Một cái (bỏ
kiểm tra chế độ hedge) sống ở lần đầu vì hai dòng hedge bị chặn bởi phép đếm dòng; đã thêm ca một
dòng.

| Nhóm | Các đột biến |
|---|---|
| Chữ ký, mã lỗi, vị thế | đảo thứ tự chuỗi ký; 110008 "không còn" ở mọi call; nhận chế độ hedge; danh sách sai id đọc thành không tìm thấy |
| Bản sửa vòng 1 | hai danh sách rỗng → không tìm thấy; huỷ trả lần đọc đầu; bỏ lệnh ngừng 403; `size` trống đọc thành 0; 110072 bỏ lệnh đã có |
| Bản sửa vòng 2 | lỗi đọc lại bọc `%w`; không gửi huỷ lại; 110001 không ánh xạ khi huỷ; client test dùng chung lệnh ngừng |
| Bản sửa vòng 3 | bỏ cờ "đã nhận huỷ"; gửi huỷ lại không giới hạn |

Máy chủ V5 giả kiểm chữ ký mọi request riêng tư. Nó cũng báo lỗi nếu một route riêng tư tới mà
không ký, hoặc một route công khai mang API key.

**Ba vòng review ngữ cảnh sạch:**

| Vòng | Kết luận | Phát hiện | Xử lý |
|---|---|---|---|
| 1 | Yêu cầu sửa | **Chặn:** hai danh sách rỗng trả `ErrOrderNotFound` → execution có thể coi chân đã khớp là chưa khớp, hoặc gửi lại tới khi bỏ cuộc trong khi lệnh tồn tại. **Lớn:** huỷ bất đồng bộ trả như cuối cùng; chuỗi rỗng đọc thành 0 ở `size`/`qty`/`cumExecQty`; 403 không được thực thi; `TestnetHosts()` thành hợp hai sàn làm yếu guard Binance; 110001 khi huỷ trước khi tạo xong. 8 nhỏ | Sửa hết, có test |
| 2 | Yêu cầu sửa, không chặn | **Lớn:** lỗi đọc lại sau 110072 bọc `%w` (do chính bản sửa vòng 1); huỷ không gửi lại cho lệnh còn sống. **Nhỏ:** lỗi giữa vòng chờ bỏ lần đọc cuối; deadline không bao thời gian request; 110001 ánh xạ cả khi đọc; tài liệu gói cũ; lệnh ngừng 403 theo client thay vì theo IP | Sửa hết, có test |
| 3 | **ĐẠT** | **Nhỏ:** lệnh ngừng không kiểm lại sau khi chờ ngân sách; lần huỷ lại trả "không còn" sau một huỷ đã được nhận; lỗi của lần huỷ lại bị mất khỏi thông điệp. Thiếu test giới hạn gửi lại | Sửa cả ba, thêm test |

**Nợ của `internal/execution` trước khi nối Bybit** (review vòng 2, ngoài phạm vi broker):
1. Đọc lại sau huỷ (`open.go`) và `settleOrder` (`close.go`) coi một lệnh chưa kết thúc là lần khớp
   cuối. Đúng với Binance, sai với Bybit.
2. `definiteRejection` chỉ biết 4xx.
3. Một lần gỡ gặp `ErrIPCoolingDown` phải dừng bảo vệ và báo, không thử lại.

**Chạy thật trên Bybit testnet** (`BYBIT_MODE=testnet`, ba lần):

| Mục | Kết quả |
|---|---|
| Đồng hồ `/v5/market/time` | HTTP 200 · vòng 168–321 ms · lệch **64–136 ms** (< 500 ms) |
| Ví `/v5/account/wallet-balance` | HTTP 200 retCode 0 · dòng USDT có mặt · **equity = 0** → HỎNG |
| Vị thế BTCUSDT `/v5/position/list` | HTTP 200 retCode 0 · một chiều · 0 coin |
| Quyền key: giao dịch hợp đồng | ĐẠT — không chỉ đọc, `ContractTrade[Order, Position]`, UTA |
| Quyền key: không rút tiền | ĐẠT — `Wallet` không chứa `Withdraw` |

Chữ ký, đồng hồ và key đều được sàn chấp nhận. Mục hỏng duy nhất là tài khoản testnet chưa có
tiền: lần đầu hỏi không kèm `coin`, sàn trả **0 coin** ("nếu không truyền, chỉ trả tài sản khác
0"), nên `cmd/bybitcheck` giờ hỏi rõ `coin=USDT`.

---

## 5. Đo tiền đề kinh tế trước khi xây tiếp

`BYBIT-BROKER-PLAN.md §1.1` (bản trước) viết: *"thống kê 3 năm cho thấy lệch Funding Rate
Binance–Bybit thường xuyên đạt 15–45% APR"*. Đã đo trên **bản sao chỉ đọc** của corpus 3 năm.

**Cách tính:**
- Tổng funding đã settle theo (sàn, coin, ngày UTC).
- Chỉ giữ ngày đủ mốc ở cả hai sàn; cadence đo từ dữ liệu, không giả định.
- d = Binance − Bybit, APR = d × 365, trên notional.
- Nguồn: `binance_futures`, `bybit_futures`.

**Độ lớn của chênh:**

| Phạm vi | coin-ngày | trung vị | trung bình | P95 | ≥ 15% | ≥ 45% |
|---|---|---|---|---|---|---|
| 36 coin, toàn bộ | 21.728 | 3,2% | 5,0% | 14,9% | 4,8% | 0,2% |
| 12 coin phủ 3 năm, 2023-24 | 4.380 | 2,7% | 4,6% | 15,8% | 5,7% | 0,1% |
| 12 coin phủ 3 năm, 2025-26 | 4.380 | 2,9% | 4,0% | 9,7% | 0,8% | 0,2% |

**Độ bền và độ hồi:**
- Chuỗi ngày ≥ 15%: trung vị **1 ngày**, P90 2 ngày.
- Ngày tín hiệu trung bình 26% APR.
- Cùng chiều sau đó: ngày kế trả 9,9%, 7 ngày sau 5,7%, **30 ngày sau 3,5%**.
- 21,7% trường hợp 30 ngày sau là âm.

**Phí:**
- Một vòng 4 lệnh taker = **21 bps** (phí đã xác minh: Binance 5,0, Bybit 5,5).
- Ở mức 30 ngày sau, hoà vốn trung vị ~22 ngày, trước trượt giá.
- Năm 2025-26 trên 12 coin: ~136 ngày.

**Kết luận:** câu "15–45% APR thường xuyên" **không đúng cho bất kỳ coin nào**. Nó chỉ gần đúng
ở dạng "có một coin nào đó hôm nay" (70% số ngày trên 36 coin năm gần nhất), nhưng hầu như không
coin nào giữ mức đó hai ngày liền. Câu trong `BYBIT-BROKER-PLAN.md` đã được sửa.

**Đổi sang vốn:** perp–perp đòn bẩy K ký quỹ trên hai sàn, vốn ≈ 2N/K. Lợi suất trên vốn = **K/2**
× lợi suất trên notional: ×1 ở 2×, ×1,5 ở 3×, không phải ×K.

**Chưa trừ:** trượt giá (các coin chênh rộng nhất lại có sổ mỏng nhất), rủi ro thanh lý hai
chân, chuyển ký quỹ giữa hai sàn, và chi phí vốn. Danh sách coin là danh sách đã sàng lọc của dự
án, nên kết quả nằm trong mẫu.

---

## 6. Vì sao Giai đoạn 3–4 chưa làm, và đặc tả cần sửa gì

Giai đoạn 3 sửa `internal/execution`, nơi giữ bất biến "hai chân cùng mở hoặc cùng phẳng" đã
qua nhiều vòng review. Đặc tả hiện có những chỗ **mâu thuẫn với bất biến, với tài liệu sàn hoặc
với số đo**. Các quyết định dưới đây là của người vận hành:

1. **Hiệu quả vốn là K/2, không phải K.** Ký quỹ nằm trên hai sàn.
2. **"Không chịu rủi ro biến động giá" là sai.** Hai tài khoản riêng không bù lãi/lỗ cho nhau.
   Một biến động khoảng 1/K thanh lý chân đang lỗ, trong khi chân đang lãi nằm ở sàn kia.
3. **Binance `/fapi/v3/account` không có `totalMarginRatio`** (luật 5). Tỷ lệ phải suy ra từ
   `totalMaintMargin` / `totalMarginBalance` và ghi rõ là suy ra. `accountMMRate` của Bybit
   "không áp dụng cho isolated margin".
4. **Bybit không có `canTrade` / `canWithdraw`.** Có `readOnly` và mảng `permissions`, trong đó
   `Wallet` chứa `Withdraw`. Client đã dùng đúng các trường này.
5. **"Chân 1 khớp một phần → dừng, không mở Chân 2" vi phạm bất biến.** Phần đã khớp phải được
   phòng hộ bằng Chân 2 hoặc gỡ ngay.
6. **"Gỡ < 300 ms" chéo sàn là con số phải đo, không phải hứa.** Gỡ cùng sàn Binance đo được
   225–284 ms. Chéo sàn còn cộng độ trễ tới Bybit và việc đọc lại một lệnh bất đồng bộ.
7. **`definiteRejection`** cần một sentinel "sàn đã từ chối" để lỗi retCode của Bybit không bị đọc
   thành mơ hồ (§3.8).
8. **Giới hạn theo UID/endpoint của Bybit chưa được đo đếm** (order/create 10/s linear).
9. **Radar ≥ 15% APR (Giai đoạn 4):** theo §5 nó sẽ bật khoảng 4,8% số coin-ngày và tín hiệu
   hồi trong một ngày. Nếu vẫn làm, hãy xếp hạng theo **chênh trượt 7–30 ngày trừ 21 bps**, không
   theo mức tức thời.

---

## 7. Lưu ý vận hành

- **Nạp tiền testnet:** vào testnet.bybit.com → Faucet, rồi chạy `go run ./cmd/bybitcheck`. Mục
  tiêu là 5/5, mã thoát 0.
- **Demo thay vì testnet:** tạo key **trong chế độ Demo Trading** trên trang mainnet, đặt
  `BYBIT_MODE=demo` và `BYBIT_TESTNET=false`. Key testnet trên host demo sẽ trả 10003.
- **HTTP 403** ("access too frequent"): dừng ít nhất 10 phút, không chạy lại ngay.
- **Tài khoản phải là UTA, chế độ một chiều** (mặc định). Chế độ hedge bị từ chối khi đọc vị thế.
- Nhị phân cổng 3.5 (`cmd/scanner`) **không link** `internal/broker`. `boundary_test.go` kiểm bằng
  `go list -deps`; `bybitcheck` là lệnh thứ tư được miễn.
