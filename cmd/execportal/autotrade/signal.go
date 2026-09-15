package autotrade

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/fees"
	"futures-arbitrage-scanner/internal/strategy"
)

// The decision, as pure functions of one reading: no clock, no venue, no lock.
// Every check runs even after one has failed, so the console names all the
// reasons at once (the step-3.2 rule), and a check whose input could not be
// read says so instead of passing or inventing a second cause.

// SettledRate is one settlement the venue lists.
type SettledRate struct {
	SettledAtMs         int64
	RatePerIntervalFrac float64
	// Special is the venue's rateType "Special": off the cadence, so it is
	// counted as a settlement crossed but kept out of the cadence and the mean.
	Special bool
}

// Snapshot is one scan's reading of both testnet markets.
type Snapshot struct {
	Symbol   string
	ReadAtMs int64

	SpotBook depth.Summary
	PerpBook depth.Summary

	// ForecastRatePerIntervalFrac is premiumIndex's lastFundingRate: the rate
	// forming for the next settlement, revised until the stamp.
	ForecastRatePerIntervalFrac float64
	NextFundingTimeMs           int64

	// Settled is every settlement from the requested instant to now, oldest
	// first. SettledErrVI says why it could not be read; the checks that need
	// it then report "not evaluated" rather than passing.
	Settled      []SettledRate
	SettledErrVI string

	// The account's taker fee per fill on each market, read from the venue.
	// FeesErrVI set means they could not be, and nothing is priced.
	SpotTakerFeeBps float64
	PerpTakerFeeBps float64
	FeeSourceVI     string
	FeesErrVI       string

	// The broker's last measured clock offset on each market, venue minus
	// local, in ms; nil when it was never measured.
	SpotClockSkewMs *int64
	PerpClockSkewMs *int64
}

// CheckView is one condition, with the numbers behind it.
type CheckView struct {
	NameVI string `json:"name_vi"`
	// Passed is the condition holding. Evaluated false means its input could
	// not be read, and Passed is then false for an entry and "no exit" for an
	// exit — never a guess in the direction of trading.
	Passed    bool   `json:"passed"`
	Evaluated bool   `json:"evaluated"`
	DetailVI  string `json:"detail_vi"`
}

// SignalView is the gauge: the last reading and what the bot made of it.
type SignalView struct {
	EvaluatedAtMs int64  `json:"evaluated_at_ms"`
	Symbol        string `json:"symbol"`

	ForecastRatePerIntervalBps     *float64 `json:"forecast_rate_per_interval_bps"`
	ForecastRatePer8hBps           *float64 `json:"forecast_rate_per_8h_bps"`
	LastSettledRatePerIntervalBps  *float64 `json:"last_settled_rate_per_interval_bps"`
	LastSettledAtMs                int64    `json:"last_settled_at_ms"`
	TrailingMeanRatePerIntervalBps *float64 `json:"trailing_mean_rate_per_interval_bps"`
	TrailingMeanRatePer8hBps       *float64 `json:"trailing_mean_rate_per_8h_bps"`
	TrailingSettlements            int      `json:"trailing_settlements"`
	TrailingWindowDays             float64  `json:"trailing_window_days"`
	IntervalSec                    int64    `json:"interval_sec"`
	NextFundingTimeMs              int64    `json:"next_funding_time_ms"`
	TimeToSettleSec                int64    `json:"time_to_settle_sec"`

	SpotMidQuote  float64  `json:"spot_mid_quote"`
	PerpMidQuote  float64  `json:"perp_mid_quote"`
	BasisBps      *float64 `json:"basis_bps"`
	EntryBasisBps *float64 `json:"entry_basis_bps"`
	BasisWidenBps *float64 `json:"basis_widen_bps"`

	SpotTakerFeeBps float64 `json:"spot_taker_fee_bps"`
	PerpTakerFeeBps float64 `json:"perp_taker_fee_bps"`
	FeeSourceVI     string  `json:"fee_source_vi"`

	// The priced round trip and what strategy.NetAPR made of it, on ONE LEG's
	// notional. NetAPROnCapitalPct divides by the capital the pair ties up —
	// the spot leg in full plus the perp margin — beside it, never instead.
	FeesPct            *float64 `json:"fees_pct"`
	SlippagePct        *float64 `json:"slippage_pct"`
	RoundTripCostPct   *float64 `json:"round_trip_cost_pct"`
	EntryCostPct       *float64 `json:"entry_cost_pct"`
	HoldingDays        float64  `json:"holding_days"`
	SettlementsInHold  int64    `json:"settlements_in_hold"`
	GrossAPRPct        *float64 `json:"gross_apr_pct"`
	NetAPRPct          *float64 `json:"net_apr_pct"`
	NetAPROnCapitalPct *float64 `json:"net_apr_on_capital_pct"`
	CapitalPerNotional float64  `json:"capital_per_notional"`
	NetAPRReasonVI     string   `json:"net_apr_reason_vi"`

	MinDepthWideQuote  *float64 `json:"min_depth_wide_quote"`
	RequiredDepthQuote float64  `json:"required_depth_quote"`

	EntryChecks   []CheckView `json:"entry_checks"`
	EntryEligible bool        `json:"entry_eligible"`
	ExitChecks    []CheckView `json:"exit_checks"`
	ExitDue       bool        `json:"exit_due"`
	VerdictVI     string      `json:"verdict_vi"`

	AppliedVI  []string `json:"applied_vi"`
	ExcludedVI []string `json:"excluded_vi"`
}

