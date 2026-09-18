# Nghiệm thu Bước 4.5l — Động cơ 2 lên portal và lệnh thật chéo sàn

**Ngày:** 2026-09-18 · **Cổng thử nghiệm:** 127.0.0.1:**8089** · **Sàn:** `demo-fapi.binance.com` (USDⓈ-M)
⟷ `api-testnet.bybit.com` (linear) · **Nền:** commit `18adff4` (bước 4.5k) cộng các thay đổi chưa commit của
bước này.

> Tài liệu này là **bản ghi của một lần đo**, không phải đặc tả. Đặc tả là
> [`docs/designs/cross-perp-engine-design.md`](../designs/cross-perp-engine-design.md); trạng thái và các
> giới hạn nằm ở [PLAN bước 4.5l](../PLAN.md).

---

## 0. Ràng buộc đã giữ

| Ràng buộc | Kết quả |
|---|---|
| Không dừng / kill / gửi tín hiệu tới 8085, 8086, 8087 | Giữ nguyên PID suốt phiên: scanner 84279, paperledger 84423, execportal 5253 |
| 8088 nếu bận thì dùng cổng khác | 8088 có `execportal` PID 50390 đang chạy → dùng **8089** |
| Chỉ host testnet/demo | `broker.NewClient` từ chối mọi host ngoài danh sách; portal không có cờ nào đổi được host |
| `go test -count=1 -race ./...` xanh 100% | 40/40 gói có test ĐẠT · 0 FAIL · 4 gói không có test · **0 DATA RACE** · exit 0 |
| Q21 — không lệnh reduce-only "thăm dò" | `crossStopReasonVI` dừng TOÀN BỘ tự động, giữ khóa; 7 test theo giá trị |
| Q22 — chỉ nhả khóa khi vị thế thật = 0 trên CẢ HAI sàn | `coordinator.Release` đọc lại `GetPosition` **và** lệnh treo mọi sàn trước khi ghi `idle` |

Tiến trình 8089 là do chính phiên này khởi động, dừng và khởi động lại — không phải một trong bốn cổng được
bảo vệ.

---

## 1. Đối soát lúc khởi động — bộ khóa dựng lại từ SÀN, không từ tệp

Tệp `.paper/coordinator-locks.json` chưa tồn tại, nên bảng khóa bắt đầu rỗng và
`ReconcileActivePositions` đọc vị thế + lệnh treo của **cả hai sàn** cho từng symbol trong vũ trụ 13 cặp:

```
khóa AAVEUSDT = occupied chủ engine_1_cash_and_carry
   (không có bản ghi khóa; SUY RA từ hình dạng vị thế sàn (short_only))
khóa BTCUSDT  = occupied chủ engine_1_cash_and_carry   … (giống trên)
khóa DOGEUSDT = occupied … ETHUSDT … LINKUSDT … LTCUSDT … SOLUSDT … SUIUSDT … UNIUSDT
```

**9 symbol** được gán cho Động cơ 1 — đúng các vị thế live của portal 8087, `source: reconciled_inferred`.
`unverified: none`. Đây là cái giữ cho Động cơ 2 không mở ngược Động cơ 1 khi tệp khóa mất hoặc khi tiến
trình kia không hề biết tới bộ khóa.

Van ký quỹ đọc được ngay lượt đầu:

```
binance_futures  unknown → green  0.2989%  SUY RA: totalMaintMargin ÷ totalMarginBalance (GET /fapi/v3/account)
bybit_linear     unknown → green  0.0000%  SÀN CÔNG BỐ: accountMMRate (GET /v5/account/wallet-balance,
                                            REGULAR_MARGIN) (đối chiếu hai tổng: 0.000000, trong 2 điểm %)
ngưỡng: vàng 0.50 · cam 0.60 · đỏ 0.65 · nhả 0.50
```

Và mặc định an toàn hoạt động:

```
execportal: auto-trader Động cơ 1 KHÔNG tự bật vì -crossperp đang bật
execportal: … phi công chỉ TƯ VẤN — đo và hiển thị, không gửi lệnh nào
```

---

## 2. BTCUSDT bị bộ khóa TỪ CHỐI (không lệnh nào được gửi)

