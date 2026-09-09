# tools/report — dựng báo cáo HTML từ kết quả `cmd/backtest`

Python **chỉ đọc**, chạy ngoài tiến trình Go, đúng ranh giới CLAUDE.md quy tắc 8:
nó đọc CSV mà `cmd/backtest` đã ghi và đọc SQLite ở chế độ `mode=ro`. Không
script nào ở đây quyết định hay thực thi một lệnh giao dịch, và không script nào
tính lại luật vào/ra — mọi con số lợi nhuận đều do `internal/strategy` sinh ra
qua `internal/backtest`.

Không dùng thư viện ngoài (kể cả PyYAML): chỉ thư viện chuẩn, vì máy chạy các
script này cũng là máy đang chạy tiến trình live.

## Bốn bước

| Script | Việc |
|---|---|
| `prep.py` | Nén CSV trước khi phân tích: bỏ `assumptions_vi` lặp trên mọi dòng, đổi `exit_reason_vi` thành nhãn phân loại. Một lưới 12.288 bộ ghi ~5,5 GB; không có bước này `analyze.py` chạm 8,2 GB RSS và bị kill. |
| `analyze.py` | Gộp CSV + corpus SQLite thành MỘT file JSON. Đọc `runs` vào bộ nhớ, còn `trades` thì **duyệt theo dòng** và chỉ giữ sáu mảng `array('d')` — 4,6 triệu lệnh vừa trong ~1,5 GB. |
| `lev.py` | Khối đòn bẩy. Tách riêng vì nó chỉ được so **những chuỗi mở được lệnh ở MỌI mức đòn bẩy**: bật mô hình ký quỹ làm `strategy` từ chối vào lệnh ở sàn chưa xác minh biểu duy trì, nên các dòng của lưới chính không so ngang được theo trục đó. Nó cũng quy lợi nhuận về **VỐN** = N·(1+f), mẫu số duy nhất trả lời được câu "có nên dùng đòn bẩy không". |
| `iso.py` | So hai lượt chạy thường chỉ khác ngưỡng basis, ra giá của lối thoát đó. |
| `build.py` | JSON + `report.template.html` → một file HTML tự chứa, song ngữ VI/ZH. |

## Chạy

```bash
S=/tmp/bt                      # thư mục làm việc, đặt đâu cũng được
go build -o $S/backtest ./cmd/backtest

for M in 12 6 3; do
  $S/backtest -sweep -months $M \
    -min-rate-bps 0.3,0.5,0.8,1.0 -persist 1,2,3,6 -min-net-apr 0.02 \
    -exit-net-apr 0,0.005 -exit-persist 3,12,48 -notional 50000 -hold-days 30,90 \
    -exit-neg-bps 0,0.5,1.0,2.0 -exit-neg-periods 1,2,3,6 -exit-neg-cum 0,0.25,0.5,1.0 \
    -top 40 -csv $S/wide$M.csv -trades-csv $S/trades$M.csv > $S/wide$M.txt 2> $S/wide$M.err
  python3 tools/report/prep.py $S/wide$M.csv   $S/wide$M.slim.csv   runs
  python3 tools/report/prep.py $S/trades$M.csv $S/trades$M.slim.csv trades
done

python3 tools/report/analyze.py --db data/scanner.db --config config.yaml \
  --window 12=$S/wide12.slim.csv:$S/trades12.slim.csv:$S/wide12.err \
  --window 6=$S/wide6.slim.csv:$S/trades6.slim.csv \
  --window 3=$S/wide3.slim.csv:$S/trades3.slim.csv \
  --out $S/wide.json

# Khối đòn bẩy: một lượt quét riêng chỉ đổi trục ký quỹ.
$S/backtest -sweep -months 12 -perp-margin 0,0.5,0.33,0.2,0.1,0.05 -liq-buffer 2 \
  -min-rate-bps 0.3 -persist 6 -min-net-apr 0.02 -exit-net-apr 0 -exit-persist 48 \
  -exit-neg-bps 2.0 -exit-neg-periods 2 -exit-neg-cum 1.0 -min-hold 0 \
  -notional 50000 -hold-days 90 -top 1 -csv $S/margin.csv > /dev/null
python3 tools/report/lev.py --csv $S/margin.csv --config config.yaml --out $S/leverage.json

# Giá của lối thoát basis: hai lượt chạy THƯỜNG, chạy sát nhau (xem ghi chú
# trong iso.py về vì sao khoảng cách thời gian giữa hai lượt là quan trọng).
sed -e 's/^  max_basis_pct: .*/  max_basis_pct: 1000.0/' \
    -e 's/^  max_basis_widen_pct: .*/  max_basis_widen_pct: 1000.0/' config.yaml > $S/config-nobasis.yaml
$S/backtest -months 12 -csv $S/p-with.csv    > /dev/null
$S/backtest -months 12 -csv $S/p-without.csv -config $S/config-nobasis.yaml > /dev/null
python3 tools/report/iso.py --on $S/p-with.csv --off $S/p-without.csv --out $S/isolation.json

python3 tools/report/build.py --json $S/wide.json --leverage $S/leverage.json \
  --isolation $S/isolation.json --commit $(git rev-parse --short HEAD) --gates 2.0,2,1.0 \
  --title "Phán quyết backtest <bước>" \
  --out docs/reports/backtest-<mô-tả>-<ngày>.html
```

