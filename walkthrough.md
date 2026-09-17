# Walkthrough — Hướng A: xử lý các lỗi P1 R8 và R6 của kiểm toán Cấp độ 1

> **Ngày:** 2026-09-17 · **Nền:** commit `a165ce7` · **Trạng thái:** chưa commit, chưa nghiệm thu trên testnet
> **Nguồn yêu cầu:** `docs/reports/audit-level-1-single-exchange-2026-09-17.md`, mục R8 (hai phần) và R6.
> **Phạm vi:** chỉ `cmd/execportal` (auto-trader TESTNET, quyết định Q18) và `static/`. Không đụng `cmd/scanner`, `internal/execution`, `internal/broker`.

## 0. An toàn vận hành trong lúc sửa

- Ba daemon **không bị động tới**: scanner `:8085` (PID 84279), paperledger `:8086` (PID 84423), execportal `:8087` (PID 5253). Cùng PID trước và sau toàn bộ phiên.
- Execportal đang chạy là **binary cũ**. Mã mới chỉ có hiệu lực khi người vận hành **tự khởi động lại** portal. Việc đó chưa làm, và khởi động lại sẽ tiếp nhận lại 8 vị thế testnet (ETH, SOL, DOGE, LTC, SUI, LINK, UNI, AAVE) theo đường adopt sẵn có.
- Mọi test chạy offline trên `brokertest`. Không test nào mở socket, không lệnh nào tới sàn.

## 1. R8 phần 1: van cắt lỗ vẫn chạy cho cặp Halt do rớt mạng

### Quy tắc
Cặp bị halt **chỉ vì** 5 lần đọc sàn hỏng liên tiếp (`haltFromRead`) mà đang giữ vị thế của bot sẽ được quét lại ở mỗi lượt. Bot chỉ hành động khi lần đọc chứng minh vị thế **sạch**:
- `HedgeBothOpen`;
- đúng **một** ý định, trùng `IntentID` của vị thế, do bot mở (`FromAutotrade`);
- phần lệch hai chân ≤ dung sai một bước.

Khi đó chỉ **hai** lối thoát rủi ro được gửi lệnh:

| Lối thoát | Khi cặp đang Halt do đọc hỏng |
|---|---|
| Cắt lỗ basis giãn (`CheckExitBasis`) | ✅ gửi lệnh đóng, log `HALT_RISK_EXIT` |
| Thoát funding âm (`CheckExitFunding`, gồm hysteresis `ExitNegative*`) | ✅ gửi lệnh đóng, log `HALT_RISK_EXIT` |
| Chốt lời hội tụ (`CheckExitTakeProfit`) | ❌ giữ lại, trang ghi "KHÔNG phải rủi ro — chờ XÁC NHẬN" |
| Đủ số mốc (`CheckExitEpochs`) | ❌ giữ lại (lịch hẹn, không phải rủi ro) |
| Vào lệnh mới | ❌ không bao giờ |

### Các ca biên đã xử lý
- **Đọc được nhưng không sạch** (lệch chân, bằng chứng mâu thuẫn, ý định lạ): bot **không** giao dịch. Cặp bị **halt lại với số mới**, `haltFromRead=false`, nên van tự động tắt. Người vận hành đã đọc "halt do mạng", mà đây không còn là halt do mạng, nên xác nhận theo số cũ bị từ chối.
- **Xác nhận hoặc halt khác chen vào giữa lúc đọc và lúc gửi:** lệnh thoát bị huỷ. `haltedRiskJobValidLocked` được kiểm cả lúc phán xử lẫn trong `sendExit`: cùng trạng thái, cùng `haltSeq`, cùng `IntentID`, bot đang chạy, không kill, không stop.
- **Trong lúc lệnh đóng khẩn cấp đang ra sàn:** cặp ở `StateClosing`. Nút xác nhận (đơn lẻ hay tất cả) không thể chen vào vì cặp không ở trạng thái halted, và chỗ của cặp vẫn được tính.
- **Sau lệnh đóng khẩn cấp, cặp luôn ở trạng thái DỪNG BẢO VỆ:**
  - phẳng → halt **số mới**, lý do "ĐÃ CẮT LỖ KHẨN CẤP… không vào lệnh mới tới khi XÁC NHẬN", không giữ chỗ;
  - chưa gửi được gì (bận hoặc bị từ chối) → khôi phục **đúng** halt cũ, lượt sau thử lại;
  - báo động, gửi chưa xác nhận, hoặc đóng chưa phẳng → halt về vị thế, van tắt.
