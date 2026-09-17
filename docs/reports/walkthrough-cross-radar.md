# Radar chéo sàn Binance ⟷ Bybit — Bước 4.5i, Hướng 1: báo cáo thực hiện

> **Ngày:** 2026-09-17 · **Trạng thái:** 🟡 đã xây, kiểm và chạy thật trên cổng phụ; **chưa triển khai lên 8085/8087**
> (8087 đang chạy auto-trader testnet, nên không khởi động lại — xem §7).
> **Chỉ đọc:** không route, không nút, không dòng Go nào của radar đặt lệnh.

---

## 1. Tóm tắt

| Tiêu chí nghiệm thu | Kết quả |
|---|---|
| 1. `GET /api/cross-radar` trả ma trận ≥ 12 cặp | ✅ **13/13 cặp live ở cả hai sàn** — đo trên scanner phụ `:8088`, và qua portal phụ `:8089` tại `/api/scanner/cross-radar` |
| 2. Funding và best bid/ask của hai sàn cùng lúc | ✅ mỗi chân có rate/8h, chu kỳ, tuổi, chế độ phát, bid/ask, spread chạm, độ sâu và trạng thái tươi riêng (§4) |
| 3. Portal hiện bảng radar cập nhật liên tục | ✅ tab **Radar Chéo Sàn**, đọc mỗi 5 s khi tab mở; chụp headless 1440 px, 400 px và trạng thái cảnh báo (§5) — **trên cổng phụ**; 8087 cần khởi động lại (§7) |
| 4. Báo cáo này | ✅ |
| `go test -count=1 -race ./...` | ✅ **38/38 package đạt**, 0 FAIL, 0 data race |

---

## 2. Chỗ lệch khỏi đặc tả, và vì sao

| Đặc tả | Đã làm | Lý do |
|---|---|---|
| Trường `net_apr_capital_pct` | `after_cost_apr_capital_pct` + `after_cost_apr_notional_pct` | CLAUDE.md quy tắc 2: chữ "net" chỉ thuộc `internal/strategy`. Con số này chỉ trừ phí 4 lệnh và spread chạm; `costs_excluded_vi` liệt kê năm thứ chưa trừ. |
| 🟢 khi Net APR ≥ 20% và spread ≤ 5 bps | Giữ hai điều kiện đó, **thêm**: chênh hiện tại phải hoà vốn trong ≤ `good_max_breakeven_days` (3 ngày) | Review chỉ ra mâu thuẫn: APR "7 ngày giữ" rải phí thành ~11%/năm, nhưng corpus 3 năm đo một đợt ≥ 15% kéo dài **trung vị 1 ngày**, P90 2–3 ngày. Một chênh cần cả tuần mới hoà vốn sẽ không bao giờ trả được phí, dù APR 7 ngày của nó cao. Mỗi cặp giờ có `breakeven_hold_days`. |
| Top 10 sổ lệnh cập nhật 3–5 s | Giá chạm từ WebSocket đang chạy (tươi hơn 3 s); độ sâu lấy từ lượt quét REST có sẵn, kèm tuổi | Quy tắc 10: không stream độ sâu liên tục. |
| Rate "hiện tại và dự phóng" | Rate ĐANG HÌNH THÀNH (`premiumIndex` của Binance, `tickers` của Bybit), `rate_model: forming_gross` | Sàn còn sửa số này tới mốc settle. Radar xếp hạng và ghi nhật ký, không quyết định (bài học bước 3.2). |
| Ngưỡng 15/25% gõ trong giao diện | Giao diện đọc ngưỡng từ `cross_radar.event_thresholds_gross_apr_pct` | `config.yaml` là nguồn duy nhất. |
| API trên `:8085`/`:8087` | Đúng hai route đó, nhưng đo trên `:8088`/`:8089` | "Không làm gián đoạn daemon đang chạy". Lệnh triển khai ở §7. |

Công thức APR sau chi phí giữ đúng đặc tả, gồm hệ số **K/2**. Mọi số trên radar là chênh **gộp**
hoặc **sau phí + spread chạm** — không số nào là lãi.

---

## 3. Mã đã tạo / sửa