`--title` đặt thẻ `<title>` tĩnh. **Mỗi báo cáo phải có tên riêng**: đó là tên
hiện trên tab trình duyệt và trong danh sách, và hai trang cùng tên thì không
phân biệt được trang nào là lượt đo nào.

`--leverage` và `--isolation` đều tuỳ chọn; thiếu thì mục tương ứng biến mất
thay vì hiện số rỗng.

## Báo cáo trục giữ (`hold.py` + `hold_build.py`)

Một cặp script riêng, cùng ranh giới đọc-only, trả lời một câu hỏi hẹp: *giữ
mỗi lệnh bao lâu thì hoà vốn, và trục giữ nào (holding_days, cửa sổ suy giảm N,
cổng giữ tối thiểu M, cổng đảo dấu) làm được việc đó*. Nó đo thêm một thứ lưới
chính không đo: **chân trời hoà vốn** đọc thẳng từ `funding_history` — với mỗi
mốc settle làm điểm vào, bao nhiêu ngày cho tới khi funding thu được bằng chi
phí vòng Go đã định giá. Đó là phép đo corpus, không phải luật, và nó nói mọi
luật giữ phải đối mặt với cái gì.

```bash
$S/backtest -sweep -months 12 \
  -min-rate-bps 0.3,0.5 -persist 6 -min-net-apr 0.02 \
  -exit-net-apr 0,0.005 -exit-persist 12,48,96,192 -notional 50000 -hold-days 30,60,90,180,365 \
  -exit-neg-bps 0,2.0 -exit-neg-periods 1,2 -exit-neg-cum 0,1.0 -min-hold 0,0.5,1.0,1.5,2.0 \
  -top 40 -csv $S/hold12.csv -trades-csv $S/holdtrades12.csv > $S/hold12.txt 2> $S/hold12.err
# (lặp với -months 6 nếu muốn cửa sổ thứ hai; hold.py đọc CSV thô, không cần prep.py)
python3 tools/report/hold.py --db data/scanner.db --config config.yaml \
  --window 12=$S/hold12.csv:$S/holdtrades12.csv --window 6=$S/hold6.csv:$S/holdtrades6.csv \
  --out $S/hold.json
python3 tools/report/hold_build.py --json $S/hold.json --commit $(git rev-parse --short HEAD) \
  --title "Chân trời hoà vốn <ngày>" --out docs/reports/backtest-hold-<ngày>.html
```

`hold.py` lấy bộ đang ship từ khối `strategy:` của `config.yaml` (đọc theo
dòng, không PyYAML) và đặt nó vào lưới: trang có bảng "đổi đúng MỘT trục từ bộ
ship", là phép đo thao tác viên thật sự làm, bên cạnh trung bình lưới. Lưới
phải CHỨA bộ ship trên mọi trục, nếu không mục đó và sổ lệnh của nó biến mất.

## Sàng cặp (`cmd/pairscreen` + `pairscreen.py`)

Mở rộng danh sách cặp mà không bỏ qua đánh giá rủi ro: nửa SÀN do Go đo trực
tiếp, nửa CORPUS do Python đọc, và một phán quyết sáu điều kiện cho từng tổ hợp
cặp × sàn perp — đánh giá hết, không dừng ở điều kiện đầu tiên rớt.

