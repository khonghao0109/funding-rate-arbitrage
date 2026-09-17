# BÁO CÁO KIỂM TOÁN CẤP ĐỘ 1: ARBITRAGE TRÊN MỘT SÀN (BINANCE)

> **Cập nhật:** 2026-09-17  
> **Phạm vi kiểm toán:** Mã nguồn tại commit `10495ff` (chế độ READ-ONLY, không sửa file, không ký khoá, không đặt lệnh).  
> **Chưa xác minh được:** Quyền Margin của API key (`canMargin`). Repo không có bản ghi `/api/v3/account` hay `apiRestrictions`, và muốn đọc thì phải gửi request ký bằng khoá thật.

---

## HAI TIỀN ĐỀ CẦN HIỆU CHỈNH TRƯỚC
1. **Quy đổi Funding âm:** $-5\text{ bps}/8h$ không phải khoảng $-5,4\%/\text{năm}$. Một năm có 1.095 mốc settle 8h, nên $-5\text{ bps}/8h \approx -54,75\%/\text{năm}$. Con số $-5,4\%/\text{năm}$ ứng với khoảng $-0,5\text{ bps}/8h$.
2. **Chi phí vòng phí:** Phí 4 lượt $0,16\%$ là chi phí trả một lần, không phải chi phí mỗi năm. Muốn so với APR thì phải chia cho số mốc settle mà vị thế thực sự giữ qua.

---

## 1. HIỆN TRẠNG CHIỀU THUẬN (LONG SPOT + SHORT PERP)

### Điểm mạnh đã kiểm chứng trong code
| Cơ chế | Dẫn chứng | Nhận xét |
| :--- | :--- | :--- |
| **Hai chân khớp tuần tự, spot trước (mặc định)** | `internal/execution/types.go:195`, `open.go:155-171` | Spot mua bằng tiền thật không thể bị thanh lý, nên để nó "trần" trong tích tắc là hướng an toàn nhất. |
| **Chân 1 khớp thiếu thì không gửi chân 2** | `open.go:161-166` | Gỡ ngay chân 1 nếu chân 1 không đủ điều kiện hoặc thiếu hụt. |
| **Khớp một phần (Partial fill)** | `open.go:246-385` | Thu nhỏ cặp về chân nhỏ hơn trước khi gỡ. Có kiểm tra cỡ còn đóng được, sổ lệnh và mức trượt giá để tránh trả trọn vòng phí phá cặp đang phòng hộ. |
| **Lỗi mơ hồ (timeout, 5xx)** | `open.go:510-561`, `open.go:870-883` | Tra lại theo `ClientOrderID`. Chỉ gửi lại khi sàn trả lời không có lệnh đó. |
| **Luôn đọc lại lệnh sau khi huỷ** | `open.go:442-466` | Tránh trường hợp lệnh huỷ đua với một lần khớp từ sàn. |
| **Thứ tự đóng vị thế** | `close.go:279-296` | Perp đóng trước, rồi spot đóng đúng bằng lượng perp thực sự đã đóng. |
| **Lệnh MARKET futures chờ xác nhận** | `close.go:431-460` | Hỏi lại sàn đến khi thấy trạng thái khớp thực tế. |
| **Chứng minh phẳng bằng hai nguồn** | `open.go:767-812` | Số dư trên sàn và sổ lệnh của bot. Lệch nhau thì báo lỗi, không tự hoà giải. |

