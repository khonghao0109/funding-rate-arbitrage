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
//
// The schedules themselves live in config.yaml since step 1.4, so an operator
// can enter the rates their own account pays - which is the only fully correct
// source, since a fee depends on VIP tier and 30-day volume. This package holds
// the calculation, not the table.
type Schedule struct {
	Source      string
	MakerFeeBps float64
	TakerFeeBps float64

	Verified bool
	DocURL   string
	NoteVI   string
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