```bash
# 1. một bản config với danh sách ứng viên (chỉ thêm dòng vào `symbols:`)
# 2. corpus funding 12 tháng cho ứng viên — bỏ paradex (chỉ số liên tục, backtest từ chối)
for src in binance_futures bybit_futures okx_futures gate_futures kraken_futures hyperliquid_futures; do
  go run ./cmd/backfill -config $S/config-screen.yaml -months 12 -source $src
done
# 3. phía sàn: registry, ánh xạ hedge, sổ lệnh 9 nguồn, vòng phí ở 50k
go run ./cmd/pairscreen -config $S/config-screen.yaml -notional 50000 -out $S/screen.json
# 4. ghép hai nửa, phán quyết, dựng trang
python3 tools/report/pairscreen.py --db data/scanner.db --screen $S/screen.json --out $S/pairscreen.json
python3 tools/report/hold_build.py --json $S/pairscreen.json --template tools/report/pairscreen.template.html \
  --commit $(git rev-parse --short HEAD) --title "Sàng cặp <ngày>" --out docs/reports/pairscreen-<ngày>.html
```

Ngưỡng funding mặc định (`--min-bps 0.94`) suy từ mục tiêu: 5%/năm trên VỐN ở
K = 2 sau vòng 0,30% ⇒ gộp ≥ 10,3%/năm ⇒ 0,94 bps/8h. Nó là ngưỡng của mục
tiêu, không phải điều kiện vào lệnh. "Sàn tốt nhất" của một cặp ưu tiên sàn có
corpus ≥ `--min-cover-days` (okx ~96 ngày, gate ~180) rồi mới so funding —
trung bình 96 ngày không so được với 365 ngày.

## Bộ ngưỡng trên vũ trụ mới (`expand.py`)

Sau khi danh sách cặp đổi, câu hỏi không còn là "bộ nào tốt nhất" mà là "bộ
đang ship có còn chạy được trên vũ trụ mới không". `expand.py` đọc MỘT lượt
chạy thường (không `-sweep`) cho mỗi cửa sổ — tức đúng khối `strategy:` của
`config.yaml`, không phải một bộ trong lưới — và so nó với mốc giữ suốt của
chính từng chuỗi. Nó dùng lại `corpus()`, `load_runs()`, `stream_trades()`,
`set_row()` và `marginals()` của `hold.py`, nên không có luật nào được tính
lại ở đây.

```bash
S=/tmp/bt13
go build -o $S/backtest ./cmd/backtest
for M in 12 6 3; do
  $S/backtest -months $M -csv $S/runs$M.csv -trades-csv $S/trades$M.csv
done
$S/sweep.sh          # lưới quanh bộ ship, 12 tháng — lưới PHẢI chứa bộ ship
python3 tools/report/expand.py --db data/scanner.db --config config.yaml \
  --window 12=$S/runs12.csv:$S/trades12.csv \
  --window 6=$S/runs6.csv:$S/trades6.csv \
  --window 3=$S/runs3.csv:$S/trades3.csv \
  --sweep $S/sweep12.csv:$S/sweeptrades12.csv --out $S/expand.json
python3 tools/report/hold_build.py --json $S/expand.json \
  --template tools/report/expand.template.html --commit $(git rev-parse --short HEAD) \
  --title "Bộ ngưỡng trên 13 cặp" --out docs/reports/backtest-expansion-<ngày>.html
```

Ba điều kiện trước khi chạy, vì cả ba đều đã hụt ở lần đầu:

1. **Cặp mới cần ảnh sổ lệnh và ảnh instrument**, không chỉ corpus funding.
   `cmd/backtest` lấy chi phí vòng từ `depth_snapshots` (30 ngày gần nhất) và
   lấy ánh xạ hedge từ `instrument_snapshots` của ngày mới nhất — cả hai do
   `cmd/scanner` ghi. Chạy một tiến trình scanner riêng với `strategy.enabled:
   false` và một cổng khác trong ~5 phút là đủ: nó ghi cả hai, không ghi
   `signal_journal`, và **không được đụng tiến trình 3.5 đang chạy**.
2. **Nến giờ cho cặp mới** (`cmd/backfill -prices -symbol <cặp>`), nếu không
   lối thoát basis không được xét ở mọi mốc của chuỗi đó.
3. `expand.py` **từ chối** một CSV chứa nhiều hơn một bộ tham số, và từ chối
   một CSV mà bộ tham số không khớp `config.yaml` — trang này nói "bộ đang
   ship", nên nó phải chứng minh được điều đó.

## Bộ ngưỡng đã áp dụng (`applied.py`)

