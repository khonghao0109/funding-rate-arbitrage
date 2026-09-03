package fees

// Schedule is one venue's commission at its DEFAULT tier: no VIP level, no
// 30-day volume discount, no token-holding rebate. It is a documented UPPER
// BOUND on what a fresh account pays, never an estimate of what a particular
// account pays.
//
// MakerFeeBps and TakerFeeBps are FRACTIONAL basis points, not whole ones.
// docs/CONVENTIONS.md §1.2 originally asked for integers, on the reasoning that
// a quoted fee is exact and integers avoid floating-point error. The premise is
// wrong: Hyperliquid's maker leg is 0.015% (1.5 bps) and Paradex's is 0.003%
// (0.3 bps). Rounding 1.5 to 2 misstates that leg by a third and rounding 0.3 to
// 0 makes it free, which is a far larger error than any float64 rounding of a
// constant that is only ever multiplied by a notional. The convention was
// amended rather than the numbers.
//
// Verified says the numbers came from the venue's own published schedule, which
// was read and cited. It exists because 0 is a REAL fee on some venues -
// Paradex charges retail accounts nothing - so an unfilled entry must not be
// indistinguishable from a free one. Anything downstream must refuse to produce
// a cost when Verified is false rather than treating the zeros as data. This is
// the same rule the top-of-book quantities follow at step 1.2.
type Schedule struct {
	Source      string
	MakerFeeBps float64
	TakerFeeBps float64

	Verified bool
	DocURL   string
	NoteVI   string
}

// schedules is the fee table, keyed by the same source identifiers the scanner's
// registry uses. Four venues are deliberately left unverified: their fee pages
// require a login or did not respond, and CLAUDE.md rule 5 forbids writing a
// remembered number that would be wrong without looking wrong. Step 1.4 moves
// this table to config.yaml, where an operator supplies the rates their own
// account actually pays - which is the only fully correct source anyway.
//
// Read on 2026-09-03.
var schedules = []Schedule{
	{
		Source: "binance_futures", MakerFeeBps: 2.0, TakerFeeBps: 5.0, Verified: true,
		DocURL: "https://www.binance.com/en/support/faq/detail/360033544231",
		NoteVI: "USDⓈ-M futures, Regular User. Con số nằm trong ví dụ tính toán trên trang " +
			"hỗ trợ chính thức của Binance; bảng biểu phí gốc (/en/fee/futureFee) đòi đăng nhập.",
	},
	{
		Source: "binance_spot", MakerFeeBps: 10.0, TakerFeeBps: 10.0, Verified: true,
		DocURL: "https://www.binance.com/en/fee/schedule",
		NoteVI: "Spot, Regular User (khối lượng 30 ngày < 1.000.000 USD, không giữ BNB).",
	},
	{
		Source: "hyperliquid_futures", MakerFeeBps: 1.5, TakerFeeBps: 4.5, Verified: true,
		DocURL: "https://hyperliquid.gitbook.io/hyperliquid-docs/trading/fees",
		NoteVI: "Perps bậc 0 (không yêu cầu khối lượng): maker 0,015% · taker 0,045%.",
	},
	{
		Source: "kraken_futures", MakerFeeBps: 2.0, TakerFeeBps: 5.0, Verified: true,
		DocURL: "https://www.kraken.com/features/fee-schedule",
		NoteVI: "Kraken Futures bậc 1 (khối lượng futures 30 ngày < 5 triệu USD).",
	},
	{
		// Paradex charges retail accounts nothing and Pro accounts a tiered
		// taker between 0.035% and 0.045%. The scanner does not know which
		// account will trade, so it takes the WORST case: Pro, top of the taker
		// range. The after-fee number is therefore a lower bound on what a
		// retail account keeps, which is the safe direction to be wrong in.
		Source: "paradex_futures", MakerFeeBps: 0.3, TakerFeeBps: 4.5, Verified: true,
		DocURL: "https://docs.paradex.trade/trading/trading-fees.md",
		NoteVI: "Bậc Pro, đầu cao của thang taker (0,035–0,045%): maker 0,003% · taker 0,045%. " +
			"Tài khoản Retail hiện là 0%, nên số sau phí là cận DƯỚI của cái giữ lại được.",
	},

	// --- Not verified. No number is better than a wrong number. ---
	{
		Source: "bybit_futures",
		NoteVI: "Chưa xác minh: trang biểu phí của Bybit không phản hồi từ môi trường này. " +
			"Không điền theo trí nhớ (CLAUDE.md luật 5). Nhập tay ở config.yaml tại Bước 1.4.",
	},
	{
		Source: "bybit_spot",
		NoteVI: "Chưa xác minh: cùng lý do với bybit_futures.",
	},
	{
		Source: "okx_futures",
		NoteVI: "Chưa xác minh: trang biểu phí của OKX trả về 404 hoặc chỉ có metadata, " +
			"bảng phí thật đòi đăng nhập.",
	},
	{
		Source: "gate_futures",
		NoteVI: "Chưa xác minh: thông báo công khai của Gate có maker 0,0200% / taker 0,0500% " +
			"nhưng ghi rõ là cho 'USDT-M TradFi Perpetuals' (cổ phiếu, kim loại, chỉ số, " +
			"forex, hàng hoá) — KHÔNG phải perp crypto. Dùng con số đó sẽ là đúng bảng, sai thị trường.",
	},
	{
		Source: "pyth",
		NoteVI: "Oracle, không giao dịch được nên không có phí. Đây là lý do khác hẳn " +
			"'chưa tra được', và nó không bao giờ nằm trong một phép so sánh.",
	},
}