| Tệp | Nội dung |
|---|---|
| `config.yaml`, `internal/config/crossradar.go` *(mới)*, `config.go` | Khối `cross_radar:`. Validate từ chối: hai nguồn trùng, nguồn không tồn tại, không phải perp, **khác quote** (bẫy quote bridging), K < 1, ngưỡng âm (0 hoặc bỏ trống lấy mặc định), ngưỡng trùng. |
| `internal/scanner/cross_radar.go` *(mới)* | Hàm thuần `buildCrossRadar`: chênh, APR gộp, phí từ biểu phí đã xác minh, spread chạm, số ngày hoà vốn, APR sau chi phí trên notional và vốn, basis, trạng thái, ghi chú (chu kỳ khác nhau, sàn chỉ phát khi đổi), thứ tự. `Scanner.CrossRadar()` gom dưới từng lock (không lồng nhau) rồi tính ngoài lock. |
| `internal/scanner/cross_events.go` *(mới)* | `CrossEventTracker`, thuần, không biết store. |
| `internal/store/schema.sql`, `store.go`, `cross_events.go` *(mới)* | Schema **v6**: bảng `cross_spread_events`, khoá `(writer, symbol, threshold_apr_pct, started_at_ms)`. |
| `cmd/scanner/crossradar.go` *(mới)*, `main.go` | `GET /api/cross-radar`, `GET /api/cross-radar/events?days=`. Job ghi nhật ký chạy trên `tickLoop`, nên tick trễ được đếm; `writer` = `scanner:<port>`; đợt đang mở chỉ ghi lại khi đỉnh đổi hoặc mỗi phút. |
| `cmd/execportal/feeds/feeds.go`, `server.go`, `guard_test.go` | Proxy đường cố định, không giải mã, cache 3 s / 30 s, `days` chỉ nhận 1/7/30. Danh sách export được mở thêm đúng hai handler. |
| `static/index.html`, `static/js/radar.js` *(mới)*, `shell.js`, `main.js`, `css/portal.css` | Tab Radar Chéo Sàn; CSS gói trong `#panel-radar`. |
| `docs/WS-CONTRACT.md` §12 | Hợp đồng hai route. |

**Test mới:**
- `internal/scanner`, radar (9):
  - số học tính tay: 7,75 bps/8h → 84,8625%/năm gộp; phí 21 bps;
  - K/2 chứ không phải K; bốn trạng thái;
  - cổng hoà vốn: 2,5 bps/8h → ≈ 2,8 ngày thì xanh, 2,2 bps/8h → ≈ 3,2 ngày thì THEO DÕI;
  - cũ/thiếu → `unavailable`; phí chưa xác minh; chu kỳ 4h/8h; ghi chú on_change; thứ tự;
  - JSON không có `net_`/`profit`.
- `internal/scanner`, tracker (5): grace, đỉnh, đổi chiều, đổi chiều sau khi chạm dưới ngưỡng, một nhịp dữ liệu cũ không cắt đợt.
- `internal/store`: mở / ghi đè / đóng `restart` chỉ cho writer của mình, không đụng đợt đang sống của writer khác.
- `cmd/scanner`: phương thức, tắt lưu trữ, `days` sai; trung vị/P90, cận dưới, ngưỡng cũ vẫn được tóm tắt.
- `internal/config`: 7 cấu hình sai bị từ chối.
- `cmd/execportal/feeds`: đường cố định, TTL, cửa sổ bị từ chối không tới scanner.

---

## 4. Số đo thật — scanner phụ `:8088`, 2026-09-17 12:01:38 +07, 13/13 cặp live

- Chênh = Binance − Bybit.
- **Hoà vốn** = số ngày chênh HIỆN TẠI cần giữ để trả hết phí + spread chạm.
- **APR sau chi phí** rải chi phí trên 7 ngày giữ **giả định**, K = 2 (vốn = notional).