Sau khi người vận hành đổi khối `strategy:` (2026-09-09: basis 1,0/0,5 →
2,0/2,0, `min_hold_recovered_cost_frac` 0 → 1), câu hỏi là "bộ đã áp dụng
làm được gì, và từng thay đổi mua được gì". `applied.py` dùng lại
`expand.window()` cho ba cửa sổ, rồi thêm một DANH SÁCH phép so — mỗi phép
là một lượt chạy thường trên cùng vũ trụ, chỉ khác điều được nêu tên trong
một bản sao config, trừ nhau bằng `expand.isolation()`. Chênh trên trang là
"bộ đã áp dụng trừ bộ kia" (`delta_sign` trong JSON nói vậy), ngược dấu với
`expand.py`. `--sweep` là lưới quanh bộ đã áp dụng, nay có cả trục basis và
M vì `cmd/backtest` đã nhận chúng.

```bash
S=/tmp/applied
go build -o $S/backtest ./cmd/backtest
for M in 12 6 3; do
  $S/backtest -months $M -csv $S/runs$M.csv -trades-csv $S/trades$M.csv
done
git show <commit-trước>:config.yaml > $S/old.yaml            # bộ cũ, lấy từ git
sed 's/^  max_basis_pct: 2.0 /  max_basis_pct: 100 /; s/^  max_basis_widen_pct: 2.0 /  max_basis_widen_pct: 100 /' config.yaml > $S/nobasis.yaml
sed 's/^  min_hold_recovered_cost_frac: 1.0$/  min_hold_recovered_cost_frac: 0/' config.yaml > $S/m0.yaml
for f in old nobasis m0; do
  $S/backtest -config $S/$f.yaml -months 12 -csv $S/iso-$f.csv -trades-csv $S/isotrades-$f.csv
done
$S/backtest -sweep -months 12 -min-rate-bps 0.3,0.5,0.8 -persist 3,6 -min-net-apr 0.02 -exit-net-apr 0 \
  -exit-persist 24,48,96 -exit-neg-bps 2.0 -exit-neg-periods 2 -exit-neg-cum 1.0 -min-hold 0,1.0 \
  -notional 50000 -hold-days 90 -max-basis 1.0,2.0,100 -max-basis-widen 1.0,2.0,100 \
  -csv $S/sweep12.csv -trades-csv $S/sweeptrades12.csv        # 324 bộ, PHẢI chứa bộ đã áp dụng
python3 tools/report/applied.py --db data/scanner.db --config config.yaml \
  --window 12=$S/runs12.csv:$S/trades12.csv --window 6=... --window 3=... \
  --compare 12:old=$S/iso-old.csv:$S/isotrades-old.csv --label "old=<tiếng Việt>|<中文>" \
  --compare 12:nobasis=... --label "nobasis=..." --compare 12:m0=... --label "m0=..." \
  --sweep $S/sweep12.csv:$S/sweeptrades12.csv --out $S/applied.json
python3 tools/report/hold_build.py --json $S/applied.json --template tools/report/applied.template.html \
  --commit $(git rev-parse --short HEAD) --title "Bộ ngưỡng đã áp dụng" --out docs/reports/backtest-applied-<ngày>.html
```

Mỗi `--compare` bắt buộc có `--label` cùng khoá: CSV nói bộ kia là gì (cột
tham số) nhưng không nói VÌ SAO nó khác, và trang phải nêu được điều đó. Phép
so có khoá `old` được phán quyết trích riêng ("bộ cũ mà nó thay làm ...").
`applied.template.html` sinh từ `expand.template.html` — cùng khung, mục "giá
một lối thoát" thay bằng danh sách phép so, nhãn bộ ngưỡng thêm `B basis/dịch`.

## Bộ ngưỡng vốn và rủi ro (`capital.py`)

Xếp hạng một lưới theo hai thứ mà các trang trước không có: **vốn danh mục**
(mọi ô chuỗi, ô không được chọn tính 0 — mẫu số duy nhất đúng khi có luật chọn
chuỗi) và **sụt vốn danh mục** — đường vốn gộp theo NGÀY, vẽ lại cho MỌI bộ
từ các lệnh Go ghi ra và các mốc settle của corpus theo đúng số học của engine
(trả từ mốc sau khi mở tới mốc đóng, vòng phí trừ ở ngày đóng). Một tỷ số
lãi/sụt trên trung bình từng chuỗi sẽ tôn vinh bộ để 97% vốn nằm không và
trúng một chuỗi, nên trang này đọc biên trên của đám mây theo NGÂN SÁCH sụt
vốn thay vì xếp theo tỷ số.