### Rủi ro còn tồn đọng
| # | Mức | Rủi ro | Dẫn chứng | Kịch bản hỏng & Tác động |
| :---: | :---: | :--- | :--- | :--- |
| **R1** | **P0 (mainnet)** | Phí mua spot trừ bằng coin gốc không được trừ khỏi khối lượng | `open.go:160`, `realized.go:122-128`, `open.go:777-779` | Mainnet phí 10 bps trả bằng BTC: mua 0,0008 BTC chỉ nhận 0,0007992 BTC, trong khi perp short đủ 0,0008. Nếu ví không có sẵn BTC: perp đóng trước thành công, spot bán 0,0008 bị từ chối `-2010 Insufficient balance`, kẹt spot trần và bot halt. Testnet che lỗi này do phí spot testnet = 0 và ví có sẵn 1 BTC gieo sẵn. |
| **R2** | **P1** | Gỡ vị thế dùng lệnh MARKET, không có trần trượt giá | `open.go:344-349`, `open.go:687-739` | Sổ mỏng khi chân 2 bị từ chối thì chịu trượt giá kép: lúc vào có trần 10 bps, lúc gỡ không có trần. Chưa được tính vào chi phí dự phóng. |
| **R3** | **P2** | Vòng gỡ vị thế đóng spot trước, perp sau | `open.go:599-613` | Ngược với lập luận "perp trước" của Close. Ảnh hưởng chế độ song song khi cả hai chân đã khớp một phần: perp short để trần trong lúc bán spot. |
| **R4** | **P1** | Bụi coin lẻ (Dust) | `open.go:629-639`, `close.go:265-269` | Bụi spot dưới `minNotional` (5 USDT) không bán được qua API lệnh thường, không có client cho endpoint đổi bụi. Trang PnL chưa liệt kê bụi này. |
| **R5** | **P1** | Mức chốt lời 1,50% tính trên vốn gần như không kích hoạt theo basis | `state.go:213`, `main.go:87` | Với K=1.5, 1,50% vốn = 225 bps notional. Basis vào chỉ $\ge +5\text{ bps}$ nên phần hội tụ chỉ góp vài chục bps; phần lớn phải đến từ funding (cần 100+ ngày ở rate testnet 0.657 bps/8h). |
| **R6** | **P1** | Van spread 10 bps đo lúc quét, không đo lúc gửi lệnh đóng | `signal.go:950-981`, `engine.go:1030-1055` | Lệnh đóng gửi sau đó vài giây có thể gặp sổ lệnh biến động mà không kiểm tra lại. Không đo được spread thì van hiện mở. |
| **R7** | **P0 (mainnet)** | Không giám sát ký quỹ và thanh lý của chân Perp Short | `signal.go:826-983`, `parse.go:174-187` | Endpoint V3 không trả `liquidationPrice`. Bot chưa đọc `totalMarginRatio`. Nếu ví futures dùng cross margin và gánh nhiều vị thế, giá tăng mạnh có thể thanh lý perp. Lãi spot ở ví khác không bù được. |
| **R8** | **P1** | Cặp halt do lỗi mạng bị đóng băng cả lối thoát rủi ro | `engine.go:467-474` | Khi mạng rớt rồi có lại, cặp halt do 5 lần đọc hỏng không tự chạy cắt lỗ basis hay thoát funding cho đến khi người vận hành bấm xác nhận tay. |
| **R9** | **P2** | `CLAUDE.md` lệch với code ở ba chỗ | `CLAUDE.md:1079, 1041`, `state.go:234-240` | Chốt lời ghi +0,50% (code 1,50%), vị thế ghi 3/trần 5 (code 50/50), halt sau 3 lần đọc hỏng (code 5). |

---

## 2. KHOẢNG TRỐNG KỸ THUẬT CỦA CHIỀU NGHỊCH (SHORT SPOT MARGIN + LONG PERP)

> **KẾT LUẬN:** Hiện tại không có hạ tầng nào cho chiều nghịch, và có điểm chặn nằm ngoài code.

| Hạng mục | ĐÃ CÓ | CẦN BỔ SUNG |
| :--- | :--- | :--- |
| **Client Margin API** | Không có (`endpoints.go` không có `/sapi/*`) | Đặt/tra/huỷ lệnh margin, `/sapi/v1/margin/account` (`marginLevel`), `maxBorrowable`, lịch sử lãi vay, repay. |
| **Host được phép** | `demo-fapi.binance.com`, `testnet.binance.vision` | **Điểm chặn:** Spot testnet không hỗ trợ `/sapi` margin. Chiều nghịch không thể nghiệm thu trên testnet, mà nguyên tắc cấm chạm mainnet trước Bước 4.6. |
| **Loại thị trường** | `MarketSpot` và `MarketFuturesUSDM` | Thêm `MarketMargin` vào `broker.Market` và `RoundOrder`. |
| **Máy trạng thái execution** | Cố định mua spot / bán perp (`open.go:135-144`) | Thêm `Direction` vào Intent/Position. Chân vay phải đặt trước. Chứng minh phẳng phải dựa trên khoản nợ = 0 (gốc + lãi). |
| **Làm tròn khi trả nợ** | `RoundOrder` luôn làm tròn xuống | Lệnh mua trả nợ phải phủ cả lãi tích luỹ (phải làm tròn lên). Luật hiện tại cấm điều này. |
| **Dữ liệu backtest** | Corpus funding 3 năm và candles | Không có lịch sử lãi vay margin $\rightarrow$ không thể backtest được. |

