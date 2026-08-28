# HỢP ĐỒNG WEBSOCKET — Backend ↔ Dashboard

> **Chốt:** Bước 1.0 · **Phiên bản:** `v: 1`
> **Phạm vi hiệu lực:** toàn bộ Giai đoạn 1 (Bước 1.0 → 1.6)
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
| `prices[sym][src].best_bid_qty_coin` / `best_ask_qty_coin` | ⬜ `0` | ⬜ | ✅ | ✅ | ✅ | ✅ |
| `source_status[src].state` | ⬜ `unknown` | 🟡 | 🟡 | 🟡 | 🟡 | ✅ |
| `source_status[src].reconnect_count` | ⬜ `0` | ⬜ | ⬜ | ⬜ | ⬜ | ✅ |
| `source_status[src].uptime_sec` | ⬜ `0` | ⬜ | ⬜ | ⬜ | ⬜ | ✅ |
| `meta.sources[].market_type` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `meta.sources[].tradable` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `meta.sources[].stale_after_sec` | ⬜ `10` | ✅ | ✅ | ✅ | ✅(yaml) | ✅ |
| `meta.sources[].taker_fee_bps` | ⬜ `0` | ⬜ | ⬜ | ✅ | ✅(yaml) | ✅ |
| `spreads.cross_venue_groups[]` | ⬜ 1 nhóm `all`, `tradable:false` | ⬜ | ✅ tách nhóm | ✅ | ✅ | ✅ |
| `spreads.basis[]` | ⬜ `[]` | ⬜ | ✅ | ✅ | ✅ | ✅ |
| `spreads.oracle_deviation[]` | ⬜ `[]` | ⬜ | ✅ | ✅ | ✅ | ✅ |
| `*.spread_after_fees_pct` | ⬜ `null` | ⬜ | ⬜ | ✅ | ✅ | ✅ |
| `meta.cost_basis` | ⬜ `model:"none"` | ⬜ | ⬜ | ✅ | ✅ | ✅ |
| `meta.symbols` | ✅ (Go) | ✅ | ✅ | ✅ | ✅(yaml) | ✅ |