| Cặp | Khuyến nghị | Binance bps/8h | Bybit bps/8h | Chênh bps/8h | APR gộp | Phí + chạm (bps) | Hoà vốn (ngày) | APR sau chi phí / vốn | Chạm B / Y (bps) | Basis (bps) | Trạng thái |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|
| SUIUSDT | LONG BYBIT SHORT BINANCE | +0,04 (8h) | −0,93 (8h) | +0,96 | 10,57% | 23,8 | 8,2 | −1,8% | 1,38 / 1,38 | 4,1 | watch |
| LTCUSDT | LONG BYBIT SHORT BINANCE | +0,10 (8h) | −0,63 (8h) | +0,72 | 7,90% | 24,8 | 11,5 | −5,0% | 1,92 / 1,92 | 1,9 | normal |
| UNIUSDT | LONG BINANCE SHORT BYBIT | +0,26 (8h) | +0,91 (8h) | −0,65 | 7,16% | 23,9 | 12,2 | −5,3% | 1,46 / 1,46 | 4,4 | normal |
| LINKUSDT | LONG BYBIT SHORT BINANCE | +0,58 (8h) | 0,00 (8h) | +0,58 | 6,34% | 22,8 | 13,1 | −5,5% | 0,90 / 0,90 | 2,7 | normal |
| BNBUSDT | LONG BYBIT SHORT BINANCE | +0,77 (8h) | +0,40 (8h) | +0,37 | 4,10% | 22,5 | 20,0 | −7,6% | 0,14 / 1,38 | 14,7 | normal |
| XRPUSDT | LONG BINANCE SHORT BYBIT | −0,50 (8h) | −0,32 (8h) | −0,19 | 2,03% | 22,5 | 40,5 | −9,7% | 0,77 / 0,77 | 0,8 | normal |
| HYPEUSDT | LONG BINANCE SHORT BYBIT | +0,66 (**4h**) | +0,83 (8h) | −0,17 | 1,86% | 22,4 | 44,0 | −9,8% | 0,13 / 1,26 | 0,1 | normal |
| ETHUSDT | LONG BINANCE SHORT BYBIT | +0,24 (8h) | +0,30 (8h) | −0,06 | 0,61% | 21,1 | > 99 | −10,4% | 0,04 / 0,04 | −0,1 | normal |
| DOGEUSDT | LONG BINANCE SHORT BYBIT | +0,04 (8h) | +0,19 (8h) | −0,16 | 1,71% | 23,5 | 49,9 | −10,5% | 1,23 / 1,23 | 1,2 | normal |
| AAVEUSDT | LONG BYBIT SHORT BINANCE | +0,50 (8h) | +0,38 (8h) | +0,12 | 1,27% | 22,6 | 65,2 | −10,5% | 0,82 / 0,82 | 4,9 | normal |
| BTCUSDT | LONG BYBIT SHORT BINANCE | +0,49 (8h) | +0,47 (8h) | +0,02 | 0,26% | 21,0 | > 99 | −10,7% | 0,01 / 0,01 | 0,7 | normal |
| SOLUSDT | LONG BINANCE SHORT BYBIT | +0,07 (8h) | +0,15 (8h) | −0,07 | 0,82% | 23,0 | > 99 | −11,2% | 1,00 / 1,00 | 2,0 | normal |
| NEARUSDT | LONG BINANCE SHORT BYBIT | +0,91 (8h) | +1,00 (8h) | −0,09 | 0,98% | 28,5 | > 99 | −13,9% | 3,74 / 3,74 | −3,7 | normal |

**Đọc bảng:**
- **Không cặp nào dương sau chi phí và không cặp nào hoà vốn dưới 8 ngày.** Cặp rộng nhất là SUI
  10,6% gộp, cần 8,2 ngày.
- Để hoà vốn trong 1 ngày (trung vị lịch sử của một đợt rộng), chênh phải khoảng **7–8 bps/8h, tức
  ~80%/năm gộp**. Để xanh (≤ 3 ngày) cần ~2,6 bps/8h, ~29%/năm.
- Kết quả khớp báo cáo 3 năm (trung vị 3,2% APR): radar sống đang cho thấy đúng trạng thái
  "bình thường" mà corpus mô tả.
- Mọi rate Bybit ở chế độ **phát khi đổi**: tuổi 19–71 s lúc chụp không chứng minh số còn đúng, và
  radar ghi chú điều đó ở mỗi cặp.
- Basis bằng 0 ở vài cặp là thật: cùng giá chạm trên bước giá thô. Độ phân giải của basis là một tick.
- HYPE trên Binance settle mỗi 4h, trên Bybit mỗi 8h. Radar quy về 8h để so; tiền vẫn về theo mốc
  riêng từng sàn (quy tắc 3, 6).
- **Nhật ký đợt:** 0 đợt, vì không cặp nào chạm 15% trong thời gian chạy phụ. Thời lượng thật cần
  radar chạy nhiều ngày (§7).

---

## 5. Giao diện

Chụp bằng Chromium headless (Playwright đã cache sẵn trên máy) qua portal phụ `:8089`. Portal đó
không giữ key, tắt auto-trader, và thư mục ý định riêng.