var bySource = func() map[string]Schedule {
	index := make(map[string]Schedule, len(schedules))
	for _, s := range schedules {
		index[s.Source] = s
	}
	return index
}()

// For returns what is known about a source's commission. A source absent from
// the table comes back unverified rather than free.
func For(source string) Schedule {
	if schedule, ok := bySource[source]; ok {
		return schedule
	}
	return Schedule{
		Source: source,
		NoteVI: "Nguồn không có trong bảng phí",
	}
}

// All returns the whole table, in declaration order.
func All() []Schedule {
	out := make([]Schedule, len(schedules))
	copy(out, schedules)
	return out
}

// RoundTripTakerPct is the share of notional a COMPLETE two-venue trade pays in
// commission, as a percentage, assuming a taker fill every time.
//
// FOUR fills, not two. A cross-venue spread is captured by buying on one venue
// and selling on the other, and it is only turned into money by unwinding both
// legs afterwards - the position cannot be walked away from, and a perpetual
// cannot be transferred between venues. Charging only the entry would report
// half the real cost, and half of a cost is the same kind of overstatement as
// calling a gross spread a profit. See CLAUDE.md rule 2.
//
// Taker on every leg is the conservative assumption: a maker fill is cheaper on
// every venue in the table, so a real execution can only beat this number.
//
// The result is a first-order figure. Fees are charged on each fill's own
// notional and the two legs differ by exactly the spread, which is a fraction of
// a percent - far below the precision of the fee table itself. What it does NOT
// include is slippage, funding, and withdrawal or transfer cost. It is
// therefore "after trading fees", never "net profit".
//
// ok is false when either venue's schedule is unverified: no number at all is
// the honest answer, and the caller must publish null rather than a figure that
// silently treats an unknown fee as zero.
func RoundTripTakerPct(buy, sell Schedule) (float64, bool) {
	if !buy.Verified || !sell.Verified {
		return 0, false
	}
	const bpsPerPct = 100
	const fillsPerLeg = 2 // open and close
	return (buy.TakerFeeBps + sell.TakerFeeBps) * fillsPerLeg / bpsPerPct, true
}