🟡 = điền một phần.

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
    "model": "none",
    "applied": [],
    "excluded": ["taker_fee", "maker_fee", "slippage", "funding"],
    "note_vi": "Số hiển thị là chênh lệch THÔ, chưa trừ bất kỳ chi phí nào."
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
      "maker_fee_bps": 0,
      "taker_fee_bps": 0
    }
  ]
}
```

| Trường | Kiểu | Mặc định 1.0 | Ghi chú |
|---|---|---|---|
| `symbols` | []string | 4 cặp hiện tại | Bước 1.4 đọc từ `config.yaml` |
| `alert_min_spread_pct` | float | `0.05` | Ngưỡng lọc mặc định của bảng cảnh báo |
| `cost_basis.model` | string | `"none"` | `none` \| `taker_both_legs` (Bước 1.3) |
| `cost_basis.applied` | []string | `[]` | Chi phí ĐÃ trừ khỏi `*_after_fees_pct` |
| `cost_basis.excluded` | []string | cả 4 | Chi phí CHƯA trừ — FE **bắt buộc** hiển thị |
| `sources[].source` | string | — | Khoá wire, `venue + "_" + market_type` ([CONVENTIONS §2](CONVENTIONS.md)) |
| `sources[].venue` | string | — | `binance`, `bybit`, … |
| `sources[].market_type` | string | giá trị thật | `spot` \| `perp` \| `future` \| `oracle`. Là **dữ kiện tĩnh** của sàn nên điền đúng ngay từ 1.0; Bước 1.2 là lúc bắt đầu **dùng** nó để tách nhóm |
| `sources[].quote_asset` | string | `"USDT"`/`"USD"` | Kraken là `USD` — không so sánh chéo với USDT (Bước 1.2) |
| `sources[].tradable` | bool | giá trị thật | Pyth là `false` ngay từ 1.0. Bước 1.2 là lúc grouping bắt đầu dựa vào cờ này |
| `sources[].stale_after_sec` | int | `10` | Ngưỡng staleness **theo từng sàn** (Bước 1.1) |
| `sources[].maker_fee_bps` / `taker_fee_bps` | int | `0` | **Bps, số nguyên** — không dùng float cho phí niêm yết ([CONVENTIONS §1.2](CONVENTIONS.md)) |

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
| `venue_time_ms` | int64 | `0` | Thời gian **sàn phát**. `0` = sàn không cấp. ⚠️ Bybit/Paradex/Kraken hiện điền `time.Now()` — Bước 1.1 sửa thành `0` |
| `recv_at_ms` | int64 | `0` | Thời điểm **bot nhận**, đặt tại **một chỗ duy nhất**. Đây là cơ sở duy nhất của staleness |
| `age_ms` | int64 | `-1` | `server_time_ms - recv_at_ms`. `-1` = chưa đo được |
| `status` | string | `"unknown"` | `live` \| `stale` \| `unknown`. **Backend tính**, FE chỉ hiển thị |
| `best_bid` / `best_ask` | float | `0` | Đỉnh sổ. `0` = chưa biết. Bước 1.2 điền |
| `best_bid_qty_coin` / `best_ask_qty_coin` | float | `0` | Khối lượng đỉnh sổ, **tính theo coin không phải contract** — OKX/Gate/Kraken niêm yết theo contract, connector phải quy đổi trước khi điền ([CONVENTIONS §1.4](CONVENTIONS.md)). Đây là **bộ lọc thanh khoản bậc một**, không phải độ sâu: nó nói có bao nhiêu ở giá tốt nhất, không nói lệnh $60k khớp ở đâu. Độ sâu đầy đủ về qua REST ở Bước 2.7 ([PLAN §7.4](PLAN.md#74-chiến-lược-độ-sâu-sổ-lệnh)) |

> ⚠️ `venue_time_ms` **không bao giờ** được dùng để tính staleness. Nó chỉ để chẩn
> đoán độ trễ đường truyền. Lý do đầy đủ ở [PLAN.md Bước 1.1](PLAN.md).

### 4.2. `source_status[source]` — trạng thái mức KẾT NỐI

Tách khỏi mục 4.1 vì đây là thuộc tính của **kết nối**, không của từng symbol: một
sàn có thể còn kết nối nhưng ngừng đẩy một cặp thanh khoản mỏng.

| Trường | Kiểu | Mặc định 1.0 | Ghi chú |
|---|---|---|---|
| `state` | string | `"unknown"` | `connected` \| `reconnecting` \| `disconnected` \| `unknown` (Bước 1.5) |
| `last_msg_at_ms` | int64 | `0` | Message cuối nhận được từ sàn, bất kể symbol (Bước 1.1) |
| `reconnect_count` | int | `0` | Số lần reconnect từ lúc khởi động (Bước 1.5) |
| `uptime_sec` | int64 | `0` | Thời gian kết nối liên tục hiện tại (Bước 1.5) |

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
      "group_id": "all",
      "label_vi": "Tất cả nguồn",
      "market_type": "unknown",
      "quote_asset": "",
      "tradable": false,
      "note_vi": "Khối này còn trộn spot, perpetual và oracle …",
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
  "excluded_sources": []
}
```

### 5.1. `cross_venue_groups[]` — chênh lệch giữa hai VENUE cùng loại thị trường

Một nhóm = một tập nguồn **so sánh được với nhau**: cùng `market_type` **và** cùng
`quote_asset`. Ma trận chỉ được dựng **trong** một nhóm, không bao giờ xuyên nhóm.

| Trường | Kiểu | Mặc định 1.0 | Ghi chú |
|---|---|---|---|
| `group_id` | string | `"all"` | 1.2: `perp_usdt`, `perp_usd`, `spot_usdt` |
| `label_vi` | string | — | Tiêu đề hiển thị (tiếng Việt) |
| `market_type` | string | `"unknown"` | Loại thị trường chung của nhóm |
| `quote_asset` | string | `""` | Đồng quote chung |
| `tradable` | bool | `false` ở 1.0 | `false` → nhóm là **tham chiếu**: FE vẫn tô màu theo dấu của số, nhưng **không** gắn nhãn "cơ hội" cho ô nào. Nhóm `all` của 1.0 là `false` vì nó còn trộn spot/perp/oracle |
| `note_vi` | string | `""` | Cảnh báo riêng của nhóm. VD nhóm `perp_usd`: *"Chênh lệch này bao gồm cả chênh USD/USDT, không phải cơ hội thuần"* |
| `sources` | []string | mọi nguồn | Thứ tự hiển thị, do backend quyết |
| `matrix[buy][sell]` | object | — | Xem 5.4 |