---

## 3. MÔ HÌNH RỦI RO & NET APR CHIỀU NGHỊCH

### Công thức tính dòng tiền thực tế (đếm theo mốc settle)
$$
\text{KQ}_{\text{quote}}(H) = \sum_{i \in \text{mốc}} (-f_i) \cdot N_i^{\text{perp}} - \sum_{h \in \text{giờ}} r_h^{\text{vay}} \cdot Q \cdot P_h - \text{Phí}_{4\text{ lượt}} - \text{Trượt}_{4\text{ lượt}} + \text{Trôi basis}
$$

### So sánh rủi ro thanh lý sinh tử
| Tiêu chí | Chiều Thuận (Cash & Carry) | Chiều Nghịch (Reverse Margin) |
| :--- | :--- | :--- |
| **Chân Spot** | Mua bằng tiền thật, **KHÔNG BAO GIỜ bị thanh lý** | Có thể bị thanh lý khi giá TĂNG: $c=0.5 \rightarrow$ tăng $+36.4\%$ là cháy ví! |
| **Chân Perp** | Short, thanh lý khi giá TĂNG ($f=0.5 \rightarrow +49.4\%$) | Long, thanh lý khi giá GIẢM ($f=0.5 \rightarrow -49.8\%$) |
| **Hướng nguy hiểm** | Một hướng duy nhất (giá tăng) | **Cả hai hướng (mỗi chân một hướng)** |
| **Lãi vay tích luỹ** | $0$ | Lãi vay cộng dồn vào nợ, `marginLevel` tự trôi xuống theo thời gian. |
| **Điều chuyển vốn nội bộ** | Không có (Ví Spot và Futures tách biệt) | Không có (Ví Margin và Futures tách biệt) |

---

## 4. KIẾN NGHỊ HÀNH ĐỘNG CỤ THỂ

### P0 (Chặn trước Mainnet)
1. **Sửa R1 (Phí trừ bằng coin gốc):** Khối lượng chân perp phải lấy theo lượng coin thực nhận sau phí (`fills`/`commissionAsset`), hoặc bắt buộc bật cờ trả phí bằng BNB.
2. **Sửa R7 (Ký quỹ Perp Short):** Đọc `totalMarginRatio` và `liquidationPrice` từ `/fapi/v3/account`. Thêm lối thoát rủi ro khẩn cấp khi tỷ lệ ký quỹ $> 70\%$.
3. **Chính thức DỪNG / ĐÓNG BĂNG Chiều Nghịch (Short Spot Margin):** Chuyển toàn bộ nhu cầu Long Futures sang **Perp-Perp 2 Sàn (Bybit + Binance)**.

### P1 (Ưu tiên tiếp theo)
1. **Sửa R8:** Khi cặp halt chỉ vì lỗi đọc mạng mà sau đó đọc lại thấy 2 chân vẫn hedged phẳng sạch, cho phép các lối thoát rủi ro (basis widen, funding exit) tiếp tục chạy ngầm. Thêm nút "Xác nhận tất cả (Ack All)" trên UI.
2. **Cân chỉnh R5:** Thêm điều kiện chốt lời khi Basis co hẹp $\ge 15\text{ bps}$ để không bị phụ thuộc vào 100 ngày giữ funding.
3. **Sửa R6:** Kiểm tra lại spread sổ lệnh ngay tại thời điểm bắn lệnh đóng MARKET.
4. **Sửa R4:** Liệt kê bụi coin lẻ trên trang PnL của portal.
5. **Sửa R9:** Đồng bộ 3 chỗ sai lệch tài liệu trong `CLAUDE.md`.