```
POST /api/crossperp/open  {"symbol":"BTCUSDT","long_venue":"bybit_linear",
                           "short_venue":"binance_futures","qty_coin":0.001, …}
→ HTTP 409
  outcome                : both_flat
  lock_refused           : true
  refused_before_placing : true
  long  client_order_id  : null          ← không sinh id nào
  short client_order_id  : null
  error_vi               : coordinator: symbol đang BẬN: BTCUSDT đang thuộc
                           engine_1_cash_and_carry (ý định "", reconciled_inferred)
```

Đây là lý do cặp nghiệm thu đổi sang ADAUSDT: mở chân Động cơ 2 trên BTCUSDT sẽ bù trừ vị thế short live của
8087 trên tài khoản một chiều.

---

## 3. MỞ cặp thật `ADAUSDT` — long Bybit / short Binance

```
POST /api/crossperp/open {"symbol":"ADAUSDT","long_venue":"bybit_linear",
                          "short_venue":"binance_futures","notional_quote":77, …}
→ HTTP 200, 2.243 s toàn bộ lời gọi HTTP

intent_id             xadausdt-20260918-030552-036
outcome               both_open · hedged true · alarm false
target_qty_coin       360      common_step_coin 1      tolerance 1
DELTA |long-short|    0 coin
CỬA SỔ KHÔNG PHÒNG HỘ 57 ms
elapsed (máy)         1074 ms   · sổ cũ 1006 ms · chi phí vào 0.0964%

long  bybit_linear     BUY  cp1lo0b5ae228b63b56c1a4e0467f
      FILLED · đã xác nhận · 360 @ 0.2141 (trần 0.2143) · vị thế sàn +360 (đọc được)
short binance_futures  SELL cp1so73d63636da53914a8a1efe27
      FILLED · đã xác nhận · 360 @ 0.2137 (sàn 0.2135) · vị thế sàn −360 (đọc được)

pending_orders        không có
evidence_vi           LỆNH: long 360 (FILLED, đã kết thúc), short 360 (FILLED, đã kết thúc);
                      SÀN: bybit_linear +360, binance_futures −360
```

Dòng thời gian của chính cỗ máy (đồng hồ máy này, không phải đồng hồ sàn):

```
10:05:53.312  intent_received / sized 360 coin — bước chung 1
10:05:53.643  [long]  leg_placing  bybit_linear
10:05:53.643  [short] leg_placing  binance_futures     ← gửi SONG SONG
10:05:53.785  [short] leg_placed
10:05:53.849  [long]  leg_placed
10:05:54.067  [short] leg_filled                        ┐
10:05:54.124  [long]  leg_filled                        ┘ 57 ms không phòng hộ
10:05:54.276  resolved
```

**Dấu "sắp gửi lệnh" ghi trước lệnh đầu tiên:** khóa lấy lúc `…753302`, `orders_sent_at_ms = …753632`,
`leg_placing` đầu tiên lúc `…753643` — **cách 11 ms**. Khóa sau khi mở:

```json
{ "symbol": "ADAUSDT", "state": "occupied", "owner_engine": "engine_2_cross_perp",
  "intent_id": "xadausdt-20260918-030552-036", "source": "acquired",
  "venues": ["bybit_linear", "binance_futures"], "orders_sent_at_ms": 1789700753632,
  "details": "Động cơ 2: long bybit_linear / short binance_futures, 77.00 quote mỗi chân" }
```

Lệnh treo trên Binance ngay sau khi mở: **0 perp, 0 spot**.

---

## 4. Động cơ 1 bị chặn trên chính symbol đó

```
POST /api/open {"symbol":"ADAUSDT","notional_quote":20}
→ HTTP 409 · refused_before_placing true · không sinh ClientOrderID nào
  error_vi: sàn đang giữ vị thế perp −360.00000000 coin trên ADAUSDT — portal chỉ giữ
            MỘT vị thế mỗi symbol; đóng hoặc làm phẳng nó trước — chưa gửi lệnh nào
```

**Nói thẳng:** chặn được, nhưng chặn bởi chốt "một vị thế mỗi symbol" có sẵn từ 4.4b, **không phải** bởi bộ
khóa — chốt đó đọc vị thế sàn và nằm TRƯỚC bộ khóa trong trình tự của `openAs`. Trên sàn CHUNG thì hai chốt
độc lập đều chặn. Đường bộ khóa được chứng minh riêng ở `TestEngine1_IsRefusedWhileEngine2HoldsTheSymbol`
(Động cơ 2 giữ khóa → `acquireEngine1` trả lời "BẬN" và không sàn nào nhận thêm lệnh nào).

