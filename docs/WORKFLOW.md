# QUY TRÌNH LÀM VIỆC & REVIEW

> **Cập nhật:** 2026-08-28
> **Áp dụng cho:** mọi thay đổi code trong repo này, do người hay do AI agent thực hiện
> **Liên quan:** [PLAN.md](PLAN.md) · [CONVENTIONS.md](CONVENTIONS.md) · [DATA-REQUIREMENTS.md](DATA-REQUIREMENTS.md)

---

## 0. NGUYÊN TẮC

**Đơn vị công việc là một BƯỚC trong [PLAN.md](PLAN.md).** Không phải "một tính năng", không phải "một buổi làm việc", không phải "một ý tưởng vừa nghĩ ra".

| Quy tắc | Lý do |
|---|---|
| **1 bước = 1 commit** | Lịch sử git đọc được như lộ trình. Revert một bước không kéo theo bước khác |
| **Không bắt đầu bước chưa có tiêu chí nghiệm thu** | Không có tiêu chí thì không có cách nào biết đã xong |
| **Đối chiếu code thật trước khi tin tài liệu** | Tài liệu luôn trôi khỏi thực tế. Code là sự thật |
| **Review trước, commit sau** | Commit là hành động công bố. Không công bố thứ chưa ai soi |
| **Phát hiện ngoài phạm vi → ghi lại, không sửa luôn** | Bước phình ra là bước không bao giờ xong |

**Bốn cổng chặn.** Không qua được thì không đi tiếp, không có ngoại lệ:

```
Cổng 1 (P1)  Hiện trạng khớp giả định của PLAN?     → không: cập nhật PLAN trước
Cổng 2 (P5)  Review PASS?                            → không: quay lại P4
Cổng 3 (P6)  Đủ tiêu chí nghiệm thu của bước?        → không: chưa xong
Cổng 4 (P7)  Tài liệu đã đồng bộ?                    → không: chưa được commit
```

---

## 1. VÒNG LẶP 9 PHA

```
P0  CHỌN BƯỚC       ← đọc PLAN.md, kiểm tra phụ thuộc
P1  ĐỐI CHIẾU       ← codegraph: code THẬT đang làm gì?          [CỔNG 1]
P2  THIẾT KẾ        ← chỉ khi P1 phát hiện lệch, hoặc bước phức tạp
P3  TEST TRƯỚC      ← bắt buộc với mọi logic tính toán
P4  IMPLEMENT
P5  REVIEW          ← self-review + adversarial review           [CỔNG 2]
P6  NGHIỆM THU      ← đối chiếu tiêu chí trong PLAN.md           [CỔNG 3]
P7  CẬP NHẬT TÀI LIỆU ← PLAN, README, CLAUDE.md                  [CỔNG 4]
P8  SYNC + COMMIT   ← codegraph sync, rồi MỘT commit duy nhất
```

---

### P0 — CHỌN BƯỚC

1. Mở [PLAN.md](PLAN.md), chọn bước tiếp theo **theo đúng thứ tự**.
2. Kiểm tra giai đoạn trước đã đạt nghiệm thu chưa. Chưa thì không được nhảy cóc.
3. Đọc **tiêu chí nghiệm thu** của bước. Không có → viết vào PLAN.md trước khi làm gì khác.
4. Tạo nhánh:

```bash
git switch -c step/1.1-staleness-filter
```

**Không làm:** gộp hai bước "cho tiện vì chúng liên quan". Liên quan không phải lý do gộp — nếu thật sự không tách được, sửa PLAN.md để hợp nhất chúng thành một bước, rồi mới làm.

---

### P1 — ĐỐI CHIẾU HIỆN TRẠNG 🚪 CỔNG 1

Pha bị bỏ qua nhiều nhất, và là pha tiết kiệm nhiều thời gian nhất. Mục tiêu: **biết code thật đang làm gì, trước khi tin điều PLAN.md nói nó đang làm gì.**

```bash
# Bức tranh vùng sắp sửa
codegraph explore "price state update and arbitrage check"

# Ngữ cảnh cho đúng nhiệm vụ này
codegraph context "add staleness filter to price map"

# Một symbol cụ thể + ai gọi nó
codegraph node updatePrice

# QUAN TRỌNG NHẤT: đổi cái này thì vỡ cái gì
codegraph impact updatePrice
```