// trailingWindow is how far back the entry's mean rate reaches. Seven days is
// 21 settlements at 8h and 168 at 1h: enough to average a day's swing away, and
// short enough that a regime the testnet left a month ago does not price today.
const trailingWindow = 7 * 24 * time.Hour

// minTrailingSettlements is the fewest settlements a mean and a cadence are
// measured from.
const minTrailingSettlements = 3

// maxBookAge is how old a book may be when it prices the round trip. The scan
// reads both books immediately before evaluating, so this is room for the scan
// itself, not a tolerance for stale data.
const maxBookAge = 60 * time.Second

// maxClockSkewMs is the widest clock offset an entry is sent with. The broker
// corrects every signed request by the measured offset, but an offset this
// large says the measurement itself is unreliable — the plan's clock breaker.
const maxClockSkewMs = 1000

// cadenceShare is how much of the spacing must agree for a cadence to be read
// off it. The rest is missed settlements (a double gap) and nothing else; two
// cadences with real weight inside the window are refused (the history-interval
// row of CLAUDE.md's trap table).
const cadenceShare = 0.9

// measureIntervalSec reads the settlement cadence from the stamps themselves
// (rule 3: never assumed). Each gap is rounded to the minute before counting —
// the interval, not the stamps, which stay verbatim — because the testnet
// stamps land a millisecond either side of the hour.
func measureIntervalSec(settled []SettledRate) (int64, string) {
	var stamps []int64
	for _, s := range settled {
		if !s.Special {
			stamps = append(stamps, s.SettledAtMs)
		}
	}
	if len(stamps) < minTrailingSettlements {
		return 0, fmt.Sprintf("chỉ có %d mốc settle thường — cần ít nhất %d để đo chu kỳ", len(stamps), minTrailingSettlements)
	}
	counts := map[int64]int{}
	gaps := 0
	for i := 1; i < len(stamps); i++ {
		gapSec := int64(math.Round(float64(stamps[i]-stamps[i-1])/60_000)) * 60
		if gapSec <= 0 {
			return 0, "hai mốc settle trùng thời điểm — không đo được chu kỳ"
		}
		counts[gapSec]++
		gaps++
	}
	var modal int64
	for gap, n := range counts {
		if n > counts[modal] || (n == counts[modal] && gap < modal) {
			modal = gap
		}
	}
	if share := float64(counts[modal]) / float64(gaps); share < cadenceShare {
		return 0, fmt.Sprintf("khoảng cách giữa các mốc không thống nhất (%.0f%% là %ds) — chu kỳ đổi trong cửa sổ, không định giá", share*100, modal)
	}
	return modal, ""
}