**Bảng (8 cột):**
1. Cặp;
2. Khuyến nghị: LONG / SHORT kèm tên sàn;
3. Rate Binance;
4. Rate Bybit — dòng phụ ghi chu kỳ · tuổi · "khi đổi" nếu sàn chỉ phát khi đổi; bản đầy đủ nằm ở
   tooltip;
5. Chênh Δf (bps/8h) và APR gộp;
6. APR sau chi phí / vốn, kèm chi phí vòng và số ngày hoà vốn;
7. Spread chạm hai sàn và basis;
8. Trạng thái.

**Trạng thái** là huy hiệu **có chữ**, không chỉ màu: CƠ HỘI TỐT · CHỜ THANH KHOẢN · THEO DÕI ·
BÌNH THƯỜNG · THIẾU DỮ LIỆU. Dữ liệu cũ có thêm huy hiệu CŨ hoặc SỔ CŨ.

**Kích thước:** ở 1440 px cả 8 cột nằm gọn. Ở 400 px, bảng cuộn ngang trong khung riêng, trang không
tràn ngang.

**Cảnh báo** theo ngưỡng cao nhất trong config (25%), khi funding CẢ HAI sàn live — không cần sổ lệnh
live:
- hàng có nền hổ phách, vạch trái và huy hiệu chữ `≥25%`;
- nhãn `≥25%` hiện trên tên tab;
- một dòng `role="status"` chỉ liệt kê **tên cặp**, không có số phần trăm, nên trình đọc màn hình chỉ
  đọc lại khi TẬP cặp đổi, không phải mỗi 5 giây.

Đã kiểm bằng dữ liệu giả qua một portal thứ ba, vì thị trường lúc đo không có cặp nào ≥ 25%.

**Lọc và nhật ký:**
- **Lọc:** "Tất cả" hoặc "Chỉ ≥ 15%/năm". Ngưỡng lấy theo ngưỡng thấp nhất trong config; bỏ các cặp
  thiếu dữ liệu; nhớ theo trình duyệt.
- **Nhật ký đợt:** cửa sổ 1/7/30 ngày; mỗi ngưỡng một ô thống kê (số đợt, trung vị, P90, lý do kết
  thúc); bảng 100 đợt mới nhất.

**Tần suất đọc:** radar 5 s, nhật ký 30 s, chỉ khi tab đang mở và trang đang hiện.

Mọi chữ đến từ scanner được ghi bằng `textContent`. Test server của portal cấm `innerHTML`.

---

## 6. Nhật ký đợt chênh — đo cái gì

- **Một đợt** gắn với (writer, cặp, ngưỡng, chiều). Nó mở ở lần đọc đầu tiên có chênh gộp ≥ ngưỡng,
  khi funding CẢ HAI sàn `live`.
- **Đóng khi:**
  - **dưới ngưỡng** liên tục 60 s, và kết thúc được đóng dấu ở lần đọc **đầu tiên** dưới ngưỡng;
  - hoặc **đổi chiều**, kết thúc ở lần đầu dưới ngưỡng nếu đã chạm dưới;
  - hoặc **funding mất `live` liên tục 60 s**, kết thúc ở lần cuối còn thấy trên ngưỡng. Một nhịp
    ngắn — poll lỗi, ranh giới settle — **không** cắt đợt;
  - hoặc tiến trình dừng: lần khởi động sau **của cùng writer** đóng đợt với lý do `restart`.
- **Thống kê:**
  - thời lượng lấy từ đợt kết thúc vì chênh (`below`, `flip`);
  - đợt bị cắt (`stale`, `restart`) được báo là **cận dưới** (`censored_count`,
    `censored_median_lower_bound_sec`), không bị bỏ. Bỏ chúng làm thời lượng ngắn hơn thật, vì đợt
    dài nhất là đợt dễ gặp khoảng trống nhất.
- **`writer` = `scanner:<port>`:** hai scanner ghi cùng một file (scanner phụ, lần chạy lại) không đếm
  đôi một đợt và không đóng đợt đang sống của nhau. Route chỉ trả đợt của chính tiến trình.
- **Đây là đợt của funding ĐANG HÌNH THÀNH.** Nó khác "ngày liên tiếp ≥ 15% của funding ĐÃ SETTLE"
  trong báo cáo 3 năm; hai số đo không được cộng hay thay nhau.

---

## 7. Triển khai lên 8085 / 8087 — việc của người vận hành