```bash
S=/tmp/bt13/cap
COMMON=(-persist 6 -min-net-apr 0.02 -exit-net-apr 0 -exit-persist 48 -exit-neg-bps 2.0 -exit-neg-periods 2 \
        -exit-neg-cum 1.0 -min-rate-bps 0.3,0.8 -min-hold 0,1.0 -notional 50000 -hold-days 90 \
        -max-basis 1.0,2.0,100 -max-basis-widen 0.5,1.0,2.0,100)
for M in 12 6; do
  # chọn chuỗi BẬT và TẮT là hai lượt: với trail-bps 0 trục ngày vô nghĩa và sẽ nhân ba bộ giống hệt
  $S/backtest -sweep -months $M "${COMMON[@]}" -trail-bps 0.3,0.5,0.7,0.9 -trail-days 30,90,180 \
    -csv $S/on$M.csv -trades-csv $S/ontrades$M.csv
  $S/backtest -sweep -months $M "${COMMON[@]}" -trail-bps 0 -trail-days 0 \
    -csv $S/off$M.csv -trades-csv $S/offtrades$M.csv
done
python3 tools/report/capital.py --db data/scanner.db --config config.yaml \
  --window 12=$S/on12.csv,$S/off12.csv:$S/ontrades12.csv,$S/offtrades12.csv \
  --window 6=$S/on6.csv,$S/off6.csv:$S/ontrades6.csv,$S/offtrades6.csv --out $S/capital.json
python3 tools/report/hold_build.py --json $S/capital.json --template tools/report/capital.template.html \
  --commit $(git rev-parse --short HEAD) --title "Bộ ngưỡng vốn và rủi ro" --out docs/reports/backtest-capital-<ngày>.html
```

"Bộ đề xuất" của trang là bộ tốt nhất trong các bộ còn CHỐT basis hữu hạn
(ngưỡng 100 là lối thoát tắt, không bao giờ nổ trên corpus này); giá của chốt
so với tắt hẳn được in ra bằng số. `hold.py` giờ mang bốn trục mới trong
`AXES` với giá trị mặc định là thứ engine dùng trước khi cột tồn tại (basis
1,0/0,5; chọn chuỗi 0), nên CSV cũ vẫn khoá đúng.

## Ba cái bẫy đã mắc, ghi lại để khỏi mắc lại

**1. `{series}` là placeholder ĐÃ ĐƯỢC ĐẶT TRƯỚC trong template.** `withSeries()`
thay nó bằng số chuỗi của cửa sổ TRƯỚC khi các biến của `t(key, vars)` được
thay. Một chuỗi dịch dùng `{series}` cho ý nghĩa khác sẽ âm thầm ra sai số —
khối đòn bẩy đo trên 16 chuỗi nhưng hiện ra 24. Dùng tên khác (`{nser}`).

**2. Thống kê phải nêu rõ phạm vi.** Số mốc basis lần đầu cộng trên **toàn lưới**
294.912 lượt và ra 824 triệu mốc cho một corpus chỉ có vài chục nghìn. Những con
số "mỗi chuỗi một lần" phải lọc `is_base(r)` trước.

**3. Sắp xếp trên `set` phải có khoá toàn phần.** `sorted(set, key=...)` với khoá
có trùng lặp sẽ giữ thứ tự duyệt của set, mà thứ tự đó đổi giữa hai lần chạy —
cùng dữ liệu ra hai câu khác nhau. Hai lần build cùng một báo cáo phải diff được.

## Vì sao phải chạy hai lần để đo giá của lối thoát basis

`cmd/backtest` hardcode `MaxBasisPct` / `MaxBasisWidenPct` trong `baseParams`,
nên `-config` **không** với tới một lượt `-sweep`; chỉ đường chạy thường
(`plainParams`) mới đọc chúng từ khối `strategy`. Vì vậy phép đo là: chạy đường
thường hai lần, một lần với `config.yaml` thật, một lần với bản copy đã nới
ngưỡng lên 1000% để luật không bao giờ nổ, rồi so hai kết quả.

## Ranh giới

Sửa **luật** thì sửa `internal/strategy`, không sửa ở đây. Nếu một con số trên
trang HTML không truy được về một cột trong CSV mà Go đã ghi, thì con số đó
không nên tồn tại.