- **Halt do lịch sử funding mù 30 phút, do lỗi giao dịch, do mở báo động:** **không** được van tự động (đúng phạm vi yêu cầu: chỉ `readFailures`).

### Mã
- Trường `haltFromRead`, `lastHaltedExitKey`, `lastDeferKey`: [engine.go:229](cmd/execportal/autotrade/engine.go#L229)
- Lập job `haltedRiskOnly`: [engine.go:521](cmd/execportal/autotrade/engine.go#L521)
- Tập lối thoát được phép: [engine.go:1083](cmd/execportal/autotrade/engine.go#L1083)
- Kiểm hiệu lực: [engine.go:1089](cmd/execportal/autotrade/engine.go#L1089)
- Phán xử: [engine.go:1101](cmd/execportal/autotrade/engine.go#L1101)
- Kiểm lại trước khi gửi: [engine.go:1226](cmd/execportal/autotrade/engine.go#L1226)
- Hậu xử lý: [engine.go:1678](cmd/execportal/autotrade/engine.go#L1678)
- Chỉ `pairReadFailed` bật cờ: [engine.go:1733](cmd/execportal/autotrade/engine.go#L1733)
- Mọi halt khác xoá cờ trong `haltPairMayHoldLocked`. Start, Stop, Kill và xác nhận cũng xoá cờ.
- `exitAssessment.DueKeys` gắn tên check cho từng lý do thoát, để phân biệt rủi ro với chốt lời mà **không so chuỗi**: [signal.go:818](cmd/execportal/autotrade/signal.go#L818), [signal.go:829](cmd/execportal/autotrade/signal.go#L829)

## 2. R8 phần 2: XÁC NHẬN TẤT CẢ (Ack All)

### Hành vi
- `Engine.AckAll(seen map[string]int)` ([engine.go:2403](cmd/execportal/autotrade/engine.go#L2403)) là N lần xác nhận đơn lẻ gộp **nguyên tử**, dùng chung `acknowledgePairLocked` ([engine.go:2352](cmd/execportal/autotrade/engine.go#L2352)) với nút xác nhận từng cặp:
  - mỗi cặp phải kèm **số DỪNG BẢO VỆ mà trang đã hiển thị**; **một** số cũ thì từ chối **cả lô** (409), không cặp nào được giải phóng;
  - cặp halt mới phát sinh sau khi trang render: **không** được xác nhận (`still_halted`);
  - **DỪNG BẢO VỆ của cả bot** (ví dụ sau KILL): **không** xoá, vẫn xác nhận bằng XÁC NHẬN & TẮT;
  - mọi cặp được giải phóng về **TẠM DỪNG**. Vị thế còn trên sàn được tiếp nhận ở lượt quét sau. Không lệnh nào được gửi.
- Route `POST /api/autotrade/ack-all`, header `X-Execportal-Action: autotrade-ack-all`: [server.go:111](cmd/execportal/server.go#L111). Handler: [autotrade.go:762](cmd/execportal/autotrade.go#L762). Handler từ chối (400) body rỗng, thiếu `halt_seq`, symbol ngoài danh sách, symbol lặp.
- Frontend:
  - nút `⚡ XÁC NHẬN TẤT CẢ (N CẶP)` trong khối cảnh báo DỪNG BẢO VỆ: [autotrade.js:113](static/js/autotrade.js#L113). Nút mang danh sách `(symbol, halt_seq)` **đúng như lúc render**. Khối `role=alert` chỉ dựng lại khi nội dung đổi, để nút không mất focus bàn phím sau mỗi lần poll;
  - hộp xác nhận liệt kê từng cặp kèm lý do, rồi POST: [execution.js:1195](static/js/execution.js#L1195).
- Guard tests đã ghim handler mới (`guard_test.go`). `TestUI_OnlyTheExecutionTabWrites` nay cấm `autotrade-ack-all` ngoài `execution.js`. `TestUI_EveryOrderLeadingWriteFollowsItsDialog` buộc POST đi sau hộp xác nhận.

## 3. R6: van spread tức thời trước khi bắn lệnh đóng MARKET chốt lời

- `Trader.Close` nhận `CloseOrder` ([engine.go:93](cmd/execportal/autotrade/engine.go#L93)). **Chỉ** lối thoát mà *mọi* lý do đều là chốt lời mới gắn `MaxSpreadBps = cfg.MaxExitSpreadBps` (mặc định 10 bps): [engine.go:1077](cmd/execportal/autotrade/engine.go#L1077). Cắt lỗ basis, funding âm, thoát khẩn cấp khi halt, KILL, DỪNG & ĐÓNG, ĐÓNG CẶP và nút đóng tay đều gửi 0, tức không có van.
- `portal.close` đọc lại hai sổ ngay trước khi gửi, rồi `closeGuard.deferVI` ([actions.go:539](cmd/execportal/actions.go#L539), gọi tại [actions.go:599](cmd/execportal/actions.go#L599)):
  - spread spot hoặc perp **> trần** → hoãn: *"Spread sổ lệnh tức thời bị giãn (Spot x bps, Perp y bps > trần 10.0 bps) — hoãn đóng để bảo vệ lợi nhuận"*;
  - **không đo được** spread (thiếu bid/ask, sổ bắt chéo) → hoãn: *"không đo được spread tức thời… hoãn chốt lời để tránh trượt giá mù"*;
  - hoãn thì không gửi lệnh nào, file ý định giữ nguyên, trả `Deferred=true` (kèm `Refused=true`), HTTP 409.
- Engine coi `Deferred` là **không phải lỗi giao dịch**: không tăng `tradeFailures`, nên không bao giờ halt vì chờ spread. Log được khử trùng lặp theo loại hoãn (giãn / không đo được), lượt quét sau định giá lại trên sổ mới: [engine.go:1637](cmd/execportal/autotrade/engine.go#L1637).
- Spread tính lại từ giá bid/ask tốt nhất (`touchSpreadBps`), không đọc `SpreadPct` dẫn xuất.

## 4. Chỗ tôi làm KHÁC đặc tả, và lý do

1. **Phân loại chốt lời bằng cờ kiểu dữ liệu, không bằng `strings.Contains(reasonVI, "Chốt lời")`.** Khi cắt lỗ basis và chốt lời nổ **cùng lượt**, lý do ghép chứa cả chữ "Chốt lời". So chuỗi sẽ **hoãn một lệnh cắt lỗ** vì spread, đúng điều ràng buộc 3 cấm.
2. **`AckAll` nhận danh sách `(symbol, halt_seq)` trang đã hiển thị, không phải `AckAll()` trơn.** Hệ thống đã có luật `ErrStaleAcknowledgement` (qua nhiều vòng review): chỉ xác nhận thứ đã đọc. Bản không tham số sẽ xoá cả một halt mới về lệch chân phát sinh giữa lúc render và lúc bấm.
3. **`AckAll` không đưa cả bot từ `EMERGENCY_HALTED` về `RUNNING`.** Halt của bot sinh ra từ KILL hoặc DỪNG & ĐÓNG không phẳng. Bật lại bằng Ack All sẽ lách luật "Start chỉ từ TẮT", và mọi cặp lúc đó đã bị rút khỏi run. Halt của bot vẫn xác nhận bằng XÁC NHẬN & TẮT.
4. **Hoãn không bị đếm vào `tradeFailures`.** Nếu dùng `Refused` như đặc tả, 5 lần hoãn liên tiếp sẽ halt cặp chỉ vì sổ giãn.
5. **Không hardcode 10 bps ở portal.** Trần lấy từ `MaxExitSpreadBps` của cặp (mặc định 10.0, có trên form), nên `0` tắt cả van lúc quét lẫn van trước khi gửi.
6. Nút dùng class có sẵn `btn primary small` (dự án không có `btn-warning btn-sm`).

## 5. Kiểm thử

### Test mới hoặc viết lại (20)
| Test | Chứng minh |
|---|---|
| `TestPortfolio_AReadHaltedHedgedPairStillTakesTheBasisStop` | Rớt mạng 5 lần → halt → mạng về → basis +110 bps → lệnh đóng khẩn cấp tự gửi, phẳng, **vẫn halt số mới**, không giữ chỗ, 3 lượt sau không vào lại dù tín hiệu đủ |
| `…StillTakesTheNegativeFundingExit` | Như trên với funding âm |
| `TestPortfolio_AReadHaltedPairDoesNotTakeProfit` | Chốt lời đạt nhưng không gửi, halt giữ nguyên số, trang ghi rõ |
| `…ThatReadsUnhedgedIsReHaltedAndNotTraded` | Đọc thấy lệch → halt số mới, van tắt kể cả khi hedge sạch trở lại |
| `…WhoseReadingIsNotItsCleanHedgeIsReHalted` (3 ca) | Ý định lạ, hai ý định, lệch chân vượt bước → halt số mới, van tắt, 0 lệnh |
| `TestPortfolio_AReadHaltedPairDoesNotTakeTheEpochExit` | Đủ `MaxHoldEpochs` khi halt → không đóng, trang ghi "chờ XÁC NHẬN" |
| `TestPortfolio_ARefusedHaltedRiskExitRetriesQuietly` | Lệnh khẩn cấp bị từ chối 4 lượt → thử lại mỗi lượt, cùng halt, không đếm lỗi, chỉ 2 dòng log; hết từ chối → phẳng, halt số mới |
| `TestPortfolio_AKillOrAckAllDuringAHaltedRiskExit` | Ack All lúc lệnh đang ra sàn → không xác nhận, báo `exit_in_flight`; KILL chờ lệnh xong → phẳng, bot halt |
| `TestPortfolio_AnAcknowledgementBeforeTheSendVoidsAHaltedRiskExit` | Xác nhận chen giữa đọc và gửi → không gửi |
| `TestPortfolio_AHaltedPairWithAPositionIsReadButNeverJudged` (**viết lại**) | Halt về vị thế (5 lần đóng bị từ chối) vẫn không đọc thị trường, không đóng |
| `TestPortfolio_AckAllReleasesEveryHaltTheOperatorRead` | Rỗng, symbol lạ, một số cũ → từ chối cả lô; đúng → 2 cặp TẠM DỪNG, lượt sau tiếp nhận, 0 lệnh mở |
| `TestPortfolio_AckAllLeavesUnlistedHaltsAndTheBotHalt` | Cặp không liệt kê vẫn halt; halt của bot sau KILL không bị xoá |
| `TestEngine_ATakeProfitDeferredByTheLiveSpreadIsRetriedNotCounted` | Spread 15 bps × 7 lượt → không lỗi, không halt, 1 dòng log; spread 1 bps → phẳng |
| `TestEngine_ABasisStopIsSentWhateverTheLiveSpread` | Cắt lỗ basis với spread 15 bps → đóng ngay, `MaxSpreadBps=0` |
| `TestActions_ATakeProfitCloseIsDeferredOnAWideLiveSpreadAndARiskCloseIsNot` | **Portal thật**: chốt lời với spot hoặc perp 15 bps → hoãn, 0 lệnh, file ý định nguyên; cắt lỗ cùng sổ → phẳng |
| `TestCloseGuard_AnUnmeasurableTouchDefersAndATightOneSends` | Bảng: sát, đúng trần, 15 bps, thiếu bid, sổ bắt chéo, không van |
| `TestAutotradeAPI_ATakeProfitWaitsForATightLiveBook` | **Đầu-cuối**: quét thấy sổ sát, lúc gửi sổ perp 15 bps → hoãn, không lỗi; lượt sau sổ sát → phẳng |
| `TestAutotradeAPI_AckAllReleasesTheHaltsThePageListed` | Route: sai header 403, body sai 400, số cũ 409, đúng 200 → 2 cặp TẠM DỪNG, 0 lệnh |
| `TestAutotradeAPI_EveryWriteIsBehindTheWalls` (mở rộng) | `ack-all` sau đủ các lớp chặn header/origin/method |
| `TestUI_*` (mở rộng) | `autotrade-ack-all` chỉ ở `execution.js`, đi sau hộp xác nhận |

### Kiểm tra đột biến (mutation): mỗi quy tắc bị gỡ đều làm test đỏ
| Đột biến | Test đỏ |
|---|---|
| Không lập job rủi ro cho cặp halt | 4 test ReadHalted |
| Cho mọi lối thoát (kể cả chốt lời) chạy khi halt | `…DoesNotTakeProfit` |
| Lệnh khẩn cấp phẳng thì nhả halt | `…BasisStop`, `…NegativeFundingExit` |
| Hoãn bị đếm như lỗi | `…DeferredByTheLiveSpread…` |
| Ack All bỏ kiểm số cũ | `…AckAllReleases…` |
| Bỏ kiểm hiệu lực halt lúc phán xử và lúc gửi | `…BeforeTheSendVoids…` |
| Lệnh đóng của bot bỏ van spread | `…WaitsForATightLiveBook` |
| Không đo được spread thì vẫn gửi | `TestCloseGuard_…` |
| Portal không báo `Deferred` cho engine | `…WaitsForATightLiveBook` (đếm 1 lỗi) |
| *(sau review)* Bỏ nâng cấp halt khi thấy ý định lạ | `…NotItsCleanHedgeIsReHalted` |
| *(sau review)* Bỏ nâng cấp halt khi lệch chân | `…NotItsCleanHedgeIsReHalted` |
| *(sau review)* Cho lối thoát theo mốc chạy khi halt | `…DoesNotTakeTheEpochExit` |
| *(sau review)* Bỏ nhóm `exit_in_flight` của Ack All | `…DuringAHaltedRiskExit` |
| *(sau review)* Bỏ khử trùng lặp log thử lại | `…RetriesQuietly` |

### Kết quả chạy
```
gofmt -l .                     → (trống)
go vet ./...                   → (trống)
go build ./...                 → ok
go test -count=1 -race ./...   → 37 package ok, 0 FAIL, 0 DATA RACE (3 package không có test)
go test -race -count=3 ./cmd/execportal/ ./cmd/execportal/autotrade/ → ok, ok
```

## 5b. Review độc lập (ngữ cảnh sạch, chỉ đọc)

**Không có lỗi chặn.** Người review xác nhận: không đường nào mở vị thế hay chốt lời khi halt; các cuộc đua ack/kill/stop với lệnh khẩn cấp đang bay đều an toàn; chỉ chốt lời thuần mới mang van spread; slot được tính đúng. Các phát hiện và cách xử lý:

| # | Mức | Phát hiện | Đã xử lý |
|---|---|---|---|
| 1 | Major (test) | Xoá luật nâng halt khi thấy ý định lạ hoặc lệch chân mà không test nào đỏ | Thêm test 3 ca; đột biến nay đỏ |
| 2 | Major (test) | Không test cấm lối thoát theo mốc khi halt | Thêm test; đột biến nay đỏ |
| 3 | Minor | Kiểm `haltSeq` trong `sendExit` không với tới được hôm nay | Giữ lại (phòng thủ nhiều lớp), thêm chú thích lý do |
| 4 | Minor | Chưa test KILL giữa lệnh khẩn cấp | Thêm test |
| 5 | Minor | Log hoãn chốt lời khử trùng lặp theo văn bản có số liệu, và khoá không được xoá | Khoá theo **loại** (giãn / không đo được); xoá khi hết chốt lời chờ, khi Stop/Start, khi xác nhận |
| 6 | Minor | Sổ lệnh vĩnh viễn không đo được spread thì chốt lời bị chặn im lặng | **Chưa làm**, ghi ở mục 6 (chỉ mất lãi, không chặn cắt lỗ) |
| 7 | Minor | Ack All báo cặp có lệnh khẩn cấp đang bay là "đã giải phóng" | Thêm nhóm `exit_in_flight`, trang báo "lệnh đóng khẩn cấp đang ra sàn" |
| 8 | Minor | Lệnh khẩn cấp bị từ chối ghi 3 dòng log mỗi lượt | Thử lại im lặng (`haltRiskRetry`), log khi lý do đổi |
| 9 | Minor (UI) | Cờ writable đổi làm dựng lại cả vùng `role=alert` | Bỏ writable khỏi khoá, bật/tắt nút tại chỗ |
| 10 | Ghi chú | Chốt lời cùng lượt với lối thoát theo mốc thì không có van spread | Chủ đích: lối thoát theo mốc không phải lãi chờ được, giữ như trước |

## 6. Chưa làm / còn mở

- **Chưa nghiệm thu qua trang trên testnet:** máy không có Chrome để bấm thử headless, và portal `:8087` đang gánh 8 vị thế nên không được khởi động lại trong phiên này. Nút Ack All và hộp xác nhận mới chỉ được kiểm bằng test tĩnh (cú pháp `node`, guard tests), **chưa render thật**.
- Chưa commit, chưa `codegraph sync`, chưa cập nhật PLAN.md/CLAUDE.md (WORKFLOW P7).
- Van rủi ro chỉ chạy **khi bot đang RUNNING**. Nếu cả bot bị halt hoặc tắt thì không có lượt quét nào.
- Review #6: nếu một sổ lệnh testnet cứ bắt chéo hoặc thiếu mức giá, mọi chốt lời bị hoãn vô hạn (chỉ 1 dòng log). Đề xuất: đếm số lần hoãn "không đo được" liên tiếp và cảnh báo sau N lần. Chưa chọn N.
- R7 (giám sát ký quỹ perp) vẫn chưa có. Van khẩn cấp ở đây là basis và funding, không phải thanh lý.
