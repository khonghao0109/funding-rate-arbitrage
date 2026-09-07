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
