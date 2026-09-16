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

	// The STRICTER of the two markets' order rules for this symbol, read from
	// the venues' own exchangeInfo — never a mainnet snapshot and never a
	// constant (PLAN 4.2). Both legs round onto the coarser step, so one set of
	// three numbers describes the pair. RulesErrVI set, or any of them zero,
	// means they could not be read, and the size check then refuses instead of
	// treating "not read" as "no limit".
	StepSizeCoin     float64
	MinQtyCoin       float64
	MinNotionalQuote float64
	RulesErrVI       string
}

// CheckView is one condition, with the numbers behind it.
type CheckView struct {
	// Key names the condition for a program — stable across wording changes of
	// NameVI, which is for a person.
	Key    CheckKey `json:"key"`
	NameVI string   `json:"name_vi"`
	// Passed is the condition holding. Evaluated false means its input could
	// not be read, and Passed is then false for an entry and "no exit" for an
	// exit — never a guess in the direction of trading.
	Passed    bool   `json:"passed"`
	Evaluated bool   `json:"evaluated"`
	DetailVI  string `json:"detail_vi"`
}

// CheckKey is a condition's stable name.
type CheckKey string

// The entry conditions, then the exit conditions.
const (
	CheckFlat                CheckKey = "flat"
	CheckFormingPositive     CheckKey = "forming_positive"
	CheckLastSettledPositive CheckKey = "last_settled_positive"
	CheckEntryBasis          CheckKey = "entry_basis"
	CheckSizeFits            CheckKey = "size_fits"
	CheckNetAPR              CheckKey = "net_apr"
	CheckDepth               CheckKey = "depth"
	CheckClock               CheckKey = "clock"
	CheckTimeToSettle        CheckKey = "time_to_settle"

	CheckExitFunding    CheckKey = "exit_funding"
	CheckExitEpochs     CheckKey = "exit_epochs"
	CheckExitBasis      CheckKey = "exit_basis"
	CheckExitTakeProfit CheckKey = "exit_take_profit"
)

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

	// The smallest notional this symbol may be opened at, and what set it: the
	// venue's own minimums or the quantization guard (capital.go). nil when the
	// rules could not be read.
	SizeFloorQuote *float64 `json:"size_floor_quote"`
	SizeFloorVI    string   `json:"size_floor_vi"`
	StepSizeCoin   float64  `json:"step_size_coin"`
	// SizeErrorPct is the share of the notional the venue's step size leaves
	// unspent — NOT a hedge error, which execution makes zero (capital.go).
	SizeErrorPct   *float64 `json:"size_error_pct"`
	PlannedQtyCoin *float64 `json:"planned_qty_coin"`

	// A HELD pair's running result as this reading prices it (holdingResult),
	// and the early take-profit threshold it is measured against. All nil on a
	// flat pair and on a held one whose inputs could not be read;
	// HoldingResultReasonVI then says which.
	HoldingFundingQuote       *float64 `json:"holding_funding_quote"`
	HoldingDriftQuote         *float64 `json:"holding_drift_quote"`
	HoldingEntryFeeQuote      *float64 `json:"holding_entry_fee_quote"`
	HoldingExitCostQuote      *float64 `json:"holding_exit_cost_quote"`
	HoldingCashResultQuote    *float64 `json:"holding_cash_result_quote"`
	HoldingReturnOnCapitalPct *float64 `json:"holding_return_on_capital_pct"`
	TakeProfitTargetPct       float64  `json:"take_profit_target_pct"`
	HoldingResultReasonVI     string   `json:"holding_result_reason_vi"`
	HoldingResultLabelVI      string   `json:"holding_result_label_vi"`

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

	add := func(key CheckKey, name string, evaluated, passed bool, detail string) {
		sig.EntryChecks = append(sig.EntryChecks, CheckView{Key: key, NameVI: name, Evaluated: evaluated, Passed: evaluated && passed, DetailVI: detail})
	}

	if in.Flat {
		add(CheckFlat, "Không có vị thế nào mở trên symbol", true, true, "sàn báo hai chân phẳng")
	} else {
		add(CheckFlat, "Không có vị thế nào mở trên symbol", true, false, "sàn đang giữ: "+in.HeldVI+" — bot không mở thêm và không quản lý vị thế không phải của nó")
	}

	add(CheckFormingPositive, "Funding đang hình thành > 0", true, snap.ForecastRatePerIntervalFrac > 0,
		fmt.Sprintf("premiumIndex.lastFundingRate %+.4f bps mỗi chu kỳ của symbol — chưa settle, chỉ để chặn, không dùng để dự phóng", snap.ForecastRatePerIntervalFrac*10_000))

	switch {
	case snap.SettledErrVI != "":
		add(CheckLastSettledPositive, "Mốc settle gần nhất > 0", false, false, "không đọc được lịch sử funding: "+snap.SettledErrVI)
	case sig.LastSettledRatePerIntervalBps == nil:
		add(CheckLastSettledPositive, "Mốc settle gần nhất > 0", false, false, "sàn không liệt kê mốc settle nào trong cửa sổ")
	default:
		add(CheckLastSettledPositive, "Mốc settle gần nhất > 0", true, *sig.LastSettledRatePerIntervalBps > 0,
			fmt.Sprintf("%+.4f bps, settle lúc %s", *sig.LastSettledRatePerIntervalBps, time.UnixMilli(sig.LastSettledAtMs).Format("02/01 15:04")))
	}

	// Trụ cột 1 — the entry basis filter. A hedged pair collects
	// (basis at entry − basis at exit) on top of the funding, so a perp trading
	// ABOVE the spot is the discount this strategy is paid for and a perp
	// trading below it is the same amount paid out. Entering on a dip also
	// arms the widening exit: the book recovers, the basis climbs back towards
	// zero, and a stop sized for a structural break fires on a recovery.
	const basisEntryName = "Basis lúc vào ≥ ngưỡng"
	switch {
	case sig.BasisBps == nil:
		add(CheckEntryBasis, basisEntryName, false, false, "không có giá giữa của cả hai sổ lệnh — không đo được basis")
	default:
		add(CheckEntryBasis, basisEntryName, true, *sig.BasisBps >= cfg.MinEntryBasisBps,
			fmt.Sprintf("perp trên spot %+.2f bps, cần ≥ %+.2f bps (vào lúc basis lõm là trả trước phần hội tụ, không phải thu)",
				*sig.BasisBps, cfg.MinEntryBasisBps))
	}

	// The venue's own floor on this size, and the quantization guard. A
	// notional the venue would refuse is not merely a wasted order: the portal
	// refuses it before placing, the engine counts that as a failed trade, and
	// five in a row halt the pair. A notional only a few steps wide opens
	// perfectly hedged and deploys far less of its slot than the allocation
	// says it does (capital.go).
	const sizeName = "Quy mô đủ lớn cho luật sàn và bước nhảy"
	floor, floorWhy, floorOK := sizeFloorQuote(snap.StepSizeCoin, snap.MinQtyCoin, snap.MinNotionalQuote, snap.SpotBook.MidPriceQuote)
	switch {
	case snap.RulesErrVI != "":
		add(CheckSizeFits, sizeName, false, false, "không đọc được luật sàn: "+snap.RulesErrVI)
	case !floorOK:
		add(CheckSizeFits, sizeName, false, false, floorWhy)
	default:
		sig.SizeFloorQuote, sig.SizeFloorVI = ptr(floor), floorWhy
		add(CheckSizeFits, sizeName, true, cfg.NotionalQuote >= floor,
			fmt.Sprintf("notional %.2f quote ≥ sàn %.2f (%s)? · bước %.8f ⇒ %.2f%% quy mô không vào được thị trường, trần %.0f%%",
				cfg.NotionalQuote, floor, floorWhy, snap.StepSizeCoin, deref(sig.SizeErrorPct), maxQuantizationErrorFrac*100))
	}

	aprDetail := sig.NetAPRReasonVI
	if sig.NetAPRPct != nil {
		aprDetail = fmt.Sprintf("%.2f%%/năm trên notional một chân (%.2f%% trên vốn) ≥ %.2f%%? · trung bình %d mốc đã settle, giữ %s, chi phí vòng %.4f%%",
			*sig.NetAPRPct, deref(sig.NetAPROnCapitalPct), cfg.MinNetAPRPct, sig.TrailingSettlements, holdVI(cfg, sig), deref(sig.RoundTripCostPct))
	}
	add(CheckNetAPR, "Net APR dự phóng ≥ ngưỡng", sig.NetAPRPct != nil, sig.NetAPRPct != nil && *sig.NetAPRPct >= cfg.MinNetAPRPct, aprDetail)

	depthOK, depthEval, depthDetail := depthCheck(cfg, snap)
	add(CheckDepth, fmt.Sprintf("Độ sâu ±0,5%% ≥ %.0f× notional ở cả 4 phía", cfg.DepthMultiple), depthEval, depthOK, depthDetail)

	const clockName = "Lệch đồng hồ sàn ≤ 1000 ms"
	if snap.SpotClockSkewMs == nil || snap.PerpClockSkewMs == nil {
		add(CheckClock, clockName, false, false, "chưa đo được đồng hồ của cả hai sàn")
	} else {
		spot, perp := *snap.SpotClockSkewMs, *snap.PerpClockSkewMs
		worst := max(abs64(spot), abs64(perp))
		add(CheckClock, clockName, true, worst <= maxClockSkewMs, fmt.Sprintf("spot %+d ms, futures %+d ms", spot, perp))
	}

	switch {
	case snap.NextFundingTimeMs <= 0:
		add(CheckTimeToSettle, "Còn đủ xa mốc settle kế tiếp", false, false, "sàn không cho nextFundingTime")
	default:
		left := time.UnixMilli(snap.NextFundingTimeMs).Sub(in.Now)
		add(CheckTimeToSettle, "Còn đủ xa mốc settle kế tiếp", true, left > cfg.MinTimeToSettle,
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
	sig.StepSizeCoin = snap.StepSizeCoin
	// What the size really becomes on the venue's grid: the quantity floors onto
	// the coarser step, and what floors away never reaches the market.
	if snap.StepSizeCoin > 0 && snap.SpotBook.MidPriceQuote > 0 && cfg.NotionalQuote > 0 {
		qty := math.Floor(cfg.NotionalQuote/snap.SpotBook.MidPriceQuote/snap.StepSizeCoin) * snap.StepSizeCoin
		sig.PlannedQtyCoin = ptr(qty)
		sig.SizeErrorPct = ptr((cfg.NotionalQuote - qty*snap.SpotBook.MidPriceQuote) / cfg.NotionalQuote * pctPerUnit)
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

// fillHolding puts a held pair's running result on the gauge. It is called only
// for a pair the bot HOLDS: on a flat pair every one of these stays nil, so a
// reader cannot mistake a scan for a position.
func (sig *SignalView) fillHolding(cfg Config, r holdingResult) {
	sig.TakeProfitTargetPct = cfg.TargetTakeProfitNetPct
	sig.HoldingResultLabelVI = holdingResultLabelVI
	if !r.OK {
		sig.HoldingResultReasonVI = r.ReasonVI
		return
	}
	sig.HoldingFundingQuote, sig.HoldingDriftQuote = ptr(r.FundingQuote), ptr(r.DriftQuote)
	sig.HoldingEntryFeeQuote, sig.HoldingExitCostQuote = ptr(r.EntryFeeQuote), ptr(r.ExitCostQuote)
	sig.HoldingCashResultQuote, sig.HoldingReturnOnCapitalPct = ptr(r.CashResultQuote), ptr(r.ReturnOnCapitalPct)
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

// The two conversions this file does by hand, named so a reader can check them
// against the identifier's unit (rule 4).
const (
	bpsPerUnit = 10_000.0
	pctPerUnit = 100.0
)

// holdingResult is what a held pair has made SO FAR, as ONE reading prices it:
//
//	CashResultQuote = funding the venue's published rates imply
//	                + the pair's drift from its two ENTRY FILLS to the current mids
//	                − the commission the two entry fills paid
//	                − what closing on the CURRENT book would cost
//
// Entry SLIPPAGE is deliberately NOT a term of its own. The drift is measured
// from the FILL price, not from the mid the decision was taken on, so whatever
// the entry gave up crossing the spread is already inside it; subtracting it
// again is the double count PLAN 4.5e records against RealizedQuote + drift.
//
// Three things this figure is not, and each one travels on holdingResultLabelVI:
//
//   - It is not the funding the VENUE paid. The engine reads settled RATES and
//     multiplies by the perp leg's notional AT ENTRY, because the mark at a past
//     settlement is not republished (measured on the testnet 2026-09-13: the
//     venue's own funding row and the book's figure differed by -0.0228%, all
//     of it the mark). cmd/paperledger and the PnL page read the FUNDING_FEE rows;
//     this does not, and must not be read as if it did.
//   - It is not "net" (CLAUDE.md rule 2). The exit half is an ESTIMATE from a
//     book that will have moved by the time the order is sent, and the five
//     costs internal/strategy excludes by name are still excluded here.
//   - It is not realized. Nothing is realized until execution proves both legs
//     flat and reads the fills back from the venue.
//
// OK false means NO number here may be used — not that the result is zero.
type holdingResult struct {
	OK       bool
	ReasonVI string

	FundingQuote  float64
	DriftQuote    float64
	EntryFeeQuote float64
	ExitCostQuote float64

	CashResultQuote    float64
	CapitalQuote       float64
	ReturnOnCapitalPct float64
}

// holdingResultLabelVI travels with every figure holdingResult produces.
const holdingResultLabelVI = "TẠM TÍNH, chưa hiện thực hoá: funding suy ra từ RATE sàn công bố nhân notional chân perp LÚC VÀO " +
	"(không phải các dòng FUNDING_FEE sàn đã trả) + trôi giá từ GIÁ KHỚP lúc vào tới giá giữa lượt quét này " +
	"− phí taker hai lượt khớp lúc vào − phí và trượt giá ƯỚC TÍNH để đóng trên sổ lệnh HIỆN TẠI. " +
	"Trượt giá lúc vào đã nằm trong phần trôi giá, không trừ lần hai. Không phải lãi ròng."

// priceHolding prices one held pair from one reading. after is the settlements
// the venue listed strictly after the open, which is also what the funding and
// epoch exits count (rule 6: counted, never derived from a duration).
//
// now is the instant it is being priced AT, passed in and never read from the
// clock, the same contract internal/strategy holds itself to. It is here for
// one reason: this figure is the only one that closes a position for a GAIN,
// and it is computed from two mids. A book minutes old makes a drift the market
// does not have, and a take-profit acting on it pays a real round trip for an
// imagined profit. The basis STOP deliberately has no such guard — a stop that
// silently stops working when a book ages is worse than one acting on a stale
// price, and its job is to act.
func priceHolding(snap Snapshot, pos PositionView, after []SettledRate, now time.Time) holdingResult {
	out := holdingResult{ReasonVI: ""}
	switch {
	case snap.FeesErrVI != "":
		out.ReasonVI = "không đọc được phí của tài khoản: " + snap.FeesErrVI + " — 'chưa tra' không phải 'miễn phí'"
		return out
	case snap.SettledErrVI != "":
		out.ReasonVI = "không đọc được lịch sử funding: " + snap.SettledErrVI
		return out
	case !(pos.QtyCoin > 0):
		out.ReasonVI = "không biết khối lượng hai chân đang giữ"
		return out
	case !(pos.SpotEntryAvgQuote > 0) || !(pos.PerpEntryAvgQuote > 0):
		out.ReasonVI = "không có giá khớp lúc vào của cả hai chân (vị thế tiếp nhận thiếu file ý định?)"
		return out
	case !(snap.SpotBook.MidPriceQuote > 0) || !(snap.PerpBook.MidPriceQuote > 0):
		out.ReasonVI = "không có giá giữa của cả hai sổ lệnh"
		return out
	case !(pos.CapitalQuote > 0):
		out.ReasonVI = "không biết vốn cặp này đang khoá"
		return out
	}
	for _, b := range []depth.Summary{snap.SpotBook, snap.PerpBook} {
		if age := now.Sub(time.UnixMilli(b.SampledAtMs)); b.SampledAtMs <= 0 || age > maxBookAge || age < -maxBookAge {
			out.ReasonVI = fmt.Sprintf("sổ %s đo lúc %s, cách lúc đánh giá %s — quá %s, không định giá chốt lời trên giá cũ",
				b.Source, time.UnixMilli(b.SampledAtMs).Format("15:04:05"), age.Round(time.Second), maxBookAge)
			return out
		}
	}

	qty := pos.QtyCoin
	spotEntryNotional := qty * pos.SpotEntryAvgQuote
	perpEntryNotional := qty * pos.PerpEntryAvgQuote

	// The SHORT perp leg is paid the rate on its own notional at each
	// settlement. Only the settlements strictly after the open count — holding
	// 7h59m of an 8h period pays nothing (rule 6) — and a Special rate is a
	// settlement the account really crossed, so it is counted here even though
	// the cadence and the entry mean leave it out.
	rateSum := 0.0
	for _, r := range after {
		rateSum += r.RatePerIntervalFrac
	}
	out.FundingQuote = rateSum * perpEntryNotional
	out.DriftQuote = (snap.SpotBook.MidPriceQuote-pos.SpotEntryAvgQuote)*qty + (pos.PerpEntryAvgQuote-snap.PerpBook.MidPriceQuote)*qty
	out.EntryFeeQuote = spotEntryNotional*snap.SpotTakerFeeBps/bpsPerUnit + perpEntryNotional*snap.PerpTakerFeeBps/bpsPerUnit

	// The exit is priced at what the two legs are worth NOW — the 4.3 rule that
	// an exit leg is sized at the coins' current value, never at the entry
	// notional — against the sides it would really take: SELL the spot, BUY the
	// perp back. A book that cannot absorb either one prices nothing: a refused
	// fill contributing zero cost is how a loss is shown as a profit.
	spotExitNotional := qty * snap.SpotBook.MidPriceQuote
	perpExitNotional := qty * snap.PerpBook.MidPriceQuote
	spotSell := strategy.EstimateFill(snap.SpotBook, strategy.SideSell, spotExitNotional)
	perpBuy := strategy.EstimateFill(snap.PerpBook, strategy.SideBuy, perpExitNotional)
	if !spotSell.Fillable {
		out.ReasonVI = "sổ spot không hấp thụ nổi lệnh BÁN để đóng: " + spotSell.ReasonVI
		return out
	}
	if !perpBuy.Fillable {
		out.ReasonVI = "sổ perp không hấp thụ nổi lệnh MUA để đóng: " + perpBuy.ReasonVI
		return out
	}
	out.ExitCostQuote = spotExitNotional*(snap.SpotTakerFeeBps/bpsPerUnit+spotSell.SlippagePct/pctPerUnit) +
		perpExitNotional*(snap.PerpTakerFeeBps/bpsPerUnit+perpBuy.SlippagePct/pctPerUnit)

	out.CashResultQuote = out.FundingQuote + out.DriftQuote - out.EntryFeeQuote - out.ExitCostQuote
	out.CapitalQuote = pos.CapitalQuote
	out.ReturnOnCapitalPct = out.CashResultQuote / out.CapitalQuote * pctPerUnit
	if !finite(out.CashResultQuote) || !finite(out.ReturnOnCapitalPct) {
		out.ReasonVI = "kết quả tạm tính không phải số hữu hạn — không công bố"
		return out
	}
	out.OK = true
	return out
}

// exitAssessment is what one reading says about a held pair.
type exitAssessment struct {
	Due                  bool
	ReasonsVI            []string
	Checks               []CheckView
	SettlementsSinceOpen int
	BasisWidenBps        *float64
	// Result is the running result the take-profit exit is judged on, priced
	// whether or not it fired; Result.OK false says why it could not be.
	Result holdingResult
}

// assessExit runs every exit check on a held pair. A check that cannot be
// evaluated never closes the pair: missing data is not a reason to trade.
func assessExit(cfg Config, snap Snapshot, pos PositionView, now time.Time) exitAssessment {
	var out exitAssessment
	// reasonVI empty takes the default shape, "name (detail)"; a check whose
	// wording is a contract of its own passes the whole sentence.
	addReason := func(key CheckKey, name string, evaluated, exit bool, detail, reasonVI string) {
		// Passed is "no exit", so the page reads green as "keep holding".
		out.Checks = append(out.Checks, CheckView{Key: key, NameVI: name, Evaluated: evaluated, Passed: !(evaluated && exit), DetailVI: detail})
		if evaluated && exit {
			out.Due = true
			if reasonVI == "" {
				reasonVI = name + " (" + detail + ")"
			}
			out.ReasonsVI = append(out.ReasonsVI, reasonVI)
		}
	}
	add := func(key CheckKey, name string, evaluated, exit bool, detail string) {
		addReason(key, name, evaluated, exit, detail, "")
	}

	var after []SettledRate
	for _, s := range snap.Settled {
		if s.SettledAtMs > pos.OpenedAtMs {
			after = append(after, s)
		}
	}
	sort.SliceStable(after, func(i, j int) bool { return after[i].SettledAtMs < after[j].SettledAtMs })
	out.SettlementsSinceOpen = len(after)

	const fundingName = "Mốc settle sau khi vào ≤ 0"
	switch {
	case snap.SettledErrVI != "":
		add(CheckExitFunding, fundingName, false, false, "không đọc được lịch sử funding: "+snap.SettledErrVI)
	case len(after) == 0:
		add(CheckExitFunding, fundingName, true, false, "chưa có mốc settle nào sau khi vào")
	case cfg.MinHoldEpochs > 0 && len(after) < cfg.MinHoldEpochs:
		// Trụ cột 2 — the amortization floor. The funding exit is forbidden
		// here whatever the rate did; the take-profit and the basis stop below
		// are NOT, so a position that has already earned its round trip back,
		// or one whose basis broke, still leaves.
		last := after[len(after)-1]
		add(CheckExitFunding, fundingName, true, false,
			fmt.Sprintf("đang trong sàn giữ tối thiểu %d mốc để khấu hao phí vòng (đã qua %d mốc · mốc gần nhất %+.4f bps) — lối thoát funding khoá, chốt lời và cắt lỗ basis vẫn chạy",
				cfg.MinHoldEpochs, len(after), last.RatePerIntervalFrac*bpsPerUnit))
	default:
		// Trụ cột 4 — hysteresis. Past the amortization floor the pair leaves
		// only on a RUN of settlements at or below the configured charge: one
		// print is noise beside a round trip, and paying the trip to dodge it
		// is the churn the retired step-3.3 rule measured.
		floorFrac := cfg.ExitNegativeFundingRateBps / bpsPerUnit
		consecutiveNeg := 0
		for i := len(after) - 1; i >= 0; i-- {
			if after[i].RatePerIntervalFrac <= floorFrac {
				consecutiveNeg++
			} else {
				break
			}
		}
		need := cfg.ExitNegativeConsecutiveEpochs
		last := after[len(after)-1]
		lastVI := func(r SettledRate) string {
			return fmt.Sprintf("mốc %s: %+.2f bps", time.UnixMilli(r.SettledAtMs).Format("02/01 15:04"), r.RatePerIntervalFrac*bpsPerUnit)
		}
		exitDue := false
		detail := ""
		switch {
		case need > 0 && consecutiveNeg >= need:
			exitDue = true
			var runVI []string
			for _, r := range after[len(after)-consecutiveNeg:] {
				runVI = append(runVI, lastVI(r))
			}
			detail = fmt.Sprintf("%d mốc liên tiếp ≤ %.1f bps, cần %d (%s)", consecutiveNeg, cfg.ExitNegativeFundingRateBps, need, strings.Join(runVI, " · "))
		case cfg.MinHoldEpochs == 0 && last.RatePerIntervalFrac <= 0:
			// No amortization floor: the step-3.2 rule, kept so a run configured
			// the old way behaves exactly the old way.
			exitDue = true
			detail = fmt.Sprintf("mốc %s settle %+.4f bps ≤ 0", time.UnixMilli(last.SettledAtMs).Format("02/01 15:04"), last.RatePerIntervalFrac*bpsPerUnit)
		case last.RatePerIntervalFrac <= 0:
			detail = fmt.Sprintf("%s — chưa kích hoạt thoát: mới %d mốc liên tiếp ≤ %.1f bps, cần %d (tránh trả trọn vòng phí để né một khoản âm nhỏ hơn nhiều)",
				lastVI(last), consecutiveNeg, cfg.ExitNegativeFundingRateBps, need)
		default:
			detail = fmt.Sprintf("mốc %s settle %+.4f bps > 0",
				time.UnixMilli(last.SettledAtMs).Format("02/01 15:04"), last.RatePerIntervalFrac*bpsPerUnit)
		}
		add(CheckExitFunding, fundingName, true, exitDue, detail)
	}

	const epochName = "Đã giữ qua đủ số mốc settle"
	switch {
	case cfg.MaxHoldEpochs == 0:
		add(CheckExitEpochs, epochName, true, false, fmt.Sprintf("không giới hạn (giữ khi funding còn dương) · đã qua %d mốc", len(after)))
	case snap.SettledErrVI != "":
		add(CheckExitEpochs, epochName, false, false, "không đọc được lịch sử funding: "+snap.SettledErrVI)
	default:
		add(CheckExitEpochs, epochName, true, len(after) >= cfg.MaxHoldEpochs, fmt.Sprintf("đã qua %d / %d mốc sàn liệt kê sau lúc vào", len(after), cfg.MaxHoldEpochs))
	}

	const basisName = "Basis giãn quá ngưỡng so với lúc vào"
	nowBps, okNow := basisBps(snap.SpotBook.MidPriceQuote, snap.PerpBook.MidPriceQuote)
	switch {
	case !okNow:
		add(CheckExitBasis, basisName, false, false, "không có giá giữa của hai sổ lệnh")
	case pos.EntryBasisBps == 0 && pos.Adopted:
		add(CheckExitBasis, basisName, false, false, "vị thế tiếp nhận không mang giá giữa lúc vào — không đo được độ giãn")
	default:
		widen := nowBps - pos.EntryBasisBps
		out.BasisWidenBps = &widen
		addReason(CheckExitBasis, basisName, true, widen > cfg.MaxBasisWidenBps,
			fmt.Sprintf("basis %+.2f bps, lúc vào %+.2f bps, giãn %+.2f > %.0f?", nowBps, pos.EntryBasisBps, widen, cfg.MaxBasisWidenBps),
			fmt.Sprintf("Cắt lỗ basis nổ: Basis giãn %+.1f bps > %.0f bps so với lúc vào", widen, cfg.MaxBasisWidenBps))
	}

	// Trụ cột 3 — take profit on convergence. Priced on every reading, whether
	// or not it fires, so the page can show how far a held pair is from it; a
	// reading that could not be priced never closes anything.
	out.Result = priceHolding(snap, pos, after, now)
	const takeProfitName = "Chốt lời hội tụ Basis"
	switch {
	case cfg.TargetTakeProfitNetPct <= 0:
		add(CheckExitTakeProfit, takeProfitName, true, false,
			"không đặt ngưỡng chốt lời sớm (0 = tắt) — vị thế giữ tới khi funding hoặc basis quyết định")
	case !out.Result.OK:
		add(CheckExitTakeProfit, takeProfitName, false, false, "không định giá được kết quả tạm tính: "+out.Result.ReasonVI)
	default:
		r := out.Result
		addReason(CheckExitTakeProfit, takeProfitName, true, r.ReturnOnCapitalPct >= cfg.TargetTakeProfitNetPct,
			fmt.Sprintf("tạm tính %+.4f quote trên vốn %.2f = %+.2f%%, cần ≥ %+.2f%% · funding %+.4f, trôi giá %+.4f, phí vào %.4f, đóng ước %.4f",
				r.CashResultQuote, r.CapitalQuote, r.ReturnOnCapitalPct, cfg.TargetTakeProfitNetPct,
				r.FundingQuote, r.DriftQuote, r.EntryFeeQuote, r.ExitCostQuote),
			fmt.Sprintf("Chốt lời hội tụ Basis: Net PnL %+.2f%% trên vốn ≥ ngưỡng %+.2f%%", r.ReturnOnCapitalPct, cfg.TargetTakeProfitNetPct))
	}
	return out
}
