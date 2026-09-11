# Gói nghiên cứu Crowding Reversal (nhận 2026-09-04)

Tài liệu tham chiếu cho **GĐ 6 — Crowding Reversal** (quyết định Q11, Q12
trong [docs/PLAN.md §7.1](../../PLAN.md)). Đây là bản chọn lọc từ gói
`crowding-reversal-python-share-20260904.zip` mà tác giả gửi; tài liệu gốc
viết bằng tiếng Trung.

**Chỉ đọc, không chạy trong tiến trình.** Quy tắc 8 của CLAUDE.md: Go sở hữu
mọi thứ quyết định giao dịch. Mã Python ở đây là *nguồn để port* sang Go ở
Bước 6.1 và là *bằng chứng* cho các con số PLAN trích dẫn; nó không được
import, gọi, hay chạy bởi bất kỳ tiến trình nào của dự án. Sau khi Bước 6.3
xong, bản Go là reference đóng băng và thư mục này chỉ còn giá trị lịch sử.

## Bản gốc

- SHA-256 của zip gốc: `dae529bff6aae22ea55b31551f788311366eac51a9bc7e6040739c102ed0f557`
- Bỏ khỏi repo (giữ ở ngoài): `crowding_reversal_results/backtest_dashboard.html`
  (1,75 MB, mở bằng trình duyệt), `core_path.parquet` (1,1 MB), `overview.png`,
  và bản thô `rust_parity_fixture.csv` (3,8 MB) — ở đây chỉ giữ bản nén.
- Dữ liệu thô (~290 MB parquet nến 1 phút và metrics 5 phút) **không có trong
  gói gốc**; các file `INCOME_*.md`, `SUPPLEMENTAL_*.md`, `knowledge_base/` mà
  `STRATEGY_RESEARCH_CONCLUSION.md` tham chiếu cũng không có.

## Nội dung

| File | Dùng để |
|---|---|
| `crowding_reversal_research.py` | Luật chiến lược (6 hàm lõi ~120 dòng) + toàn bộ stress test; **nguồn port của 6.1** |
| `crowding_reversal_pre_rust_audit.py` | Cách từng cột của fixture được sinh — đọc trước khi viết test parity |
| `crowding_reversal_signal.py` | Sinh quyết định JSON từ dữ liệu local, không nối sàn |
| `crowding_reversal_dashboard.py` | Sinh HTML báo cáo (HTML không giữ trong repo) |
| `test_*.py` | 13 test về nhân quả, giới hạn vị thế, trễ 1 nến — port thành test bảng Go ở 6.1 |
| `CROWDING_REVERSAL_PLAYBOOK.md` | Luật đóng băng, cách paper trade, điều kiện lên sàn |
| `CROWDING_REVERSAL_RESEARCH.md` | Toàn bộ bảng số, stress test phí/trễ/biên, giới hạn |
| `CROWDING_REVERSAL_RUST_READINESS.md` | Audit 5 năm, quy ước số học và dung sai parity (đích thật là Go) |
| `STRATEGY_RESEARCH_CONCLUSION.md` | Kết luận, các hướng đã loại — gồm bằng chứng Q11 (perp↔quý 0,25%/năm) |
| `crowding_reversal_results/rust_reference_manifest.json` | Tham số đóng băng, SHA-256 nguồn, thống kê lõi, `known_limits` |
| `crowding_reversal_results/rust_parity_fixture.csv.gz` | 10.957 dòng × 26 cột, đầu vào 4h + đầu ra kỳ vọng; SHA-256 sau giải nén `fa9eae27c979f7fe031f6d54b1db954afbc81c3634e9542460a4533b49ec3bb2` |
| `crowding_reversal_results/*.csv` | Bootstrap, placebo, kịch bản phí/trễ, lân cận tham số, trade episodes, coverage |

## Đọc số cho đúng

Mọi lợi suất trong gói là **CAGR lãi kép trên equity, sau phí GIẢ ĐỊNH 5
bps/chiều và funding lịch sử, chưa có trượt giá đo từ sổ, thanh lý, sự cố
sàn** — theo quy tắc 2 của repo thì không được gọi là "ròng". Một sàn
(Binance), 4,5 năm, đoạn validation đã được tác giả xem nhiều lần. Tác giả
chỉ duyệt reference engine và paper trading ≥ 6 tháng; **không duyệt vốn
thật**. Chi tiết và giới hạn: PLAN.md GĐ 6.