Kết quả pha này là một đoạn ghi chú ngắn trả lời **ba câu**:

1. Code hiện tại thực sự làm gì ở vùng này?
2. PLAN.md giả định gì? Có lệch không?
3. Thay đổi này lan tới đâu (`impact`)? Có vượt ra ngoài phạm vi bước không?

> **🚪 Cổng 1 — nếu hiện trạng lệch với giả định của PLAN:** dừng. Cập nhật PLAN.md **trước**, không code rồi sửa tài liệu sau. Một kế hoạch sai được thực hiện đúng vẫn cho ra kết quả sai.

---

### P2 — THIẾT KẾ

Bỏ qua được nếu bước nhỏ và P1 không phát hiện gì bất ngờ.

Bắt buộc khi: đổi kiểu dữ liệu dùng chung, thêm package, chạm ranh giới phụ thuộc, hoặc `codegraph impact` trả về nhiều hơn ~5 điểm gọi.

Ghi ra **trước khi code**, ngắn thôi:
- Chữ ký hàm / struct sẽ đổi
- Cái gì **không** đổi (quan trọng ngang cái đổi)
- Điểm nào có thể vỡ, xử lý ra sao

---

### P3 — TEST TRƯỚC

**Bắt buộc** với: mọi phép tính tài chính, mọi chuyển đổi đơn vị, mọi hàm parse payload sàn.

**Không bắt buộc** với: đổi cấu trúc thư mục, sửa comment, đổi tên thuần tuý.

```bash
# Payload thật của sàn lưu vào testdata/, không bịa
exchanges/testdata/binance_markprice.json
```

Viết test **thất bại trước**, xem nó thất bại đúng lý do mình nghĩ, rồi mới implement. Test pass ngay từ đầu là test không kiểm tra gì cả.

---

### P4 — IMPLEMENT

- Bám [CONVENTIONS.md](CONVENTIONS.md), đặc biệt **§1 hậu tố đơn vị**.
- Chạy `gofmt` và `go vet` liên tục, không để dồn.
- **Commit vặt thoải mái ở local** — sẽ squash ở P8. Đừng để nỗi lo "1 commit" cản trở việc lưu tiến độ.

**Khi phát hiện việc ngoài phạm vi** (bug khác, code xấu, ý tưởng hay):

```
KHÔNG sửa ngay.
→ Ghi vào PLAN.md §14 (nợ) hoặc mục "Chưa có gì", gắn giai đoạn xử lý.
→ Nếu là bug CHẶN bước hiện tại: dừng, quay lại P0, làm bước sửa bug riêng.
```

Đây là ranh giới giữa senior và junior: không phải kỹ năng viết code, mà là kỷ luật **không** viết code chưa tới lượt.

---

### P5 — REVIEW 🚪 CỔNG 2

Hai lớp, không thay thế nhau.

#### 5.1. Self-review

```bash
gofmt -l .                    # phải không in ra gì
go vet ./...
go test ./...
go test -race ./...           # bắt buộc nếu đụng goroutine
go build ./...

git diff --stat               # phạm vi có đúng bằng bước không?
codegraph impact <symbol_đã_đổi>   # có chạm ai ngoài dự kiến không?
```

Đọc lại **toàn bộ diff của chính mình** trước khi đưa cho ai khác. Không đọc diff của mình là bất lịch sự với người review.

#### 5.2. Adversarial review

Review phải chạy trong **ngữ cảnh sạch** — người/agent chưa viết code này. Người vừa viết xong luôn đọc thấy ý định của mình chứ không đọc thấy chữ trên màn hình.

```
/code-review              # review diff hiện tại
/code-review high         # bước chạm tiền, credential, hoặc concurrency
```

#### 5.3. Checklist review