// cadenceAgreesWithNext cross-checks the cadence measured from history against
// the venue's own next stamp: the next settlement must be exactly one measured
// interval after the last regular one. A symbol whose cadence changed in the
// last settlement or two hides inside the modal share, and projecting its old
// cadence would misstate the APR by the ratio of the two; this catches it. It
// also refuses for the minute or two after each settlement while the history
// read has not yet listed it — an entry waits, which costs nothing.
func cadenceAgreesWithNext(snap Snapshot, intervalSec int64) string {
	if snap.NextFundingTimeMs <= 0 {
		return ""
	}
	var lastMs int64
	for _, r := range snap.Settled {
		if !r.Special && r.SettledAtMs > lastMs {
			lastMs = r.SettledAtMs
		}
	}
	if lastMs <= 0 {
		return ""
	}
	gapSec := int64(math.Round(float64(snap.NextFundingTimeMs-lastMs)/60_000)) * 60
	if gapSec != intervalSec {
		return fmt.Sprintf("mốc kế tiếp cách mốc đã settle gần nhất %ds, lịch sử cho chu kỳ %ds — chu kỳ vừa đổi, hoặc lịch sử chưa liệt kê mốc vừa settle", gapSec, intervalSec)
	}
	return ""
}

// trailingMean is the mean settled rate of the regular settlements at or after
// fromMs.
func trailingMean(settled []SettledRate, fromMs int64) (float64, int) {
	sum, n := 0.0, 0
	for _, s := range settled {
		if s.Special || s.SettledAtMs < fromMs {
			continue
		}
		sum += s.RatePerIntervalFrac
		n++
	}
	if n == 0 {
		return 0, 0
	}
	return sum / float64(n), n
}

// basisBps is perp over spot, in basis points of the spot mid.
func basisBps(spotMidQuote, perpMidQuote float64) (float64, bool) {
	if !(spotMidQuote > 0) || !(perpMidQuote > 0) {
		return 0, false
	}
	return (perpMidQuote - spotMidQuote) / spotMidQuote * 10_000, true
}

