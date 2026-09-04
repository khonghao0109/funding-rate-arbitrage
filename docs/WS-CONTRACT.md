# HỢP ĐỒNG WEBSOCKET — Backend ↔ Dashboard

> **Chốt:** Bước 1.0 · **Phiên bản:** `v: 1`
> **Phạm vi hiệu lực:** toàn bộ Giai đoạn 1 (Bước 1.0 → 1.6), mở rộng ở Bước 2.7a
> theo đúng luật ở mục 8 (thêm message type và trường mới có mặc định, `v` giữ nguyên)
> **Liên quan:** [PLAN.md](PLAN.md) · [CONVENTIONS.md](CONVENTIONS.md#1-quy-tắc-quan-trọng-nhất-của-dự-án-này-hậu-tố-đơn-vị)

---

## 0. VÌ SAO CÓ TÀI LIỆU NÀY

Ba bước 1.1, 1.2 và 1.3 đều đổi dữ liệu gửi cho frontend. `static/app.js` dài 925
dòng, hardcode danh sách nguồn ở 5 chỗ và **không có test nào bảo vệ**. Sửa nó ba
lần liên tiếp là ba lần có nguy cơ vỡ dashboard.

Hợp đồng dưới đây được thiết kế **một lần cho cả giai đoạn**. Từ Bước 1.1 trở đi,
backend chỉ **điền dữ liệu thật** vào các trường đã khai báo sẵn — không đổi shape,
không thêm/bớt message type, không đổi tên trường.

**Quy tắc bất biến của hợp đồng này:**

| # | Quy tắc |
|---|---|
| 1 | Mọi trường mang đơn vị phải có hậu tố đơn vị trong tên — **kể cả trên wire** ([CONVENTIONS §1](CONVENTIONS.md)) |
| 2 | Không bao giờ có trường tên `profit_*`. Số chưa trừ phí gọi là `*_gross_pct` |
| 3 | Trạng thái dữ liệu do **backend** quyết định, frontend không tự tính. Đồng hồ trình duyệt lệch đồng hồ server |
| 4 | Trường chưa có dữ liệu gửi **giá trị mặc định đã định nghĩa**, không bỏ trường đi |
| 5 | Frontend **không được hardcode** danh sách nguồn/symbol — dựng từ message `meta` |

---

## 1. GIÁ TRỊ MẶC ĐỊNH THEO BƯỚC

Bảng này nói rõ trường nào có dữ liệu thật ở bước nào. Ô ⬜ nghĩa là trường đã tồn
tại trên wire nhưng đang mang giá trị mặc định.

| Trường | 1.0 | 1.1 | 1.2 | 1.3 | 1.4 | 1.5 |
|---|---|---|---|---|---|---|
| `prices[sym][src].price` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `prices[sym][src].venue_time_ms` | ⬜ `0` | ✅ | ✅ | ✅ | ✅ | ✅ |
| `prices[sym][src].recv_at_ms` | ⬜ `0` | ✅ | ✅ | ✅ | ✅ | ✅ |
| `prices[sym][src].status` | ⬜ `unknown` | ✅ | ✅ | ✅ | ✅ | ✅ |
| `prices[sym][src].age_ms` | ⬜ `-1` | ✅ | ✅ | ✅ | ✅ | ✅ |
| `prices[sym][src].best_bid` / `best_ask` | ⬜ `0` | ⬜ | ✅ | ✅ | ✅ | ✅ |
| `prices[sym][src].best_bid_qty_coin` / `best_ask_qty_coin` | ⬜ `0` | ⬜ | 🟡 5/9 nguồn | 🟡 | 🟡 | 🟡 |
| `source_status[src].state` | ⬜ `unknown` | 🟡 suy ra từ im lặng | 🟡 | 🟡 | 🟡 | ✅ connector tự báo |
| `source_status[src].reconnect_count` | ⬜ `0` | ⬜ | ⬜ | ⬜ | ⬜ | ✅ |
| `source_status[src].uptime_sec` | ⬜ `0` | ⬜ | ⬜ | ⬜ | ⬜ | ✅ |
| `meta.sources[].market_type` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `meta.sources[].tradable` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `meta.sources[].stale_after_sec` | ⬜ `10` | ✅ | ✅ | ✅ | ✅(yaml) | ✅ |
| `meta.sources[].taker_fee_bps` | ⬜ `0` | ⬜ | ⬜ | 🟡 5/9 sàn | ✅(yaml) | ✅ |
| `spreads.cross_venue_groups[]` | ⬜ 1 nhóm `all`, `tradable:false` | ⬜ | ✅ `perp_usdt`/`perp_usd`/`spot_usdt` | ✅ | ✅ | ✅ |
| `spreads.basis[]` | ⬜ `[]` | ⬜ | ✅ | ✅ | ✅ | ✅ |
| `spreads.oracle_deviation[]` | ⬜ `[]` | ⬜ | ✅ | ✅ | ✅ | ✅ |
| `*.spread_after_fees_pct` | ⬜ `null` | ⬜ | ⬜ | 🟡 chỉ cặp có cả hai sàn đã xác minh phí | ✅ | ✅ |
| `meta.cost_basis` | ⬜ `model:"none"` | ⬜ | ⬜ | ✅ `taker_round_trip` | ✅ | ✅ |
| `meta.symbols` | ✅ (Go) | ✅ | ✅ | ✅ | ✅(yaml) | ✅ |

🟡 = điền một phần. `best_*_qty_coin` chỉ có ở 5 nguồn báo bằng coin (binance ×2, bybit ×2, hyperliquid); OKX, Gate, Kraken báo bằng contract và Paradex không có size — bốn nguồn đó giữ `0`, nghĩa là **chưa biết**, không phải **không có thanh khoản**. Xem [PLAN §1.2](PLAN.md).

---

## 2. PHONG BÌ CHUNG

Mọi message đều có:

```json
{ "type": "<tên>", "v": 1, "server_time_ms": 1756368000000 }
```

| Trường | Kiểu | Ý nghĩa |
|---|---|---|
| `type` | string | `meta` \| `prices` \| `spreads` \| `arbitrage` |
| `v` | int | Phiên bản hợp đồng. FE cảnh báo nếu khác `1` |
| `server_time_ms` | int64 | Đồng hồ server lúc gửi. **Mọi phép tính tuổi dữ liệu phải mốc theo trường này**, không dùng `Date.now()` |

---

## 3. `meta` — gửi MỘT LẦN ngay khi client kết nối

Đây là message xoá bỏ 5 chỗ hardcode trong `app.js`.

```json
{
  "type": "meta",
  "v": 1,
  "server_time_ms": 1756368000000,
  "symbols": ["BTCUSDT", "ETHUSDT", "XRPUSDT", "SOLUSDT"],
  "default_symbol": "BTCUSDT",
  "alert_min_spread_pct": 0.05,
  "cost_basis": {
    "model": "taker_round_trip",
    "applied": ["taker_fee_entry", "taker_fee_exit"],
    "excluded": ["slippage", "funding", "withdrawal"],
    "note_vi": "Số đã trừ phí giao dịch: taker cả bốn lượt khớp — mở và đóng cả hai chân. …"
  },
  "sources": [
    {
      "source": "binance_futures",
      "venue": "binance",
      "market_type": "perp",
      "quote_asset": "USDT",
      "tradable": true,
      "label": "Binance Futures",
      "short_label": "BIN-F",
      "color": "#f0b90b",
      "line_style": "solid",
      "enabled_by_default": true,
      "stale_after_sec": 10,
      "maker_fee_bps": 2,
      "taker_fee_bps": 5,
      "fee_verified": true
    }
  ]
}
```

| Trường | Kiểu | Mặc định 1.0 | Ghi chú |
|---|---|---|---|
| `symbols` | []string | 4 cặp hiện tại | Bước 1.4 đọc từ `config.yaml` |
| `alert_min_spread_pct` | float | `0.05` | Ngưỡng lọc mặc định của bảng cảnh báo |
| `cost_basis.model` | string | `"none"` | `none` \| `taker_round_trip`. **Bước 1.3 đặt tên là `taker_round_trip` chứ không phải `taker_both_legs` như bản 1.0 dự kiến**: chi phí là **bốn** lượt khớp (mở và đóng cả hai chân), không phải hai. Không thoát được vị thế thì không hiện thực hoá được spread, nên tính một nửa số lượt khớp là nói thiếu đúng một nửa chi phí |
| `cost_basis.applied` | []string | `[]` | Chi phí ĐÃ trừ khỏi `*_after_fees_pct` |
| `cost_basis.excluded` | []string | cả 4 | Chi phí CHƯA trừ — FE **bắt buộc** hiển thị |
| `sources[].source` | string | — | Khoá wire, `venue + "_" + market_type` ([CONVENTIONS §2](CONVENTIONS.md)) |
| `sources[].venue` | string | — | `binance`, `bybit`, … |
| `sources[].market_type` | string | giá trị thật | `spot` \| `perp` \| `future` \| `oracle`. Là **dữ kiện tĩnh** của sàn nên điền đúng ngay từ 1.0; Bước 1.2 là lúc bắt đầu **dùng** nó để tách nhóm |
| `sources[].quote_asset` | string | `"USDT"`/`"USD"` | Kraken là `USD` — không so sánh chéo với USDT (Bước 1.2) |
| `sources[].tradable` | bool | giá trị thật | Pyth là `false` ngay từ 1.0. Bước 1.2 là lúc grouping bắt đầu dựa vào cờ này |
| `sources[].stale_after_sec` | int | 10–20 | Ngưỡng staleness **theo từng sàn**, **đo từ dữ liệu thật** (Bước 1.1): mỗi sàn dư ~3× so với khoảng cách cập nhật tệ nhất quan sát được. Xem comment trên `sourceRegistry` ở [wire.go](../wire.go) |
| `sources[].maker_fee_bps` / `taker_fee_bps` | float | `0` | **Bps, có phần thập phân.** Bản 1.0 khai là số nguyên; Bước 1.3 sửa vì biểu phí thật không nguyên (Hyperliquid maker 1,5 bps, Paradex maker 0,3 bps). JSON không phân biệt int/float nên shape không đổi. Xem [CONVENTIONS §1.2](CONVENTIONS.md) |
| `sources[].fee_verified` | bool | `false` | **Thêm ở Bước 1.3.** `true` = biểu phí đã đọc từ tài liệu của chính sàn và có trích dẫn trong `internal/fees`. `false` = **chưa tra được**, KHÔNG phải miễn phí — hai chuyện này đều để số ở `0`, và 4/9 sàn đang ở trường hợp sau. Cặp nào có một sàn `false` thì `spread_after_fees_pct` là `null` |

---

## 4. `prices` — snapshot định kỳ (ticker 200 ms)

Thay cho `map[symbol]map[source]float64` phẳng hiện tại.

```json
{
  "type": "prices",
  "v": 1,
  "server_time_ms": 1756368000000,
  "prices": {
    "BTCUSDT": {
      "binance_futures": {
        "price": 65012.35,
        "venue_time_ms": 0,
        "recv_at_ms": 0,
        "age_ms": -1,
        "status": "unknown",
        "best_bid": 0,
        "best_ask": 0,
        "best_bid_qty_coin": 0,
        "best_ask_qty_coin": 0
      }
    }
  },
  "source_status": {
    "binance_futures": {
      "state": "unknown",
      "last_msg_at_ms": 0,
      "reconnect_count": 0,
      "uptime_sec": 0
    }
  }
}
```

### 4.1. `prices[symbol][source]` — trạng thái mức DỮ LIỆU

| Trường | Kiểu | Mặc định 1.0 | Ghi chú |
|---|---|---|---|
| `price` | float | — | Mid-price `(bid+ask)/2` |
| `venue_time_ms` | int64 | `0` | Thời gian **sàn phát**. `0` = connector không đọc được mốc thời gian nào từ message đó. ✅ Bước 1.1 đã dọn xong: **không sàn nào còn điền đồng hồ nội bộ** — Bybit/Paradex/Kraken trước đây điền `time.Now()`, OKX/Gate rơi về `time.Now()` khi parse lỗi; nay tất cả để `0`. Chỉ dùng để chẩn đoán, **không bao giờ** để tính staleness |
| `recv_at_ms` | int64 | `0` | Thời điểm **bot nhận**, đặt tại **một chỗ duy nhất** (`updatePrice` trong [main.go](../main.go)). Đây là cơ sở **duy nhất** của staleness |
| `age_ms` | int64 | `-1` | `server_time_ms - recv_at_ms`. `-1` = chưa đo được |
| `status` | string | `live` \| `stale` | **Backend tính** từ `recv_at_ms`, FE chỉ hiển thị. `unknown` chỉ còn khi chưa từng nhận được gì |
| `best_bid` / `best_ask` | float | `0` | Đỉnh sổ. `0` = chưa biết. Bước 1.2 điền |
| `best_bid_qty_coin` / `best_ask_qty_coin` | float | `0` | Khối lượng đỉnh sổ, **tính theo coin không phải contract** — OKX/Gate/Kraken niêm yết theo contract, connector phải quy đổi trước khi điền ([CONVENTIONS §1.4](CONVENTIONS.md)). Đây là **bộ lọc thanh khoản bậc một**, không phải độ sâu: nó nói có bao nhiêu ở giá tốt nhất, không nói lệnh $60k khớp ở đâu. Độ sâu đầy đủ về qua REST ở Bước 2.7 ([PLAN §7.4](PLAN.md#74-chiến-lược-độ-sâu-sổ-lệnh)) |

> ⚠️ `venue_time_ms` **không bao giờ** được dùng để tính staleness. Nó chỉ để chẩn
> đoán độ trễ đường truyền. Lý do đầy đủ ở [PLAN.md Bước 1.1](PLAN.md).
>
> Đo thật lúc chạy Bước 1.1: `venue_time_ms` của Binance lớn hơn `recv_at_ms`
> **80ms** — đồng hồ sàn chạy trước đồng hồ ta. Lấy hiệu hai mốc đó ra sẽ đo
> **lệch đồng hồ**, không phải độ mới của dữ liệu.
>
> Từ Bước 1.1, **mọi nguồn đã đăng ký đều xuất hiện trong `source_status`**, kể cả
> nguồn chưa từng gửi gì. Bỏ nó đi khiến một sàn không bao giờ kết nối trông y hệt
> một sàn không tồn tại — đúng cách Pyth từng biến mất khỏi dashboard không dấu vết.
>
> Giá `stale` **vẫn nằm trong `prices`** kèm giá cuối cùng, chỉ bị loại khỏi phép
> so sánh. Xoá hẳn hàng đó khỏi snapshot sẽ khiến sàn chết biến mất khỏi màn hình,
> đọc thành "không có gì để báo" thay vì "feed này đã chết".

### 4.2. `source_status[source]` — trạng thái mức KẾT NỐI

Tách khỏi mục 4.1 vì đây là thuộc tính của **kết nối**, không của từng symbol: một
sàn có thể còn kết nối nhưng ngừng đẩy một cặp thanh khoản mỏng.

| Trường | Kiểu | Mặc định 1.0 | Ghi chú |
|---|---|---|---|
| `state` | string | giá trị thật | `connected` \| `reconnecting` \| `disconnected` \| `unknown`. **Từ Bước 1.5 connector tự báo** thay vì chỉ suy ra từ im lặng — nhưng KHÔNG thay thế: kết quả là **hợp** của hai nguồn (xem ghi chú dưới bảng) |
| `last_msg_at_ms` | int64 | giá trị thật | Message cuối nhận được từ sàn, **bất kể symbol và bất kể loại** (giá hay trade). Đây là thứ phân biệt "sàn chết" với "một cặp thanh khoản mỏng đang yên ắng" |
| `reconnect_count` | int | `0` | Số lần nối lại **kể từ lúc khởi động**, không tính lần kết nối đầu (Bước 1.5). Đây là con số trả lời "đêm qua sàn nào chập chờn" sau một phiên chạy không người trông |
| `uptime_sec` | int64 | `0` | Độ dài kết nối **hiện tại**, không cộng dồn qua các lần đứt (Bước 1.5). Bằng `0` khi `state` khác `connected`, để không có chuyện báo tuổi socket bên cạnh trạng thái mất kết nối |

**`state` được quyết bởi HAI nguồn, vì không nguồn nào đủ:**

- **Connector** biết thứ im lặng không biết: sàn có thể mất kết nối ngay một giây
  sau tick cuối, mà suy luận từ im lặng vẫn gọi nó khoẻ thêm 45 giây nữa. Chỉ
  connector báo được `reconnecting` — trạng thái này trước Bước 1.5 **chưa bao giờ
  xuất hiện trên wire**.
- **Im lặng** biết thứ connector không biết, và đây mới là lỗi hay lẩn: một
  subscription bị sàn âm thầm huỷ để lại socket **thật sự đang mở**, khoẻ theo mọi
  thước đo connector có, và không bao giờ đẩy thêm dữ liệu nữa.

Nên: connector báo `connected` **cộng** im lặng vượt ngưỡng mất-kết-nối → vẫn là
`disconnected`. Ngưỡng im lặng giữ nguyên như Bước 1.1: 3× `stale_after_sec`, tối
thiểu 45s, và 90s ân hạn khởi động cho sàn chưa từng gửi gì.

---

## 5. `spreads` — ba khối TÁCH BIỆT

Khối này tồn tại để thi hành Bước 1.2: **không trộn** spot với perp, và **không**
coi oracle là nơi giao dịch được.

```json
{
  "type": "spreads",
  "v": 1,
  "server_time_ms": 1756368000000,
  "symbol": "BTCUSDT",
  "cross_venue_groups": [
    {
      "group_id": "perp_usdt",
      "label_vi": "Perpetual · quote USDT",
      "market_type": "perp",
      "quote_asset": "USDT",
      "tradable": true,
      "note_vi": "",
      "sources": ["binance_futures", "bybit_futures"],
      "matrix": {
        "binance_futures": {
          "bybit_futures": { "spread_gross_pct": 0.031, "spread_after_fees_pct": null }
        }
      }
    }
  ],
  "basis": [],
  "oracle_deviation": [],
  "excluded_sources": [
    { "source": "pyth", "reason": "oracle", "note_vi": "Oracle, không giao dịch được …" }
  ]
}
```

### 5.1. `cross_venue_groups[]` — chênh lệch giữa hai VENUE cùng loại thị trường

Một nhóm = một tập nguồn **so sánh được với nhau**: cùng `market_type` **và** cùng
`quote_asset`. Ma trận chỉ được dựng **trong** một nhóm, không bao giờ xuyên nhóm.

| Trường | Kiểu | Mặc định 1.0 | Ghi chú |
|---|---|---|---|
| `group_id` | string | `"all"` | Từ 1.2: `perp_usdt`, `perp_usd`, `spot_usdt` — `<market_type>_<quote viết thường>` |
| `label_vi` | string | — | Tiêu đề hiển thị (tiếng Việt) |
| `market_type` | string | `"unknown"` | Loại thị trường chung của nhóm |
| `quote_asset` | string | `""` | Đồng quote chung |
| `tradable` | bool | `false` ở 1.0 | `false` → nhóm là **tham chiếu**: FE vẫn tô màu theo dấu của số, nhưng **không** gắn nhãn "cơ hội" cho ô nào, và backend **không sinh cảnh báo** từ nhóm đó. Từ 1.2 là `AND` cờ `tradable` của mọi thành viên: một nguồn không giao dịch được làm cả nhóm thành tham chiếu |
| `note_vi` | string | `""` | Cảnh báo riêng của nhóm. Nhóm quote khác USDT luôn có: *"Nhóm này quote bằng USD chứ không phải USDT — không so trực tiếp với nhóm USDT…"*. Trong một nhóm mọi nguồn đã cùng quote, nên ghi chú nói về việc **so giữa hai nhóm**, không phải trong nhóm |
| `sources` | []string | mọi nguồn | Thứ tự hiển thị, do backend quyết |
| `matrix[buy][sell]` | object | — | Xem 5.4 |

> **1.0 gửi đúng một nhóm `all` chứa toàn bộ nguồn**, đánh dấu `tradable: false`.
> **Từ 1.2 nhóm `all` không còn tồn tại**: mỗi nhóm là một cặp (`market_type`,
> `quote_asset`) và ma trận **chỉ** được dựng trong nhóm.
>
> Một nhóm cần **ít nhất hai nguồn**. Nguồn lẻ loi bị loại với lý do `no_peer` và
> **không có nhóm nào** được gửi cho nó — nên `cross_venue_groups: []` là trạng
> thái **bình thường**, không phải lỗi. FE không được coi mảng rỗng là "chưa có dữ
> liệu": `basis`, `oracle_deviation` và `excluded_sources` vẫn phải hiển thị.

### 5.2. `basis[]` — spot ↔ perp CÙNG MỘT SÀN

Nền tảng cho Giai đoạn 2. `[]` ở Bước 1.0.

```json
{
  "venue": "binance",
  "spot_source": "binance_spot",
  "perp_source": "binance_futures",
  "spot_price": 65000.10,
  "perp_price": 65012.35,
  "basis_abs_quote": 12.25,
  "basis_pct": 0.0188
}
```

`basis_abs_quote` tính bằng **đồng quote của nhóm** (`perp_price - spot_price`),
không phải USD — hai thứ này khác nhau khi quote là USD của Kraken.

### 5.3. `oracle_deviation[]` — Pyth ↔ sàn, CHỈ THAM CHIẾU

`[]` ở Bước 1.0, điền từ 1.2. Không bao giờ sinh cảnh báo từ khối này.

```json
{
  "oracle_source": "pyth",
  "source": "binance_futures",
  "deviation_pct": 0.004,
  "quote_asset_mismatch": true
}
```

`deviation_pct` = `(giá sàn - giá oracle) / giá oracle * 100`.

`quote_asset_mismatch` **thêm ở Bước 1.2**, mặc định `false`. Pyth quote bằng USD
còn 6/9 sàn quote bằng USDT, nên phần lớn dòng ở đây mang **cả chênh USD/USDT** chứ
không chỉ độ lệch của sàn. Khác `basis[]` — nơi cặp lệch quote bị **loại hẳn** vì
con số đó sẽ bị đọc nhầm là basis funding — khối này **giữ** dòng lệch quote, vì
đối chiếu sàn với oracle chính là mục đích của nó; nhưng phải gắn cờ, và dashboard
hiện nhãn "lệch quote". Trừ được chân USDT ra cần một giá tham chiếu USDT/USD, việc
của Giai đoạn 2.

Oracle cũ (stale) **không sinh dòng nào**: độ lệch đo với một giá đóng băng mô tả
một thị trường đã đi mất.

### 5.4. Ô ma trận

```json
{ "spread_gross_pct": 0.031, "spread_after_fees_pct": null }
```

| Trường | Kiểu | Mặc định 1.0 | Ghi chú |
|---|---|---|---|
| `spread_gross_pct` | float | — | `(sell - buy) / buy * 100`. **THÔ** — chưa trừ gì |
| `spread_after_fees_pct` | float\|null | `null` | Bước 1.3 điền: `spread_gross_pct` trừ phí taker của **bốn** lượt khớp. Vẫn `null` khi **một trong hai sàn chưa xác minh được biểu phí** — coi phí chưa biết là 0 sẽ đăng nguyên spread thô như thể bắt được nó không tốn gì. **Không phải lợi nhuận ròng** — chưa trừ slippage và funding |

> 🚩 Tên trường là `spread_*`, **không phải** `profit_*`, và có `_gross_`/`_after_fees_`
> tường minh. Đây là thi hành trực tiếp [CLAUDE.md luật 2](../CLAUDE.md).

### 5.5. `excluded_sources[]`

Nguồn có giá nhưng bị loại khỏi mọi nhóm, kèm lý do — để FE giải thích được vì sao
một sàn biến mất khỏi ma trận thay vì im lặng bỏ đi. `[]` ở Bước 1.0.

```json
{ "source": "pyth", "reason": "oracle", "note_vi": "Oracle, không giao dịch được" }
```

`reason`: `no_price` | `oracle` | `quote_mismatch` | `stale` | `disconnected` | `no_peer` | `unregistered`.

`no_price` được dùng từ Bước 1.0: nguồn gửi giá không dùng được (≤ 0, NaN, vô cực) bị loại khỏi ma trận và phải nói ra lý do, không được im lặng biến mất.

`stale` được dùng từ Bước 1.1: nguồn ngừng gửi quá ngưỡng của nó. Giá cuối **vẫn
nằm trong `prices`** kèm `status: "stale"` để dashboard hiển thị được sàn đó đang
chết — nó chỉ bị loại khỏi phép so sánh, không bị xoá khỏi màn hình.

Bước 1.2 thêm ba lý do:

| `reason` | Nghĩa |
|---|---|
| `oracle` | Nguồn là oracle. Không mua bán được ở đó nên nó **không bao giờ** vào một nhóm, dù dữ liệu mới đến đâu. Chỉ xuất hiện trong `oracle_deviation[]` |
| `no_peer` | Không có nguồn nào khác cùng `market_type` và cùng `quote_asset` để so. Một nguồn đứng một mình không phải ma trận một cột |
| `unregistered` | Nguồn không có trong registry: không biết loại thị trường lẫn đồng quote, nên không biết nó so được với cái gì. **Thêm ở 1.2** — trước đó dùng `quote_mismatch`, một khẳng định sai vì ta không biết quote của nó |

Thứ tự ưu tiên khi một nguồn dính nhiều lý do: `no_price` → `stale` → `oracle` /
`unregistered` → `no_peer`. Một oracle đã cũ báo `stale`, không báo `oracle`: cả
hai đều đúng, nhưng cái đang thay đổi mới là cái đáng nói.

---

## 6. `arbitrage` — một cơ hội được phát hiện

```json
{
  "type": "arbitrage",
  "v": 1,
  "server_time_ms": 1756368000000,
  "opportunity": {
    "id": "BTCUSDT|perp_usdt|binance_futures|bybit_futures|1756368000000",
    "symbol": "BTCUSDT",
    "kind": "cross_venue",
    "group_id": "perp_usdt",
    "buy_source": "binance_futures",
    "sell_source": "bybit_futures",
    "buy_price": 65000.10,
    "sell_price": 65012.35,
    "spread_gross_pct": 0.0188,
    "spread_after_fees_pct": null,
    "detected_at_ms": 1756368000000
  }
}
```

| Trường | Kiểu | Mặc định 1.0 | Ghi chú |
|---|---|---|---|
| `id` | string | — | **Backend sinh.** FE hiện tự chế `Date.now() + Math.random()` — bỏ |
| `kind` | string | `"cross_venue"` | `cross_venue` \| `basis`. **Từ 1.2 chỉ `cross_venue` được gửi:** cảnh báo sinh trong một nhóm `tradable`, nên hai đầu luôn cùng `market_type` và cùng `quote_asset` và oracle không thể xuất hiện. `basis` là dữ liệu tham chiếu ở `spreads.basis[]`, **cố ý không sinh cảnh báo** |
| `group_id` | string | `"all"` | Nhóm đã sinh ra cơ hội này. Từ 1.2 là `perp_usdt`/`perp_usd`/`spot_usdt`. Cooldown khoá theo `symbol|group|buy|sell`, nên cảnh báo của nhóm này không nuốt cảnh báo của nhóm kia |
| `spread_gross_pct` | float | — | Thay cho `profit_pct` cũ |
| `spread_after_fees_pct` | float\|null | `null` | Bước 1.3, cùng quy tắc với ô ma trận. ⚠️ Cảnh báo vẫn kích hoạt theo `spread_gross_pct` so với `alert_min_spread_pct`, **không** theo số sau phí: một vòng round trip tốn khoảng 0,19% nên gần như mọi cảnh báo hiện âm sau phí. Quyết định "cái gì đáng hành động" là việc của Giai đoạn 3, không phải hệ quả phụ của việc thêm bảng phí |
| `detected_at_ms` | int64 | — | Thay cho `timestamp` cũ |

---

## 7. ĐỔI TÊN SO VỚI HỢP ĐỒNG CŨ

| Cũ | Mới | Vì sao |
|---|---|---|
| `opportunity.profit_pct` | `opportunity.spread_gross_pct` | Chênh lệch thô không phải lợi nhuận ([CLAUDE.md luật 2](../CLAUDE.md)) |
| `opportunity.timestamp` | `opportunity.detected_at_ms` | Hậu tố đơn vị ([CONVENTIONS §1.1](CONVENTIONS.md)) |
| `prices[sym][src]` = float | = object | Cần chỗ cho trạng thái và mốc thời gian |
| `spreads.spreads[b][s]` = float | `cross_venue_groups[].matrix[b][s]` = object | Cần tách nhóm và chỗ cho số đã trừ phí |
| `spreads.prices` | *(bỏ)* | Trùng với message `prices`, FE không dùng |
| message `price_update` | *(bỏ)* | **Backend chưa bao giờ gửi.** FE có handler chết |

---

## 8. ĐỔI HỢP ĐỒNG SAU NÀY

Trong Giai đoạn 1: **không đổi shape**. Đó là toàn bộ lý do bước 1.0 tồn tại. Được
phép **thêm** một trường kèm giá trị mặc định có ghi tài liệu; không bao giờ được
đổi tên, đổi kiểu hay xoá một trường.

Đã thêm theo đúng luật đó:

| Bước | Thêm gì | Mặc định |
|---|---|---|
| 1.2 | `oracle_deviation[].quote_asset_mismatch` | `false` |
| 1.2 | `excluded_sources[].reason` nhận thêm giá trị `unregistered` | — (FE hiện `note_vi`, không phụ thuộc danh sách giá trị) |
| 1.3 | `meta.sources[].fee_verified` | `false` |
| 1.3 | `meta.sources[].maker_fee_bps`/`taker_fee_bps` đổi từ **int** sang **float** | `0` — JSON chỉ có một kiểu số nên shape trên wire không đổi; xem §3 |
| 1.3 | `cost_basis.model` nhận giá trị `taker_round_trip` (bản 1.0 dự kiến `taker_both_legs`) | `"none"` |
| 1.5 | **không thêm trường nào** — chỉ đổ dữ liệu thật vào `state`, `reconnect_count`, `uptime_sec` đã đặt sẵn từ 1.0 | — |

Bước 1.5 là bằng chứng cho lý do Bước 1.0 tồn tại: metric kết nối cần đúng ba
trường, cả ba đã có sẵn trên wire từ đầu với mặc định ghi rõ, nên đổ dữ liệu vào
chúng **không cần một dòng JavaScript nào**. `state: "reconnecting"` cũng vậy — FE
đã có sẵn chấm trạng thái cho nó từ 1.1, chỉ là backend chưa bao giờ gửi.

Từ Giai đoạn 2 (`funding`, đăng ký symbol theo client): thêm message type mới và
thêm trường mới có mặc định — **không** đổi tên và **không** đổi kiểu trường đang có.
Đổi phá vỡ thì tăng `v` và ghi vào bảng ở mục 7.

Bước 2.7a đã thêm theo đúng luật đó, `v` giữ nguyên `1`:

| Bước | Thêm gì | Mặc định |
|---|---|---|
| 2.7a | message type **`funding`** (mục 9) | — (type mới, FE cũ bỏ qua) |
| 2.7a | `meta.funding_basis` | `{model:"gross", applied:[], excluded:[...]}` |
| 2.7a | `meta.sources[].funding_stale_after_sec` | `0` = nguồn này không có funding |
| 2.7a | `meta.sources[].funding_publish_mode` | `""` = nguồn này không có funding |
| 2.7a | HTTP `GET /api/funding/history` (mục 10) | — (route mới) |

---

## 9. `funding` — bảng funding realtime (Bước 2.7a)

Gửi **ngay sau `meta`** lúc client kết nối, rồi **mỗi 5 giây** khi có client. Chậm
hơn `prices` (200ms) rất nhiều vì funding là con số chậm: sàn nhanh nhất phát lại
mỗi ~1 giây, sàn chậm nhất im lặng hàng chục phút **theo thiết kế**.

```json
{
  "type": "funding", "v": 1, "server_time_ms": 1788496607935,
  "funding": {
    "BTCUSDT": {
      "bybit_futures": {
        "model": "discrete",
        "rate_per_8h_bps": 0.6863,
        "rate_per_interval_bps": 0.6863,
        "interval_sec": 28800,
        "apr_gross_pct": 7.51,
        "next_funding_at_ms": 1788508800000,
        "is_estimated": true,
        "recv_at_ms": 1788496581000,
        "age_ms": 26000,
        "status": "live",
        "stale_reason": "",
        "mark_price": 81124.3,
        "index_price": 0,
        "rate_cap_per_interval_bps": 200,
        "rate_floor_per_interval_bps": 0,
        "has_cap": true,
        "has_floor": false,
        "rate_type": "",
        "raw_rate": 0.00006863,
        "raw_rate_field": "fundingRate",
        "hedge_spot_source": "binance_spot",
        "hedge_note_vi": "",
        "breakeven_days_fees_only": null
      }
    }
  }
}
```

### 9.1. Đơn vị trên wire

Go và SQLite mang funding ở dạng **phân số** (`RatePer8hFrac`, `APRFrac`) vì phép
tính cần thế. Wire mang **bps và phần trăm**, vì đó là từ vựng hợp đồng này đã
dùng sẵn (`taker_fee_bps`, `spread_gross_pct`) và là thứ dashboard hiển thị.
Chuyển đổi nằm ở **đúng một chỗ** (`newWireFundingPoint`) — hai định nghĩa của
"per 8h" sẽ trôi khỏi nhau, và Bước 3.5 so backtest với paper trading sẽ mất giá
trị chẩn đoán.

| Trường | Kiểu | Mặc định | Ghi chú |
|---|---|---|---|
| `model` | string | — | `discrete` \| `continuous`. Paradex là `continuous`: tích luỹ qua funding index, **không có mốc settle** |
| `rate_per_8h_bps` | float | — | **Số so sánh chéo sàn.** Cùng một bps ở chu kỳ 1h và 8h là hai mức lợi suất khác hẳn nhau, nên bảng so per-interval là bảng không so gì cả |
| `rate_per_interval_bps` | float | — | Rate của ĐÚNG MỘT chu kỳ settle của sàn đó |
| `interval_sec` | int64 | — | Chu kỳ thật, tính bằng giây. **Không bao giờ ghim 8h** ([CLAUDE.md luật 3](../CLAUDE.md)) |
| `apr_gross_pct` | float | — | Đếm số settle thật trong năm: ×1095 cho 8h, ×8760 cho 1h. **THÔ** |
| `next_funding_at_ms` | int64 | `0` | Mốc settle kế tiếp, epoch ms tuyệt đối. `0` = không tồn tại (continuous) hoặc sàn không công bố. FE đếm ngược theo `server_time_ms`, **không** `Date.now()` |
| `is_estimated` | bool | `false` | Rate còn động trong chu kỳ đang chạy, khác với rate đã chốt |
| `recv_at_ms` | int64 | `0` | Thời điểm bot nhận. **Cơ sở duy nhất** của độ tươi, y như `prices` |
| `age_ms` | int64 | `-1` | `server_time_ms - recv_at_ms`. `-1` = chưa đo được |
| `status` | string | `unknown` | `live` \| `stale` \| `unknown`. **Backend quyết** — xem 9.2 |
| `stale_reason` | string | `""` | `""` \| `age` \| `settled`. Hai lỗi khác nhau, xem 9.2 |
| `mark_price` / `index_price` | float | `0` | Giá funding thực sự tính trên. `0` = sàn không gửi |
| `rate_cap_per_interval_bps` / `rate_floor_per_interval_bps` | float | `0` | Trần/sàn sàn công bố cho MỘT chu kỳ |
| `has_cap` / `has_floor` | bool | `false` | **Hai cờ TÁCH BIỆT**: Bybit công bố cap và không có floor, một cờ chung sẽ biến floor chưa đặt thành "funding không bao giờ âm" |
| `rate_type` | string | `""` | Binance gắn nhãn `Special` cho rate do chia cổ tức; backtest phải lọc |
| `raw_rate` / `raw_rate_field` | float / string | `0` / `""` | Giá trị **y nguyên sàn gửi** và tên trường nó đến từ. Để truy vết một con số trên màn hình về đúng message — **không bao giờ** để tính |
| `hedge_spot_source` | string | `""` | Chân spot dashboard đề xuất. `""` = không mở được vị thế, `hedge_note_vi` nói vì sao |
| `hedge_note_vi` | string | `""` | Lý do từ chối bằng lời của sàn, hoặc lý do chọn chân này khi có nhiều ứng viên |
| `breakeven_days_fees_only` | float\|null | `null` | Số ngày funding thu về đủ bù **hoa hồng bốn lượt khớp**, và KHÔNG gì khác |

### 9.2. `status` — hai phép kiểm độc lập

Độ tươi funding **không đo được bằng ngưỡng tuổi đơn thuần**, và đây là phát hiện
đo được của Bước 2.7a.

`status` = `stale` khi **một trong hai**:

1. **`stale_reason: "age"`** — im lặng quá `meta.sources[].funding_stale_after_sec`.
   Ngưỡng này ĐO theo từng sàn (config.yaml) và lớn hơn ngưỡng giá rất nhiều.
2. **`stale_reason: "settled"`** — mốc settle mà reading này nêu **đã trôi qua**
   (quá 2 phút ân hạn). Reading mô tả một kỳ đã kết thúc, dù nó vừa về một giây trước.

Phép kiểm ② tồn tại vì ① không làm nổi việc: **Bybit chỉ phát khi một trường
funding đổi giá trị thật** — đo 2026-09-04, 2.639 giây im lặng trong khi socket
vẫn tải sổ lệnh — nên ngưỡng tuổi của nó phải bằng cả một chu kỳ settle, và một
subscription chết sẽ trông "live" suốt 8 tiếng nếu chỉ có ①.

`meta.sources[].funding_publish_mode` nói mode nào áp dụng: `periodic` (tuổi đo
được sức sống) hay `on_change` (im lặng là bình thường, tuổi không chứng minh gì
— thứ phát hiện chết là `source_status` ở mục 4.2 cộng phép kiểm ②).

Reading `stale` **vẫn nằm trong message** kèm số cuối cùng, y như giá: xoá nó đi
sẽ khiến một sàn có subscription chết trông giống một sàn **không có funding**.

### 9.3. `breakeven_days_fees_only` — cái gì đã trừ, cái gì chưa

`null` khi: không có chân spot · một trong hai sàn **chưa xác minh biểu phí** ·
sàn không công bố chu kỳ dùng được · rate **không trả tiền cho phía này** (spot
long + perp short NHẬN khi rate dương; rate âm là chi phí, mà chi phí thì không
hoà vốn).

Khi có số: nó là `phí round-trip taker bốn lượt ÷ funding một ngày`, trong đó
funding một ngày **đếm số settle** (`86400 / interval_sec`), không nhân APR với
thời gian nắm giữ ([CLAUDE.md luật 6](../CLAUDE.md)). Chưa trừ slippage, chưa trừ
chi phí vay chân spot, và giả định rate giữ nguyên — điều sẽ không xảy ra. Nó là
công cụ **xếp thứ tự một bảng**, không phải dự báo.

`meta.funding_basis` là chỗ khai báo điều đó cho toàn bộ khối funding, song song
với `meta.cost_basis` của ma trận spread:

```json
"funding_basis": {
  "model": "gross",
  "applied": [],
  "excluded": ["taker_fee", "slippage", "spot_borrow", "hedge_funding_cost", "withdrawal"],
  "note_vi": "Mọi số funding ở đây là THÔ …"
}
```

`applied` rỗng và **sẽ còn rỗng tới Bước 3.1** — bước đầu tiên trong lộ trình
được phép nói chữ "ròng".

---

## 10. HTTP API — lịch sử funding (Bước 2.7a)

Không đi qua WebSocket, và có lý do: lịch sử nằm trong SQLite,
`internal/scanner` không biết `internal/store` và **không nên biết**; hơn nữa
"cho tôi 30 ngày khi tôi bấm" là câu hỏi kéo, không phải luồng đẩy. `cmd/scanner`
phục vụ nó vì entrypoint là nơi đã sở hữu store.

```
GET /api/funding/history?symbol=BTCUSDT&days=30
```

| Tham số | Bắt buộc | Ghi chú |
|---|---|---|
| `symbol` | có | Phải là cặp có trong `config.yaml`, nếu không → **400** |
| `days` | không | Mặc định 30, hợp lệ 1–400. Ngoài khoảng → **400**, *không* kẹp im lặng: dashboard hỏi 5.000 ngày là dashboard có lỗi |

```json
{
  "symbol": "BTCUSDT",
  "from_ms": 1785904607935, "to_ms": 1788496607935,
  "note_vi": "Đây là các mốc ĐÃ SETTLE, khác với số realtime trên bảng …",
  "series": [
    { "source": "kraken_futures", "model": "discrete", "interval_sec": 3600,
      "points": [ { "funding_at_ms": 1788480000000, "rate_per_8h_bps": 0.0836,
                    "apr_gross_pct": 0.92, "interval_sec": 3600,
                    "gap_prev_sec": 3600, "rate_type": "" } ] }
  ],
  "coverage": [
    { "source": "okx_futures", "model": "discrete", "rows": 279,
      "oldest_at_ms": 1780560000000, "newest_at_ms": 1788480000000 }
  ]
}
```

Ba điểm không được bỏ:

- **`coverage[]` đi kèm, không phải tuỳ chọn.** Kho dữ liệu **không đều** và sẽ
  không bao giờ đều: OKX giữ ~3 tháng, Kraken giữ cả năm. Một đường đơn giản
  dừng lại giữa biểu đồ sẽ bị đọc thành "sàn này ngừng trả funding".
- **`gap_prev_sec` là khoảng cách ĐO ĐƯỢC tới mốc trước.** Nó khác `interval_sec`
  đúng ở chỗ có settle bị bỏ lỡ hoặc sàn đổi chu kỳ, và ở những hàng đó nó là
  con số duy nhất trung thực.
- **Đây là mốc ĐÃ SETTLE, khác message `funding`.** Message `funding` là rate của
  một kỳ **đang chạy** và còn đổi. Lẫn hai thứ này làm hỏng corpus của Bước 3.3.

Lỗi trả về `{"error_vi": "..."}` kèm status: **503** khi
`storage.enabled=false` (không có database ≠ không có mốc nào — hai chuyện đó vẽ
ra cùng một biểu đồ rỗng), **400** cho tham số sai, **500** khi đọc store hỏng.