> **1.0 gửi đúng một nhóm `all` chứa toàn bộ nguồn**, đánh dấu `tradable: false`.
> Ma trận và cách tô màu giữ nguyên như trước; điểm khác duy nhất nhìn thấy được là
> ô vượt ngưỡng **không còn nhấp nháy cam như một cơ hội** — vì so sánh spot với
> perp không phải cơ hội. Bước 1.2 tách thành nhiều nhóm, FE **không cần sửa** vì
> đã lặp qua mảng.

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

`[]` ở Bước 1.0. Không bao giờ sinh cảnh báo từ khối này.

```json
{ "oracle_source": "pyth", "source": "binance_futures", "deviation_pct": 0.004 }
```

### 5.4. Ô ma trận

```json
{ "spread_gross_pct": 0.031, "spread_after_fees_pct": null }
```

| Trường | Kiểu | Mặc định 1.0 | Ghi chú |
|---|---|---|---|
| `spread_gross_pct` | float | — | `(sell - buy) / buy * 100`. **THÔ** — chưa trừ gì |
| `spread_after_fees_pct` | float\|null | `null` | `null` = chưa có mô hình phí. Bước 1.3 điền. **Không phải lợi nhuận ròng** — chưa trừ slippage |

> 🚩 Tên trường là `spread_*`, **không phải** `profit_*`, và có `_gross_`/`_after_fees_`
> tường minh. Đây là thi hành trực tiếp [CLAUDE.md luật 2](../CLAUDE.md).

### 5.5. `excluded_sources[]`

Nguồn có giá nhưng bị loại khỏi mọi nhóm, kèm lý do — để FE giải thích được vì sao
một sàn biến mất khỏi ma trận thay vì im lặng bỏ đi. `[]` ở Bước 1.0.

```json
{ "source": "pyth", "reason": "oracle", "note_vi": "Oracle, không giao dịch được" }
```

`reason`: `no_price` | `oracle` | `quote_mismatch` | `stale` | `disconnected` | `no_peer`.

`no_price` được dùng ngay từ Bước 1.0: nguồn gửi giá không dùng được (≤ 0, NaN, vô cực) bị loại khỏi ma trận và phải nói ra lý do, không được im lặng biến mất.

---

## 6. `arbitrage` — một cơ hội được phát hiện

```json
{
  "type": "arbitrage",
  "v": 1,
  "server_time_ms": 1756368000000,
  "opportunity": {
    "id": "BTCUSDT|all|binance_futures|bybit_futures|1756368000000",
    "symbol": "BTCUSDT",
    "kind": "cross_venue",
    "group_id": "all",
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
| `kind` | string | `"cross_venue"` | `cross_venue` \| `basis`. ⚠️ **Chưa thi hành ở 1.0:** `checkArbitrage` vẫn duyệt mọi nguồn nên oracle vẫn có thể xuất hiện trong bảng cảnh báo. Bước 1.2 mới chặn |
| `group_id` | string | `"all"` | Nhóm đã sinh ra cơ hội này |
| `spread_gross_pct` | float | — | Thay cho `profit_pct` cũ |
| `spread_after_fees_pct` | float\|null | `null` | Bước 1.3 |
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

Trong Giai đoạn 1: **không đổi**. Đó là toàn bộ lý do bước 1.0 tồn tại.

Từ Giai đoạn 2 (`funding`, đăng ký symbol theo client): thêm message type mới và
thêm trường mới có mặc định — **không** đổi tên và **không** đổi kiểu trường đang có.
Đổi phá vỡ thì tăng `v` và ghi vào bảng ở mục 7.