| # | Trục | Câu hỏi |
|---|---|---|
| 1 | **Đúng đắn** | Có trường hợp biên nào sai không? Chia cho 0? Map rỗng? Sàn trả null? |
| 2 | **Đơn vị** ⚠️ | Mọi biến mang đơn vị có hậu tố chưa? Có chỗ nào `rate`/`size`/`interval` trần trụi qua ranh giới hàm không? |
| 3 | **Ranh giới** | `exchanges/` có import `internal/` không? `broker/` có rò vào luồng ingest không? → **chặn merge** |
| 4 | **Đồng thời** | Goroutine có điều kiện thoát? Có giữ lock khi gọi I/O? `-race` sạch? |
| 5 | **Lỗi** | Có nuốt lỗi im lặng? Có bọc `%w`? Có panic trong thư viện? |
| 6 | **Test** | Logic tài chính có test? Có golden test cho connector mới? |
| 7 | **Tài liệu** | Bẫy đã biết của sàn có comment tại chỗ + link tài liệu? |
| 8 | **Phạm vi** | Diff có đúng bằng bước không? Có "tiện tay sửa luôn" không? |
| 9 | **Số liệu** | Có chỗ nào hiển thị lợi nhuận **thô** như thể là ròng? |

> **🚪 Cổng 2 — review FAIL:** quay lại P4. Không commit, không "để sau sửa", không "TODO trong code". Sửa xong review lại từ đầu.

---

### P6 — NGHIỆM THU 🚪 CỔNG 3

Mở PLAN.md, đọc mục **Nghiệm thu** của bước, **thực hiện đúng phép thử đó** — không phải phép thử tương tự, không phải "chắc là được".

Ví dụ Bước 1.1 ghi *"ngắt mạng 1 sàn → sàn đó biến mất khỏi ma trận trong ≤5s, không sinh cảnh báo giả"* → phải thật sự ngắt mạng sàn đó và bấm giờ.

Ghi lại kết quả để đưa vào commit message.

> **🚪 Cổng 3 — không đạt:** bước chưa xong. Quay lại P4.

---

### P7 — CẬP NHẬT TÀI LIỆU 🚪 CỔNG 4

Tài liệu là một phần của bước, **không phải việc dọn dẹp sau đó**.

| File | Cập nhật khi |
|---|---|
| **[PLAN.md](PLAN.md)** | **Luôn luôn** — đánh dấu bước xong, cập nhật checklist phụ lục, sửa bảng "Hiện trạng" nếu khoảng trống đã được lấp |
| **[README.md](README.md)** | Khi hành vi mà người dùng thấy thay đổi, hoặc khi một mục trong "Chưa làm được gì" đã xong |
| **[CLAUDE.md](CLAUDE.md)** | Khi thêm luật mới, phát hiện bẫy mới, hoặc điểm yếu trong danh sách đã được sửa |
| **[DATA-REQUIREMENTS.md](DATA-REQUIREMENTS.md)** | Khi xác minh được một mục 🟡, hoặc phát hiện đặc tính mới của sàn |
| **[CONVENTIONS.md](CONVENTIONS.md)** | Khi đặt ra quy ước mới, hoặc trả xong một khoản nợ ở §14 |

**Cụ thể trong PLAN.md, mỗi bước xong phải sửa 3 chỗ:**

```
1. Tiêu đề bước:      #### Bước 1.1 — ...            →  #### Bước 1.1 — ... ✅
2. Bảng tổng hợp:     | 1 | Củng cố lõi | 6 | ...    →  cập nhật trạng thái
3. Checklist phụ lục: [  ] GĐ 1 ... 0/6 bước          →  [  ] GĐ 1 ... 1/6 bước
```

> **🚪 Cổng 4 — tài liệu chưa đồng bộ:** chưa được commit. Một PLAN.md nói sai về thực tại còn tệ hơn không có PLAN.md, vì nó khiến người sau tin nhầm.

---

### P8 — SYNC + COMMIT

Thứ tự bắt buộc: **sync trước, commit sau** — để đồ thị phản ánh đúng code sắp được commit.

```bash
# 1. Cập nhật code graph
codegraph sync
codegraph status          # xác nhận số node/edge đã đổi hợp lý

# 2. Gộp về MỘT commit duy nhất cho bước này
git add -A
git commit                # nếu đây là commit đầu của nhánh
# hoặc, nếu đã có commit vặt ở local:
git reset --soft $(git merge-base HEAD main) && git commit

# 3. Đưa về main
git switch main
git merge --ff-only step/1.1-staleness-filter
git branch -d step/1.1-staleness-filter
```