---

## 5. ĐÓNG tuần tự và nhả khóa

```
POST /api/crossperp/close {"symbol":"ADAUSDT","first_venue":"binance_futures", …}
→ HTTP 200, 2.214 s

outcome                both_flat · already_flat false · refused false · alarm false
chân đóng trước        short / binance_futures          ← sàn căng hơn (MMR 0.2989% vs 0.0000%)
trước khi đóng         long +360 · short −360
LỆNH TREO CUỐI CÙNG    0        ← đọc OpenOrders của CẢ HAI sàn; −1 nghĩa là không đọc được
opening_orders_proven  true
CỬA SỔ KHÔNG PHÒNG HỘ  759 ms   · 1 vòng · 2211 ms
KHÓA                   ĐÃ NHẢ — symbol về IDLE
long   bybit_linear     đóng 360 · vị thế sàn 0 (đọc được)
short  binance_futures  đóng 360 · vị thế sàn 0 (đọc được)
pending_orders         không có
```

```
10:07:20.306  close_started
10:07:20.561  [short] close_leg binance_futures · vòng 1 · sàn giữ −360
10:07:21.204  [long]  close_leg bybit_linear    · vòng 1 · sàn giữ +360   ← TUẦN TỰ, chân kia theo số THỰC
10:07:22.252  close_done — 1.947 s, 1 vòng, chân short trước, lệch cỡ 759 ms:
              bybit_linear +0, binance_futures +0
```

**759 ms là cái giá của việc đóng tuần tự** — `close.go` chọn nó có chủ ý: đóng song song để lại một chân
trần mỗi khi sàn hỏng là chân nào cũng được, còn tuần tự thì chỉ sàn THỨ HAI hỏng mới để lại một chân, và
khi đó nó báo động.

---

## 6. Đối soát độc lập sau nghiệm thu

```
POST /api/crossperp/reconcile    (đọc lại vị thế VÀ lệnh treo của cả hai sàn, 13 symbol)
→ occupied: AAVEUSDT, BTCUSDT, DOGEUSDT, ETHUSDT, LINKUSDT, LTCUSDT, SOLUSDT, SUIUSDT, UNIUSDT
  ADAUSDT còn bận?  KHÔNG
  unverified:       không có

.paper/coordinator-locks.json → ADAUSDT không có trong tệp
GET /api/crossperp/status     → 0 cặp còn giữ · 0 khóa treo
GET 8087 /api/autotrade/status → 9 cặp in_position, cùng PID
```

`ReconcileActivePositions` chỉ ghi `idle` khi **mọi sàn phẳng VÀ không lệnh treo**
(`reconcile.go:232`, đọc `OpenOrders` ở `reconcile.go:313`). Một symbol phẳng mà còn lệnh treo sẽ thành
CHƯA XÁC MINH, không thành rảnh. Nên việc ADAUSDT trở về rảnh **chính là** bằng chứng `OpenOrders` = 0 trên
cả hai sàn.

---

## 7. Phi công CHỈ TƯ VẤN — một lượt quét thật

Đọc funding đã settle của **chính hai testnet**, đo nhịp settle của riêng từng sàn rồi mới niên hóa và trừ:

```
ETHUSDT   L=binance S=bybit   gross/nam  160.25%  round-trip 0.201%  sau chi phí/vốn  149.79%  occupied
UNIUSDT   L=bybit   S=binance gross/nam   73.74%  round-trip 0.228%  sau chi phí/vốn   61.85%  occupied
SOLUSDT   L=bybit   S=binance gross/nam   38.29%  round-trip 0.219%  sau chi phí/vốn   26.86%  occupied
BNBUSDT   L=binance S=bybit   gross/nam   26.11%  round-trip 0.218%  sau chi phí/vốn   14.74%  idle
BTCUSDT   L=binance S=bybit   gross/nam    8.74%  round-trip 0.191%  sau chi phí/vốn   −1.24%  occupied
ADAUSDT   L=bybit   S=binance gross/nam   11.02%  round-trip 0.283%  sau chi phí/vốn   −3.73%  idle
LINKUSDT  L=binance S=bybit   gross/nam    1.76%  round-trip 199.752% → −10413.89%            occupied

LTCUSDT   chưa có tín hiệu — bybit: tickers không có symbol linear "LTCUSDT"
AVAXUSDT  chưa có tín hiệu — bybit: tickers không có symbol linear "AVAXUSDT"
TRXUSDT   chưa có tín hiệu — bybit: sổ TRXUSDT có 6 bid và 0 ask, cần cả hai phía
```