Hai tiến trình đang chạy không bị đụng tới:
- **8085** là scanner dev, không phải lượt chạy cổng 3.5 (không có `.paper/launch-state.txt`).
- **8087** là portal với auto-trader TESTNET **đang chạy**, 12 cặp.

Thứ tự đề xuất, từ gốc repo:

1. **Scanner trước.** Dừng tiến trình 8085, rồi chạy
   `PORT=8085 go run ./cmd/scanner -started-at-file .paper/started_at`.
   Lần mở đầu nâng `data/scanner.db` lên **schema v6** (chỉ thêm bảng). Nhật ký đợt ghi với writer
   `scanner:8085`.
2. **Paper ledger.** Binary đang chạy trên 8086 biết v5. Tay cầm SQLite đang mở của nó vẫn đọc được,
   nhưng **nếu nó khởi động lại bằng binary cũ, nó sẽ từ chối file v6**. Khởi động lại nó sau bước 1.
   Cũng vậy với bất kỳ `cmd/backtest` build từ commit trước thay đổi này: nó cần bản sao lấy TRƯỚC khi
   scanner nâng file. Các bản lưu `.paper/run*` là v5, không bị ảnh hưởng.
3. **Portal cuối cùng**, lúc bot không đang đặt lệnh: `go run ./cmd/execportal -port 8087`. Bot nhận
   lại các cặp đang giữ khi khởi động (đã đo 2,47 s ở 4.5e).

Tiến trình phụ `:8088` (scanner, DB bản sao trong scratchpad) và `:8089` (portal không key) được dừng
khi phiên kết thúc.

**Để có thời lượng đợt thật:** để radar chạy nhiều ngày trên 8085, rồi đọc
`/api/scanner/cross-radar/events?days=30` qua portal, hoặc truy vấn trực tiếp:
`sqlite3 -readonly "file:data/scanner.db?mode=ro" "SELECT threshold_apr_pct, end_reason, COUNT(*), AVG(duration_sec) FROM cross_spread_events WHERE writer='scanner:8085' GROUP BY 1,2"`.

---

## 8. Review

| Vòng | Kết luận | Phát hiện | Xử lý |
|---|---|---|---|
| 1 | Yêu cầu sửa, **0 chặn** | **3 lớn:** (1) một nhịp funding mất `live` cắt ngay đợt dài nhất, và đợt bị cắt bị bỏ khỏi thống kê → thời lượng lệch ngắn; (2) nhiều scanner cùng một file → đếm đôi, và lần khởi động của một scanner đóng đợt đang sống của scanner kia; (3) nhãn xanh dựa trên 7 ngày giữ giả định, mâu thuẫn trung vị 1 ngày đã đo. **9 nhỏ:** đổi chiều kết thúc muộn; "live" của Bybit không chứng minh tươi; giá trị 0 trong config bị thay thầm bằng mặc định; tóm tắt bỏ sót ngưỡng cũ; ngưỡng gõ cứng trong giao diện; trình đọc màn hình đọc lại mỗi 5 s; CSS toàn cục; tài liệu lệch (§10/§12, "schema v6"); ghi DB mỗi 5 s | Sửa hết, mỗi cái có test hoặc kiểm bằng ảnh chụp |
| 2 | Yêu cầu sửa, **0 chặn** | **1 lớn:** lời hứa "từ chối số 0 trong config" không bao giờ chạy, vì `Load` áp mặc định trước khi kiểm. **6 nhỏ:** đợt đã dưới ngưỡng đủ grace rồi mất dữ liệu bị ghi `stale` thay vì `below`; một lần ghi DB lỗi làm mất lần đóng; bộ lọc ẩn một cặp mà dòng cảnh báo vẫn nêu; trang không hiện số cận dưới; writer gắn với cổng (một lần chạy trên cổng khác không đóng đợt cũ); ghi chú "cắt bớt" lặp hai lần | Sửa hết. Số 0 giờ được ghi rõ là "lấy mặc định", chỉ số âm bị từ chối. Lần đóng ghi lỗi được giữ lại và thử lại ở tick sau. Đợt dở được đóng kể cả khi radar đã tắt. |

**Đột biến có chủ đích sau review** — mỗi cái chạy test rồi hoàn nguyên; tất cả làm test đỏ:
- đợt dưới ngưỡng rồi mất dữ liệu bị ghi `stale`;
- một nhịp dữ liệu cũ đóng đợt ngay;
- bỏ cổng hoà vốn;
- lần khởi động đóng đợt của writer khác;
- vốn tính theo K thay vì K/2.