`.codegraph/` đã nằm trong `.gitignore` — chỉ số không bao giờ đi vào commit.

#### Định dạng commit

Tiếng Anh, thức mệnh lệnh, dòng đầu ≤ 72 ký tự.

```
<type>(<scope>): <summary>

Step <G.S> of the roadmap.

<Vì sao thay đổi, không phải làm gì — diff đã nói làm gì.>

Acceptance:
- <tiêu chí 1> — verified by <cách kiểm>
- <tiêu chí 2> — verified by <cách kiểm>

Docs: PLAN.md, README.md
```

**Ví dụ thật:**

```
feat(scanner): drop stale prices from arbitrage calculation

Step 1.1 of the roadmap.

Price state kept only the value, so a venue that dropped its WebSocket
kept contributing a frozen price to the spread matrix indefinitely and
could raise alerts on a market that no longer existed. State now carries
the receive time and anything older than the configured threshold is
excluded and surfaced as STALE in the UI.

Acceptance:
- Venue disconnect removes it from the matrix within 5s — verified by
  blocking fstream.binance.com and timing the UI
- No alerts raised from stale data — verified by 10min soak with one
  venue blocked

Docs: PLAN.md, README.md
```

**Type:** `feat` `fix` `refactor` `test` `docs` `perf` `chore`
**Scope:** `scanner` `exchanges` `instruments` `fees` `store` `strategy` `backtest` `notify` `broker` `execution` `risk` `docs`

#### Sửa sau khi đã commit nhưng chưa merge

```bash
git commit --amend        # KHÔNG tạo commit "fix review comment"
```

Một bước để lại đúng một commit trong lịch sử. Commit sửa lỗi review là nhiễu, không phải lịch sử.

---

## 2. ĐỊNH NGHĨA HOÀN THÀNH

Một bước chỉ được coi là xong khi **tất cả** đúng:

```
[ ] Tiêu chí nghiệm thu trong PLAN.md đã thực hiện và đạt
[ ] gofmt sạch, go vet sạch, go test xanh, -race sạch (nếu đụng goroutine)
[ ] Adversarial review PASS
[ ] Không vi phạm luật phụ thuộc (CONVENTIONS §12.1)
[ ] Mọi biến mang đơn vị có hậu tố đơn vị
[ ] PLAN.md đã cập nhật 3 chỗ
[ ] Tài liệu liên quan khác đã đồng bộ
[ ] codegraph sync đã chạy
[ ] Đúng MỘT commit trên main cho bước này
```

Thiếu một dòng → chưa xong. "Gần xong" không phải một trạng thái.

---

## 3. KHI MỌI THỨ KHÔNG THEO KẾ HOẠCH

| Tình huống | Xử lý |
|---|---|
| P1 phát hiện PLAN sai | Cập nhật PLAN **trước**, ghi lý do vào commit. Có thể tách thành commit `docs:` riêng |
| Bước hoá ra to gấp đôi dự kiến | Dừng. Tách thành 2 bước trong PLAN.md. Không âm thầm làm bước khổng lồ |
| Phát hiện bug chặn bước hiện tại | Dừng bước. Làm commit `fix:` riêng trước. Rồi quay lại |
| Phát hiện bug **không** chặn | Ghi vào PLAN.md, đi tiếp |
| Review FAIL 3 lần liên tiếp | Vấn đề nằm ở thiết kế, không ở code. Quay lại P2 |
| Tiêu chí nghiệm thu không kiểm được | Tiêu chí sai. Viết lại tiêu chí trước, đừng hạ chuẩn |
| Sàn đổi API giữa chừng | Cập nhật DATA-REQUIREMENTS.md cùng commit sửa code |

---

## 4. TRA CỨU NHANH