**Không con số nào ở đây nói về thị trường thật.** Hai testnet là hai thị trường giả tách rời, nên chênh
160%/năm của ETHUSDT là hiện vật của testnet chứ không phải cơ hội; vòng khứ hồi 199,75% của LINKUSDT là hiện
vật của sổ lệnh mỏng. Cái bản ghi này chứng minh là **đường ống** chạy đúng: đo được nhịp settle riêng từng
sàn, từ chối khi không đo được, và không bao giờ biến "không đọc được" thành số 0.

Phi công **chưa từng gửi lệnh nào** — `-crossperp-pilot` chưa bao giờ được bật.

---

## 8. Một khiếm khuyết do chính lần nghiệm thu tìm ra

Lần mở ADAUSDT đầu tiên bị **từ chối trước khi gửi**:

```
HTTP 422 · outcome both_flat · không sinh id lệnh nào
crossperp: từ chối trước khi gửi lệnh nào: sổ đã rộng quá chi phí vào được phép:
chi phí vào giờ 0.1211% so với 0.0000% lúc sinh tín hiệu, rộng thêm 12.11 bps > 5.00 bps
```

`openPair` đặt `SignalEntryCostPct: 0` cho lệnh bấm tay — đúng, vì không có tín hiệu nào trước đó — nhưng để
nguyên dung sai 5 bps của executor, nên **mọi** sổ lệnh thật đều "rộng thêm" đúng bằng chi phí của chính nó.
Động cơ 1 đã có miễn trừ này từ 4.4b: `actions.go` đặt `MaxEntryCostWidenBps = +Inf` khi người gọi không nêu
chi phí tín hiệu.

Sửa bằng `crossperp.Intent.MaxEntryCostWidenBpsOverride *float64` — ghi đè **theo từng ý định**, `+Inf` nghĩa
là không kiểm, `NaN` bị từ chối (NaN so sánh nào cũng sai, nên nó sẽ cho lọt mọi sổ). Phi công **giữ nguyên**
chốt 5 bps, vì một tín hiệu đã định giá lối vào một phút trước thì có thứ thật để mà so. Kèm 4 test, một
trong số đó chứng minh ghi đè **siết** được chứ không chỉ nới.

---

## 9. Còn nợ

1. **Chưa có review ngữ cảnh sạch cho bước 4.5l.** (4.5k còn nợ vòng 4 từ trước.)
2. **Portal 8087 chạy binary cũ và không hỏi bộ khóa.** Cái bảo vệ nó là coordinator ĐỌC vị thế của nó —
   thu hẹp khe hở xuống bằng khoảng từ lúc đọc tới lúc gửi, không xuống 0.
3. **Cửa sổ 57 ms là MỘT lần đo**, một cặp, một cỡ, hai testnet — không phải phân phối, và testnet không
   gánh tải như mainnet.
4. **Luật chốt lời / cắt lỗ của phi công chưa được thi hành bởi lần chạy nào.** `exitReasonVI` mới cài luật
   đảo chiều chênh funding kèm sàn giữ tối thiểu; `TakeProfitOnCapitalFrac` và `StopLossOnCapitalFrac` có
   trong cấu hình nhưng chưa có đường đi tới.
5. **Chưa lưu ý định của Động cơ 2 ra đĩa** (nợ từ 4.5k): chỉ khóa được lưu, `Adopt` dựng lại cặp từ vị thế
   sàn sau khi khởi động lại.
6. **Chưa bấm tay trên trang thật.** Máy này không có Chrome; mọi thao tác nghiệm thu đi qua `curl` với đúng
   tường bảo vệ mà trang đi qua (header hành động, Content-Type, Host). Tab đã được phục vụ và tệp
   `js/crossperp.js` trả HTTP 200, nhưng **chưa ai nhìn bằng mắt**.