func ptr(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

// per8hBps converts one symbol's per-interval rate to the 8h comparison unit
// through the one function the repository annualizes with.
func per8hBps(ratePerIntervalFrac float64, intervalSec int64) *float64 {
	if intervalSec <= 0 {
		return nil
	}
	d, err := exchanges.DeriveFundingRates(exchanges.FundingData{Source: "autotrade", Symbol: "-",
		RatePerIntervalFrac: ratePerIntervalFrac, IntervalSec: intervalSec})
	if err != nil {
		return nil
	}
	return ptr(d.RatePer8hFrac * 10_000)
}

// holdPlan is the hold an entry is priced over: MaxHoldEpochs settlements at
// the measured cadence, or ProjectionHoldDays when the bot holds while funding
// pays.
func holdPlan(cfg Config, intervalSec int64) (holdingDays float64, settlements int64) {
	if cfg.MaxHoldEpochs > 0 && intervalSec > 0 {
		return float64(int64(cfg.MaxHoldEpochs)*intervalSec) / 86_400, int64(cfg.MaxHoldEpochs)
	}
	return cfg.ProjectionHoldDays, 0
}

// entryInput is everything one entry judgement reads.
type entryInput struct {
	Cfg        Config
	Snap       Snapshot
	Now        time.Time
	MarginFrac float64
	// Flat is the venue saying both legs of the symbol are flat; HeldVI says
	// what it holds otherwise.
	Flat   bool
	HeldVI string
}

// assessEntry fills the gauge and runs every entry check.
func assessEntry(in entryInput) SignalView {
	cfg, snap := in.Cfg, in.Snap
	sig := gauge(cfg, snap, in.Now, in.MarginFrac)

	add := func(name string, evaluated, passed bool, detail string) {
		sig.EntryChecks = append(sig.EntryChecks, CheckView{NameVI: name, Evaluated: evaluated, Passed: evaluated && passed, DetailVI: detail})
	}

	if in.Flat {
		add("Không có vị thế nào mở trên symbol", true, true, "sàn báo hai chân phẳng")
	} else {
		add("Không có vị thế nào mở trên symbol", true, false, "sàn đang giữ: "+in.HeldVI+" — bot không mở thêm và không quản lý vị thế không phải của nó")
	}

	add("Funding đang hình thành > 0", true, snap.ForecastRatePerIntervalFrac > 0,
		fmt.Sprintf("premiumIndex.lastFundingRate %+.4f bps mỗi chu kỳ của symbol — chưa settle, chỉ để chặn, không dùng để dự phóng", snap.ForecastRatePerIntervalFrac*10_000))

	switch {
	case snap.SettledErrVI != "":
		add("Mốc settle gần nhất > 0", false, false, "không đọc được lịch sử funding: "+snap.SettledErrVI)
	case sig.LastSettledRatePerIntervalBps == nil:
		add("Mốc settle gần nhất > 0", false, false, "sàn không liệt kê mốc settle nào trong cửa sổ")
	default:
		add("Mốc settle gần nhất > 0", true, *sig.LastSettledRatePerIntervalBps > 0,
			fmt.Sprintf("%+.4f bps, settle lúc %s", *sig.LastSettledRatePerIntervalBps, time.UnixMilli(sig.LastSettledAtMs).Format("02/01 15:04")))
	}

	aprDetail := sig.NetAPRReasonVI
	if sig.NetAPRPct != nil {
		aprDetail = fmt.Sprintf("%.2f%%/năm trên notional một chân (%.2f%% trên vốn) ≥ %.2f%%? · trung bình %d mốc đã settle, giữ %s, chi phí vòng %.4f%%",
			*sig.NetAPRPct, deref(sig.NetAPROnCapitalPct), cfg.MinNetAPRPct, sig.TrailingSettlements, holdVI(cfg, sig), deref(sig.RoundTripCostPct))
	}
	add("Net APR dự phóng ≥ ngưỡng", sig.NetAPRPct != nil, sig.NetAPRPct != nil && *sig.NetAPRPct >= cfg.MinNetAPRPct, aprDetail)

	depthOK, depthEval, depthDetail := depthCheck(cfg, snap)
	add(fmt.Sprintf("Độ sâu ±0,5%% ≥ %.0f× notional ở cả 4 phía", cfg.DepthMultiple), depthEval, depthOK, depthDetail)

	const clockName = "Lệch đồng hồ sàn ≤ 1000 ms"
	if snap.SpotClockSkewMs == nil || snap.PerpClockSkewMs == nil {
		add(clockName, false, false, "chưa đo được đồng hồ của cả hai sàn")
	} else {
		spot, perp := *snap.SpotClockSkewMs, *snap.PerpClockSkewMs
		worst := max(abs64(spot), abs64(perp))
		add(clockName, true, worst <= maxClockSkewMs, fmt.Sprintf("spot %+d ms, futures %+d ms", spot, perp))
	}

	switch {
	case snap.NextFundingTimeMs <= 0:
		add("Còn đủ xa mốc settle kế tiếp", false, false, "sàn không cho nextFundingTime")
	default:
		left := time.UnixMilli(snap.NextFundingTimeMs).Sub(in.Now)
		add("Còn đủ xa mốc settle kế tiếp", true, left > cfg.MinTimeToSettle,
			fmt.Sprintf("còn %s tới %s, cần > %s", left.Round(time.Second), time.UnixMilli(snap.NextFundingTimeMs).Format("15:04"), cfg.MinTimeToSettle))
	}

	sig.EntryEligible = true
	var failed []string
	for _, c := range sig.EntryChecks {
		if !c.Passed {
			sig.EntryEligible = false
			failed = append(failed, c.NameVI)
		}
	}
	if sig.EntryEligible {
		sig.VerdictVI = "ĐỦ ĐIỀU KIỆN VÀO"
	} else {
		sig.VerdictVI = "CHƯA ĐỦ ĐIỀU KIỆN: " + strings.Join(failed, " · ")
	}
	return sig
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func deref(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}

func holdVI(cfg Config, sig SignalView) string {
	if cfg.MaxHoldEpochs > 0 {
		return fmt.Sprintf("%d mốc (%.2f ngày)", cfg.MaxHoldEpochs, sig.HoldingDays)
	}
	return fmt.Sprintf("%.0f ngày dự phóng (%d mốc)", sig.HoldingDays, sig.SettlementsInHold)
}

// gauge computes the shared figures: rates, cadence, basis, fees, the priced
// round trip and strategy.NetAPR.
func gauge(cfg Config, snap Snapshot, now time.Time, marginFrac float64) SignalView {
	sig := SignalView{
		EvaluatedAtMs: now.UnixMilli(), Symbol: snap.Symbol,
		ForecastRatePerIntervalBps: ptr(snap.ForecastRatePerIntervalFrac * 10_000),
		NextFundingTimeMs:          snap.NextFundingTimeMs,
		SpotMidQuote:               snap.SpotBook.MidPriceQuote, PerpMidQuote: snap.PerpBook.MidPriceQuote,
		SpotTakerFeeBps: snap.SpotTakerFeeBps, PerpTakerFeeBps: snap.PerpTakerFeeBps, FeeSourceVI: snap.FeeSourceVI,
		TrailingWindowDays: trailingWindow.Hours() / 24,
		CapitalPerNotional: 1 + marginFrac,
		RequiredDepthQuote: cfg.DepthMultiple * cfg.NotionalQuote,
	}
	if snap.NextFundingTimeMs > 0 {
		sig.TimeToSettleSec = int64(time.UnixMilli(snap.NextFundingTimeMs).Sub(now).Seconds())
	}
	if b, ok := basisBps(snap.SpotBook.MidPriceQuote, snap.PerpBook.MidPriceQuote); ok {
		sig.BasisBps = &b
	}
	if n := len(snap.Settled); n > 0 && snap.SettledErrVI == "" {
		last := snap.Settled[n-1]
		sig.LastSettledRatePerIntervalBps = ptr(last.RatePerIntervalFrac * 10_000)
		sig.LastSettledAtMs = last.SettledAtMs
	}

	intervalSec, cadenceWhy := int64(0), snap.SettledErrVI
	var meanFrac float64
	if snap.SettledErrVI == "" {
		intervalSec, cadenceWhy = measureIntervalSec(snap.Settled)
		meanFrac, sig.TrailingSettlements = trailingMean(snap.Settled, now.Add(-trailingWindow).UnixMilli())
		if cadenceWhy == "" {
			if why := cadenceAgreesWithNext(snap, intervalSec); why != "" {
				intervalSec, cadenceWhy = 0, why
			}
		}
	}
	sig.IntervalSec = intervalSec
	if sig.TrailingSettlements > 0 {
		sig.TrailingMeanRatePerIntervalBps = ptr(meanFrac * 10_000)
		sig.TrailingMeanRatePer8hBps = per8hBps(meanFrac, intervalSec)
	}
	sig.ForecastRatePer8hBps = per8hBps(snap.ForecastRatePerIntervalFrac, intervalSec)
	for _, b := range []depth.Summary{snap.SpotBook, snap.PerpBook} {
		for _, q := range []float64{b.BidDepthWithinWideQuote, b.AskDepthWithinWideQuote} {
			if sig.MinDepthWideQuote == nil || q < *sig.MinDepthWideQuote {
				v := q
				sig.MinDepthWideQuote = &v
			}
		}
	}

	switch {
	case snap.FeesErrVI != "":
		sig.NetAPRReasonVI = "không đọc được phí của tài khoản: " + snap.FeesErrVI + " — 'chưa tra' không phải 'miễn phí', không định giá"
		return sig
	case cadenceWhy != "":
		sig.NetAPRReasonVI = "không đo được chu kỳ funding: " + cadenceWhy
		return sig
	case sig.TrailingSettlements < minTrailingSettlements:
		sig.NetAPRReasonVI = fmt.Sprintf("chỉ %d mốc đã settle trong %.0f ngày — cần ít nhất %d", sig.TrailingSettlements, sig.TrailingWindowDays, minTrailingSettlements)
		return sig
	}

	cost := strategy.RoundTripCost(strategy.RoundTripInput{
		NotionalQuote: cfg.NotionalQuote,
		SpotFee: fees.Schedule{Source: snap.SpotBook.Source, TakerFeeBps: snap.SpotTakerFeeBps, Verified: true,
			NoteVI: "đọc từ tài khoản TESTNET: " + snap.FeeSourceVI},
		PerpFee: fees.Schedule{Source: snap.PerpBook.Source, TakerFeeBps: snap.PerpTakerFeeBps, Verified: true,
			NoteVI: "đọc từ tài khoản TESTNET: " + snap.FeeSourceVI},
		SpotBook: snap.SpotBook, PerpBook: snap.PerpBook,
		At: now, MaxBookAge: maxBookAge,
	})
	sig.HoldingDays, sig.SettlementsInHold = holdPlan(cfg, intervalSec)
	res := strategy.NetAPR(strategy.NetAPRInput{
		Source: snap.PerpBook.Source, Symbol: snap.PerpBook.Symbol, Model: exchanges.FundingDiscrete,
		RatePerIntervalFrac: meanFrac, IntervalSec: intervalSec, HoldingDays: sig.HoldingDays, Cost: cost,
	})
	sig.AppliedVI, sig.ExcludedVI = res.AppliedVI, res.ExcludedVI
	if !res.OK {
		sig.NetAPRReasonVI = res.ReasonVI
		return sig
	}
	if cfg.MaxHoldEpochs > 0 && res.SettlementsInHold != sig.SettlementsInHold {
		// strategy counts floor(hold / interval); a hold of exactly N intervals
		// must count N. If float arithmetic ever made it N-1, the figure would be
		// about a different hold than the one the bot will run — refused, not
		// patched.
		sig.NetAPRReasonVI = fmt.Sprintf("strategy đếm %d mốc cho kế hoạch giữ %d mốc — không công bố con số về một kỳ giữ khác", res.SettlementsInHold, sig.SettlementsInHold)
		return sig
	}
	sig.SettlementsInHold = res.SettlementsInHold
	sig.FeesPct, sig.SlippagePct, sig.RoundTripCostPct = ptr(cost.FeesPct), ptr(cost.SlippagePct), ptr(cost.TotalPct)
	sig.EntryCostPct = ptr(cost.EntrySpotBuy.SlippagePct + cost.EntryPerpSell.SlippagePct)
	sig.GrossAPRPct = ptr(res.GrossAPRFrac * 100)
	sig.NetAPRPct = ptr(res.NetAPRFrac * 100)
	sig.NetAPROnCapitalPct = ptr(res.NetAPRFrac / sig.CapitalPerNotional * 100)
	sig.AppliedVI = append(append([]string(nil), res.AppliedVI...),
		fmt.Sprintf("Lãi suất dự phóng: trung bình %d mốc ĐÃ SETTLE trong %.0f ngày qua, không phải mức đang hình thành.", sig.TrailingSettlements, sig.TrailingWindowDays),
		fmt.Sprintf("Vốn: spot trọn notional + ký quỹ perp %.0f%% notional ⇒ %.2f× notional; APR trên vốn = APR trên notional ÷ %.2f.", marginFrac*100, sig.CapitalPerNotional, sig.CapitalPerNotional),
		"Phí và sổ lệnh là của TESTNET — mainnet thu phí khác và sổ dày khác.")
	return sig
}

// depthCheck asks each side the round trip takes — spot ask and perp bid on the
// way in, spot bid and perp ask on the way out — for DepthMultiple × notional
// inside ±0.5% of mid. A side whose book stops short of the window reports a
// floor: enough if the floor already clears, unknown if it does not.
func depthCheck(cfg Config, snap Snapshot) (passed, evaluated bool, detailVI string) {
	need := cfg.DepthMultiple * cfg.NotionalQuote
	sides := []struct {
		name  string
		book  depth.Summary
		quote float64
		span  float64
	}{
		{"spot ask (vào)", snap.SpotBook, snap.SpotBook.AskDepthWithinWideQuote, snap.SpotBook.AskSpanPct},
		{"perp bid (vào)", snap.PerpBook, snap.PerpBook.BidDepthWithinWideQuote, snap.PerpBook.BidSpanPct},
		{"spot bid (ra)", snap.SpotBook, snap.SpotBook.BidDepthWithinWideQuote, snap.SpotBook.BidSpanPct},
		{"perp ask (ra)", snap.PerpBook, snap.PerpBook.AskDepthWithinWideQuote, snap.PerpBook.AskSpanPct},
	}
	// A side that falls short with its whole window published is a definite
	// failure; one that falls short of a floor is unknown. Any definite failure
	// makes the check evaluated and failed, whatever the other sides are.
	definiteFail, unknown := false, false
	var parts []string
	for _, s := range sides {
		if !s.book.OK() {
			unknown = true
			parts = append(parts, s.name+" không có sổ lệnh ("+s.book.ErrVI+")")
			continue
		}
		floor := s.span < depth.WindowWidePct
		mark := ""
		if floor {
			mark = "≥"
		}
		parts = append(parts, fmt.Sprintf("%s %s%.0f", s.name, mark, s.quote))
		switch {
		case s.quote >= need:
		case floor:
			unknown = true
		default:
			definiteFail = true
		}
	}
	return !definiteFail && !unknown, definiteFail || !unknown, fmt.Sprintf("cần %.0f quote mỗi phía: %s", need, strings.Join(parts, " · "))
}

// exitAssessment is what one reading says about a held pair.
type exitAssessment struct {
	Due                  bool
	ReasonsVI            []string
	Checks               []CheckView
	SettlementsSinceOpen int
	BasisWidenBps        *float64
}

// assessExit runs every exit check on a held pair. A check that cannot be
// evaluated never closes the pair: missing data is not a reason to trade.
func assessExit(cfg Config, snap Snapshot, pos PositionView) exitAssessment {
	var out exitAssessment
	add := func(name string, evaluated, exit bool, detail string) {
		// Passed is "no exit", so the page reads green as "keep holding".
		out.Checks = append(out.Checks, CheckView{NameVI: name, Evaluated: evaluated, Passed: !(evaluated && exit), DetailVI: detail})
		if evaluated && exit {
			out.Due = true
			out.ReasonsVI = append(out.ReasonsVI, name+" ("+detail+")")
		}
	}

	var after []SettledRate
	for _, s := range snap.Settled {
		if s.SettledAtMs > pos.OpenedAtMs {
			after = append(after, s)
		}
	}
	sort.SliceStable(after, func(i, j int) bool { return after[i].SettledAtMs < after[j].SettledAtMs })
	out.SettlementsSinceOpen = len(after)

	const fundingName = "Mốc settle gần nhất sau khi vào ≤ 0"
	switch {
	case snap.SettledErrVI != "":
		add(fundingName, false, false, "không đọc được lịch sử funding: "+snap.SettledErrVI)
	case len(after) == 0:
		add(fundingName, true, false, "chưa có mốc settle nào sau khi vào")
	default:
		last := after[len(after)-1]
		add(fundingName, true, last.RatePerIntervalFrac <= 0,
			fmt.Sprintf("mốc %s settle %+.4f bps", time.UnixMilli(last.SettledAtMs).Format("02/01 15:04"), last.RatePerIntervalFrac*10_000))
	}

	const epochName = "Đã giữ qua đủ số mốc settle"
	switch {
	case cfg.MaxHoldEpochs == 0:
		add(epochName, true, false, fmt.Sprintf("không giới hạn (giữ khi funding còn dương) · đã qua %d mốc", len(after)))
	case snap.SettledErrVI != "":
		add(epochName, false, false, "không đọc được lịch sử funding: "+snap.SettledErrVI)
	default:
		add(epochName, true, len(after) >= cfg.MaxHoldEpochs, fmt.Sprintf("đã qua %d / %d mốc sàn liệt kê sau lúc vào", len(after), cfg.MaxHoldEpochs))
	}

	const basisName = "Basis giãn quá ngưỡng so với lúc vào"
	now, okNow := basisBps(snap.SpotBook.MidPriceQuote, snap.PerpBook.MidPriceQuote)
	switch {
	case !okNow:
		add(basisName, false, false, "không có giá giữa của hai sổ lệnh")
	case pos.EntryBasisBps == 0 && pos.Adopted:
		add(basisName, false, false, "vị thế tiếp nhận không mang giá giữa lúc vào — không đo được độ giãn")
	default:
		widen := now - pos.EntryBasisBps
		out.BasisWidenBps = &widen
		add(basisName, true, widen > cfg.MaxBasisWidenBps,
			fmt.Sprintf("basis %+.2f bps, lúc vào %+.2f bps, giãn %+.2f > %.0f?", now, pos.EntryBasisBps, widen, cfg.MaxBasisWidenBps))
	}
	return out
}