```bash
# ── CodeGraph ───────────────────────────────────────
codegraph explore "<vùng>"      # P1: bức tranh tổng thể
codegraph context "<nhiệm vụ>"  # P1: ngữ cảnh cho task
codegraph node <symbol>         # P1: source + caller/callee
codegraph impact <symbol>       # P1/P5: đổi cái này vỡ cái gì
codegraph callers <symbol>
codegraph query "<từ khoá>"
codegraph files
codegraph sync                  # P8: BẮT BUỘC trước commit
codegraph status

# ── Kiểm tra ────────────────────────────────────────
gofmt -l . && go vet ./... && go test ./... && go build ./...
go test -race ./...

# ── Git ─────────────────────────────────────────────
git switch -c step/<G.S>-<slug>
git diff --stat
git reset --soft $(git merge-base HEAD main) && git commit   # squash
git switch main && git merge --ff-only step/<G.S>-<slug>
```

---

## 5. VÍ DỤ MỘT VÒNG HOÀN CHỈNH — BƯỚC 1.1

```bash
# P0 — chọn bước
#   PLAN.md GĐ1 Bước 1.1: staleness filter
#   Nghiệm thu: ngắt mạng 1 sàn → biến mất khỏi ma trận ≤5s, không cảnh báo giả
git switch -c step/1.1-staleness-filter

# P1 — đối chiếu 🚪
codegraph explore "price state and spread matrix"
codegraph impact updatePrice
#   Phát hiện: Timestamp bị vứt ở main.go:63-72 — khớp giả định của PLAN. Qua cổng.

# P2 — thiết kế
#   map[string]float64 → map[string]PricePoint{Price, VenueTimeMs, RecvAt}
#   Không đổi: hợp đồng JSON với frontend

# P3 — test trước
#   TestPriceState_ExcludesStaleVenue  → đỏ

# P4 — implement  (commit vặt local thoải mái)

# P5 — review 🚪
gofmt -l . && go vet ./... && go test -race ./...
codegraph impact updatePrice        # xác nhận không lan ngoài dự kiến
# /code-review                      → PASS

# P6 — nghiệm thu 🚪
#   Chặn fstream.binance.com, bấm giờ: biến mất sau 5s. Soak 10 phút: 0 cảnh báo giả.

# P7 — tài liệu 🚪
#   PLAN.md: Bước 1.1 ✅, checklist 0/6 → 1/6, bảng hiện trạng bỏ mục #6
#   README.md: bỏ "Lọc dữ liệu cũ" khỏi bảng "Chưa làm được gì"
#   CLAUDE.md: bỏ gạch đầu dòng về Timestamp trong "Known weaknesses"

# P8 — sync + commit
codegraph sync
git reset --soft $(git merge-base HEAD main) && git commit
git switch main && git merge --ff-only step/1.1-staleness-filter
git branch -d step/1.1-staleness-filter
```

---

## PHỤ LỤC — TÌNH TRẠNG HIỆN TẠI CỦA REPO

Tại thời điểm viết tài liệu này, repo **chưa ở trạng thái quy trình trên mô tả**:

- Lịch sử git chỉ có 1 commit (`11acd1a`).
- Toàn bộ tài liệu và cấu trúc `internal/` **đang chưa commit**.
- 9 file Go hiện có **chưa gofmt-clean** (171 dòng khoảng trắng, không đổi ngữ nghĩa).

**Cách đưa về trạng thái chuẩn trước khi bắt đầu Bước 1.1** — đây là công việc nền của Giai đoạn 0, không thuộc bước nào, nên gom thành hai commit theo chủ đề:

```bash
# 1. Chuẩn hoá định dạng — tách riêng để diff của các bước sau sạch
gofmt -w main.go exchanges/*.go
codegraph sync
git add -A && git commit -m "chore: apply gofmt to existing sources"

# 2. Nền tài liệu và cấu trúc package
codegraph sync
git add -A && git commit -m "docs: add roadmap, data requirements, conventions and workflow"
```

Tách gofmt thành commit riêng là có chủ đích: trộn 171 dòng thay đổi khoảng trắng vào một commit có ý nghĩa sẽ khiến diff đó không đọc được, và mọi `git blame` sau này đều trỏ nhầm.

Từ Bước 1.1 trở đi, áp dụng đúng quy tắc **1 bước = 1 commit**.
