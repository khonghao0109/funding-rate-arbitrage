// Package autotrade is the Binance-TESTNET auto-trader of cmd/execportal
// (PLAN §7.1 Q18): a loop that reads both testnet markets for every symbol the
// portal may trade, decides which of Strategy 1's pairs to hold, and asks the
// portal to open or close them.
//
// # What it may do, and what holds it there
//
// Q18 lifts ONE limit of Q15 — "no path from a live signal to an order" — and
// only on the testnet, only inside this package, and only through the portal's
// own open and close. Everything else of Q15/Q16/Q17 stays:
//
//   - It DECIDES and never EXECUTES. This package imports neither
//     internal/broker nor internal/execution (guard_test.go reads its imports):
//     an order reaches the venue only through the Trader it is handed, which in
//     the binary is the portal's own open/close — the same execution machine,
//     the same write lock, the same derived ClientOrderIDs as a button press.
//     Several pairs are held at once, but orders are sent ONE AT A TIME: the
//     portal's write lock admits one open or close, and this engine never asks
//     for two concurrently.
//   - Its signal is the TESTNET's own funding, books and account fees, read
//     through the Market it is handed. Not cmd/scanner, not the step-3.5
//     journal, not strategy.EvaluateEntry: the gate's decisions stay the gate's.
//     What it borrows from internal/strategy is the ARITHMETIC — RoundTripCost
//     and NetAPR, the only functions in the repository allowed to say "net".
//   - ONE position PER SYMBOL, and the invariant is execution's: both legs open
//     within the coarser step, or both flat. A hedge status that is neither —
//     unhedged, two proofs that disagree, a read that could not decide — halts
//     THAT PAIR and leaves every other pair running. It never squares anything
//     itself: that is LÀM PHẲNG, pressed by a person (the never-auto-reconcile
//     rule).
//   - The portfolio is bounded twice: at most MaxConcurrentPositions pairs held,
//     and the capital they tie up — the spot leg in full plus the perp margin —
//     never above TotalCapitalCapQuote. A halted pair keeps its place in both
//     counts until a person acknowledges it, because it may still hold legs.
//
// # The numbers it acts on (CLAUDE.md rules 2, 3, 6)
//
// Entry is priced by strategy.NetAPR on the mean of the SETTLED rates of the
// last seven days, at the cadence measured from their stamps, over the hold the
// bot actually plans — MaxHoldEpochs settlements, or ProjectionHoldDays when it
// holds while funding stays positive — with a round trip of taker fees read
// from THIS ACCOUNT and slippage estimated from the two books just read. The
// forming rate (premiumIndex lastFundingRate) is shown and must be positive, but
// nothing is projected on it. Eligible pairs are ranked by that figure. Exits
// count settlements the venue lists after the open and read the rate each one
// settled at; nothing multiplies an APR by a duration.
//
// On a testnet those inputs are the testnet's: its funding, its thin books and
// its fees (spot 0, futures taker 4 bps, measured 2026-09-15). A figure here is
// a statement about the testnet account, never about what mainnet would pay.
package autotrade

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// State is a pair's state — or, for the three portfolio values, the bot's.
type State string

const (
	StateDisabled        State = "disabled"
	StateIdleScanning    State = "idle_scanning"
	StateEvaluating      State = "evaluating"
	StateOpening         State = "opening"
	StateInPosition      State = "in_position"
	StateClosing         State = "closing"
	StateCooldown        State = "cooldown"
	StateEmergencyHalted State = "emergency_halted"

	// StateRunning is the PORTFOLIO switched on. A pair is never "running": it
	// is scanning, holding, cooling down or halted.
	StateRunning State = "running"
)

// stateVI is the badge text.
var stateVI = map[State]string{
	StateDisabled:        "TẮT",
	StateIdleScanning:    "ĐANG QUÉT",
	StateEvaluating:      "ĐANG ĐÁNH GIÁ",
	StateOpening:         "ĐANG MỞ LỆNH",
	StateInPosition:      "ĐANG GIỮ VỊ THẾ - HEDGED",
	StateClosing:         "ĐANG ĐÓNG LỆNH",
	StateCooldown:        "HỒI PHỤC",
	StateEmergencyHalted: "DỪNG BẢO VỆ",
	StateRunning:         "ĐANG CHẠY",
}

// Config is ONE PAIR's parameters. Durations carry their unit in the type;
// every other figure names it (rule 4). The scan cadence and the portfolio's
// two limits are PortfolioConfig's, because one loop scans every pair.
type Config struct {
	Symbol        string
	NotionalQuote float64

	// MinNetAPRPct is the entry floor on strategy.NetAPR, in PERCENT a year on
	// ONE LEG's notional.
	MinNetAPRPct float64

	// MinEntryBasisBps is the entry floor on the perp-over-spot basis, in basis
	// points of the spot mid. A hedged pair earns the basis it entered at minus
	// the basis it leaves at, so entering while the perp trades BELOW the spot
	// sells the convergence instead of buying it: the book recovers, the basis
	// widens back towards zero, and the exit gives up what the entry never
	// collected. Above the floor the position starts with the round trip partly
	// pre-paid. 0 admits a flat basis and a negative value admits a discount —
	// both are choices, not the shipped one.
	MinEntryBasisBps float64

	// MaxHoldEpochs closes the pair once this many settlements have been
	// listed by the venue after the open. 0 holds for as long as each
	// settlement pays — the operator's choice of 2026-09-15 — and projects the
	// entry over ProjectionHoldDays instead.
	MaxHoldEpochs int

	// MinHoldEpochs is the fee-amortization floor: how many settlements the
	// venue must list after the open before the funding exit may close the
	// pair at all. Opening costs the whole round trip up front — commission on
	// four fills plus slippage — and a single settlement pays a few hundredths
	// of it, so leaving on the first negative print realizes the entire cost to
	// avoid a charge worth a fraction of it (the step-3.3 measurement: the
	// retired rule paid 73 round trips a year per series). 0 restores the
	// step-3.2 rule — leave on ANY settlement at or below zero.
	MinHoldEpochs int

	// TargetTakeProfitNetPct closes a held pair EARLY, as soon as the reading
	// prices its running result at or above this many PERCENT of the capital
	// the pair ties up. It is the convergence half of the strategy: a basis
	// that collapses towards zero pays the round trip in days rather than in
	// the twenty to eighty settlements funding alone needs, and the capital is
	// then free for another pair. The figure it is compared against is
	// holdingResult.ReturnOnCapitalPct — an ESTIMATE, and what it does and does
	// not deduct is on that type. 0 disables it: a threshold of zero would
	// close on the first reading that rounds above break-even, which is the
	// churn MinHoldEpochs exists to stop.
	TargetTakeProfitNetPct float64

	// MaxExitSpreadBps is a brake on the TAKE-PROFIT exit alone: when a
	// reading has already cleared TargetTakeProfitNetPct but either book's
	// top is wider than this, the close is HELD BACK for one scan so the
	// order is not sent into a book the makers have stepped out of.
	//
	// It applies to nothing else. The basis stop, the funding exit, a stop
	// with close, and the kill switch all ignore it, because a wide book is
	// a reason to wait for a GAIN and never a reason to wait while a hedge
	// is breaking — deferring a risk exit is how a small loss becomes a
	// large one. 0 disables the brake.
	//
	// It is a ceiling in basis points on (best ask − best bid) ÷ mid, which
	// is depth.Summary.SpreadPct in another unit; it is recomputed here from
	// the two touch prices rather than read from that field so that a book
	// which never had the derived field filled cannot silently turn the
	// brake off.
	MaxExitSpreadBps float64

	// ExitNegativeFundingRateBps and ExitNegativeConsecutiveEpochs are the
	// hysteresis on the funding exit, past the MinHoldEpochs floor: the pair
	// leaves only once this many settlements IN A ROW have settled at or below
	// this rate, per interval, in basis points. The rate is negative (a charge)
	// and a single print of -0.1 bps against a 30 bps round trip is noise, not
	// a reason to pay for the exit and the next entry.
	ExitNegativeFundingRateBps    float64
	ExitNegativeConsecutiveEpochs int

	// Cooldown is how long a pair waits after a close or a failed open before
	// it is scanned for an entry again.
	Cooldown time.Duration

	// ProjectionHoldDays is the hold the entry is priced over when
	// MaxHoldEpochs is 0. An ASSUMPTION, reported beside every figure it
	// produces, exactly as strategy.NetAPRInput.HoldingDays is.
	ProjectionHoldDays float64

	// MaxBasisWidenBps closes the pair when perp-over-spot basis has widened by
	// more than this since the open, in basis points of the spot mid.
	MaxBasisWidenBps float64

	// MinTimeToSettle refuses an entry this close to the next settlement.
	MinTimeToSettle time.Duration

	// DepthMultiple is how many times the notional each of the four sides the
	// round trip takes must hold within ±0.5% of mid.
	DepthMultiple float64

	// MaxConsecutiveFailures halts the pair after this many failed reads in a
	// row, or this many failed trades in a row — two counters, so a clean read
	// between two failed opens does not reset the count of failed opens.
	MaxConsecutiveFailures int
}

// The shipped values of one pair — audited 2026-09-15, revised 2026-09-16 for
// the convergence-and-amortization set of
// docs/AUTOTRADE-CONVERGENCE-HOLDING-STRATEGY.md, and pinned by
// TestDefaults_AreTheAuditedSafetyThresholds. Symbol is filled per pair.
//
// The four thresholds that set the strategy are the testnet column of that
// document's §3. Mainnet is a different column — entry basis +10 bps, take
// profit +0.80% — because mainnet's round trip is 25-35 bps against the
// testnet's ~8 (spot 0, futures taker 4 bps, measured 2026-09-15), and a floor
// calibrated on the cheaper book would enter positions the expensive one cannot
// pay for. Nothing here may reach mainnet (Q14/Q15/Q18), so the shipped values
// are the testnet's and the mainnet column stays a document until 4.6.
const (
	DefaultNotionalQuote          = 65.0
	DefaultMinNetAPRPct           = 5.0
	DefaultMinEntryBasisBps       = 5.0
	DefaultMaxHoldEpochs          = 0
	DefaultMinHoldEpochs          = 6
	DefaultTargetTakeProfitNetPct = 1.50

	// DefaultMaxExitSpreadBps is the take-profit's spread brake. 10 bps is
	// wide for the majors this bot trades — the measured touch is ~1 bps —
	// so it bites only when the makers have really stepped away, and the
	// profit being protected is ~150 bps of notional, which dwarfs it.
	DefaultMaxExitSpreadBps = 10.0
	// The single print this hysteresis refuses to act on: on the 3-year corpus
	// the median negative episode costs 0.3 bps against a 30 bps round trip
	// (CLAUDE.md, step 3.3), so one settlement at -0.1 bps is noise and two in
	// a row at -2.0 bps is a regime.
	DefaultExitNegativeFundingRateBps    = -2.0
	DefaultExitNegativeConsecutiveEpochs = 2
	DefaultCooldown                      = 60 * time.Second
	DefaultProjectionHoldDays            = 30.0
	// DefaultMaxBasisWidenBps is a STRUCTURAL break, not a wide book: the
	// measured basis on a thin testnet moves tens of bps between two scans, and
	// a stop at 30 would fire on that noise and pay a round trip for it.
	DefaultMaxBasisWidenBps = 100.0
	DefaultMinTimeToSettle  = 5 * time.Minute
	DefaultDepthMultiple    = 2.0
	DefaultMaxFailures      = 5
)

// The shipped portfolio values.
const (
	DefaultScanInterval           = 10 * time.Second
	DefaultMaxConcurrentPositions = 50
	// MaxConcurrentPositionsCap is the most pairs a run may hold at once.
	MaxConcurrentPositionsCap = 50
	// DefaultTotalCapitalCapQuote bounds the capital the held pairs tie up, in
	// the quote asset: up to 50 $65 pairs at 50% perp margin tie up $4,875.
	DefaultTotalCapitalCapQuote = 10_000.0
)

// DefaultConfig is the shipped parameter set for one symbol.
func DefaultConfig(symbol string) Config {
	return Config{
		Symbol: symbol, NotionalQuote: DefaultNotionalQuote, MinNetAPRPct: DefaultMinNetAPRPct,
		MinEntryBasisBps: DefaultMinEntryBasisBps,
		MaxHoldEpochs:    DefaultMaxHoldEpochs, MinHoldEpochs: DefaultMinHoldEpochs,
		TargetTakeProfitNetPct:     DefaultTargetTakeProfitNetPct,
		MaxExitSpreadBps:           DefaultMaxExitSpreadBps,
		ExitNegativeFundingRateBps: DefaultExitNegativeFundingRateBps, ExitNegativeConsecutiveEpochs: DefaultExitNegativeConsecutiveEpochs,
		Cooldown:           DefaultCooldown,
		ProjectionHoldDays: DefaultProjectionHoldDays, MaxBasisWidenBps: DefaultMaxBasisWidenBps,
		MinTimeToSettle: DefaultMinTimeToSettle, DepthMultiple: DefaultDepthMultiple,
		MaxConsecutiveFailures: DefaultMaxFailures,
	}
}

// Bounds a start request must respect. They are SAFETY limits on a form, not
// venue facts: the venue's minimums are enforced by execution on the rounded
// size, and the portal caps the notional again.
const (
	minScanInterval  = time.Second
	maxHoldEpochsCap = 1000
	maxAPRPctBound   = 1000.0
	// maxBasisBpsBound is 100% of the spot mid: past it the two "prices" are
	// not quotes on the same coin and the figure is a decoding fault, not a
	// market.
	maxBasisBpsBound = 10_000.0
	// maxTakeProfitPctBound is on the CAPITAL a pair ties up. A take-profit
	// above it would never fire on a delta-neutral pair, which is a typo
	// silently disabling the rule rather than a choice.
	maxTakeProfitPctBound = 100.0
)

// errConfig marks a refused configuration.
var errConfig = errors.New("autotrade: cấu hình bị từ chối")

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// problems lists why this pair configuration may not run; maxNotionalQuote is
// the portal's own ceiling per leg.
func (c Config) problems(maxNotionalQuote float64) []string {
	var out []string
	if strings.TrimSpace(c.Symbol) == "" {
		out = append(out, "không có symbol")
	}
	if !finite(c.NotionalQuote) || c.NotionalQuote <= 0 || c.NotionalQuote > maxNotionalQuote {
		out = append(out, fmt.Sprintf("notional_quote %v phải trong (0, %.0f]", c.NotionalQuote, maxNotionalQuote))
	}
	if !finite(c.MinNetAPRPct) || math.Abs(c.MinNetAPRPct) > maxAPRPctBound {
		out = append(out, fmt.Sprintf("min_net_apr_pct %v phải là số hữu hạn trong ±%.0f", c.MinNetAPRPct, maxAPRPctBound))
	}
	if !finite(c.MinEntryBasisBps) || math.Abs(c.MinEntryBasisBps) > maxBasisBpsBound {
		out = append(out, fmt.Sprintf("min_entry_basis_bps %v phải là số hữu hạn trong ±%.0f", c.MinEntryBasisBps, maxBasisBpsBound))
	}
	if c.MaxHoldEpochs < 0 || c.MaxHoldEpochs > maxHoldEpochsCap {
		out = append(out, fmt.Sprintf("max_hold_epochs %d phải trong [0, %d] (0 = giữ khi funding còn dương)", c.MaxHoldEpochs, maxHoldEpochsCap))
	}
	if c.MinHoldEpochs < 0 || c.MinHoldEpochs > maxHoldEpochsCap {
		out = append(out, fmt.Sprintf("min_hold_epochs %d phải trong [0, %d] (0 = thoát ngay ở mốc settle ≤ 0 đầu tiên)", c.MinHoldEpochs, maxHoldEpochsCap))
	}
	// A floor at or above the ceiling is a pair that can never take the funding
	// exit: it would hold to MaxHoldEpochs whatever funding did. Refused rather
	// than run, because the two numbers read as if they cooperate.
	if c.MaxHoldEpochs > 0 && c.MinHoldEpochs >= c.MaxHoldEpochs {
		out = append(out, fmt.Sprintf("sàn giữ %d mốc ≥ trần giữ %d mốc — lối thoát funding không bao giờ chạy được", c.MinHoldEpochs, c.MaxHoldEpochs))
	}
	if !finite(c.TargetTakeProfitNetPct) || c.TargetTakeProfitNetPct < 0 || c.TargetTakeProfitNetPct > maxTakeProfitPctBound {
		out = append(out, fmt.Sprintf("target_take_profit_net_pct %v phải trong [0, %.0f] phần trăm trên vốn (0 = tắt chốt lời sớm)", c.TargetTakeProfitNetPct, maxTakeProfitPctBound))
	}
	if !finite(c.MaxExitSpreadBps) || c.MaxExitSpreadBps < 0 || c.MaxExitSpreadBps > maxBasisBpsBound {
		out = append(out, fmt.Sprintf("max_exit_spread_bps %v phải trong [0, %.0f] bps (0 = tắt van chặn spread khi chốt lời)", c.MaxExitSpreadBps, maxBasisBpsBound))
	}
	// A positive threshold would read "thoát khi funding dương", which is the
	// opposite of what this exit is for.
	if !finite(c.ExitNegativeFundingRateBps) || c.ExitNegativeFundingRateBps > 0 || c.ExitNegativeFundingRateBps < -maxBasisBpsBound {
		out = append(out, fmt.Sprintf("exit_negative_funding_rate_bps %v phải là số hữu hạn trong [-%.0f, 0] (ngưỡng rate ÂM)", c.ExitNegativeFundingRateBps, maxBasisBpsBound))
	}
	if c.ExitNegativeConsecutiveEpochs < 1 || c.ExitNegativeConsecutiveEpochs > maxHoldEpochsCap {
		out = append(out, fmt.Sprintf("exit_negative_consecutive_epochs %d phải trong [1, %d]", c.ExitNegativeConsecutiveEpochs, maxHoldEpochsCap))
	}
	if c.Cooldown < 0 {
		out = append(out, "thời gian hồi phục âm")
	}
	if c.MaxHoldEpochs == 0 && (!finite(c.ProjectionHoldDays) || c.ProjectionHoldDays <= 0) {
		out = append(out, fmt.Sprintf("giữ khi funding dương cần số ngày dự phóng dương, nhận %v", c.ProjectionHoldDays))
	}
	if !finite(c.MaxBasisWidenBps) || c.MaxBasisWidenBps <= 0 {
		out = append(out, fmt.Sprintf("ngưỡng basis giãn %v bps phải dương", c.MaxBasisWidenBps))
	}
	if c.MinTimeToSettle < 0 {
		out = append(out, "khoảng cách tối thiểu tới mốc settle âm")
	}
	if !finite(c.DepthMultiple) || c.DepthMultiple < 1 {
		out = append(out, fmt.Sprintf("bội số độ sâu %v phải ≥ 1", c.DepthMultiple))
	}
	if c.MaxConsecutiveFailures < 1 {
		out = append(out, "số lỗi liên tiếp tối đa phải ≥ 1")
	}
	return out
}

// Validate refuses a pair configuration no run may start with.
func (c Config) Validate(maxNotionalQuote float64) error {
	if p := c.problems(maxNotionalQuote); len(p) > 0 {
		return fmt.Errorf("%w: %s", errConfig, strings.Join(p, "; "))
	}
	return nil
}

// PortfolioConfig is one run's parameters: which pairs it may enter, how many it
// may hold, how much capital they may tie up, and each pair's own Config.
type PortfolioConfig struct {
	// Symbols are the pairs the run may ENTER. A bot position found on any other
	// symbol the portal trades is still adopted and managed to its exit — a run
	// never leaves one of its own positions unwatched — but never re-entered.
	Symbols []string

	// MaxConcurrentPositions is how many pairs may be held at once.
	MaxConcurrentPositions int

	// TotalCapitalCapQuote bounds the capital the held pairs tie up: for each,
	// its notional times the capital per notional (spot in full plus the perp
	// margin), in the quote asset.
	TotalCapitalCapQuote float64

	ScanInterval time.Duration

	// AutoRebalance re-sizes DefaultPairConfig.NotionalQuote from the account's
	// own equity every RebalanceIntervalHours (capital.go). It moves the size of
	// FUTURE opens only: a rebalance never closes, shrinks or re-prices a
	// position already on the venue, which would pay a round trip to change a
	// number. Off leaves the notional exactly as the operator typed it.
	AutoRebalance bool
	// MarginBufferPct is the share of equity held back from the slots, as a
	// fraction in [MinMarginBufferPct, MaxMarginBufferPct].
	MarginBufferPct float64
	// RebalanceIntervalHours is how long a size stands before it is re-read.
	RebalanceIntervalHours float64
	// LastRebalancedAtMs is when the size was last set from the account, on the
	// engine's own clock; 0 means never, and the first scan of a run with
	// AutoRebalance on then sizes immediately.
	LastRebalancedAtMs int64

	// DefaultPairConfig is every pair's Config unless PairOverrides names it.
	// Its Symbol is ignored.
	DefaultPairConfig Config
	// PairOverrides replaces the default for the symbols it names, whole.
	//
	// A pair with its own Config is NOT re-sized by a rebalance: naming a size
	// for one pair is an instruction, and an automatic rule may not overwrite
	// one. The console says so at every rebalance that skips one.
	PairOverrides map[string]Config
}

// DefaultPortfolioConfig is the shipped run over the given symbols.
func DefaultPortfolioConfig(symbols []string) PortfolioConfig {
	maxConcurrent := len(symbols)
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	if maxConcurrent > MaxConcurrentPositionsCap {
		maxConcurrent = MaxConcurrentPositionsCap
	}
	return PortfolioConfig{
		Symbols:                append([]string(nil), symbols...),
		MaxConcurrentPositions: maxConcurrent,
		TotalCapitalCapQuote:   DefaultTotalCapitalCapQuote,
		ScanInterval:           DefaultScanInterval,
		AutoRebalance:          DefaultAutoRebalance,
		MarginBufferPct:        DefaultMarginBufferPct,
		RebalanceIntervalHours: DefaultRebalanceIntervalHours,
		DefaultPairConfig:      DefaultConfig(""),
	}
}

// PairConfig is the Config one symbol runs with.
func (pc PortfolioConfig) PairConfig(symbol string) Config {
	c, ok := pc.PairOverrides[symbol]
	if !ok {
		c = pc.DefaultPairConfig
	}
	c.Symbol = symbol
	return c
}

// Validate refuses a run no start may begin. allowed is the portal's allow-list;
// capitalPerNotional is the capital one quote of notional ties up.
func (pc PortfolioConfig) Validate(allowed []string, maxNotionalQuote, capitalPerNotional float64) error {
	var problems []string
	isAllowed := map[string]bool{}
	for _, s := range allowed {
		isAllowed[s] = true
	}
	inRun := map[string]bool{}
	if len(pc.Symbols) == 0 {
		problems = append(problems, "không chọn cặp nào")
	}
	for _, s := range pc.Symbols {
		switch {
		case !isAllowed[s]:
			problems = append(problems, fmt.Sprintf("%q không nằm trong danh sách portal được giao dịch %v", s, allowed))
		case inRun[s]:
			problems = append(problems, fmt.Sprintf("%s được chọn hai lần", s))
		}
		inRun[s] = true
	}
	if pc.MaxConcurrentPositions < 1 || pc.MaxConcurrentPositions > MaxConcurrentPositionsCap {
		problems = append(problems, fmt.Sprintf("max_concurrent_positions %d phải trong [1, %d]", pc.MaxConcurrentPositions, MaxConcurrentPositionsCap))
	}
	if pc.ScanInterval < minScanInterval {
		problems = append(problems, fmt.Sprintf("chu kỳ quét %s ngắn hơn %s", pc.ScanInterval, minScanInterval))
	}
	// The two rebalance values are validated whether or not it is switched on:
	// a run started with the switch off and a nonsense buffer would size
	// wrongly the moment somebody turns it on.
	if !finite(pc.MarginBufferPct) || pc.MarginBufferPct < MinMarginBufferPct || pc.MarginBufferPct > MaxMarginBufferPct {
		problems = append(problems, fmt.Sprintf("margin_buffer_pct %v phải trong [%.2f, %.2f] (phần vốn giữ lại làm đệm ký quỹ)", pc.MarginBufferPct, MinMarginBufferPct, MaxMarginBufferPct))
	}
	if !finite(pc.RebalanceIntervalHours) || pc.RebalanceIntervalHours < MinRebalanceIntervalHours || pc.RebalanceIntervalHours > MaxRebalanceIntervalHours {
		problems = append(problems, fmt.Sprintf("rebalance_interval_hours %v phải trong [%.0f, %.0f]", pc.RebalanceIntervalHours, MinRebalanceIntervalHours, MaxRebalanceIntervalHours))
	}
	if pc.LastRebalancedAtMs < 0 {
		problems = append(problems, fmt.Sprintf("last_rebalanced_at_ms %d âm", pc.LastRebalancedAtMs))
	}
	capOK := finite(pc.TotalCapitalCapQuote) && pc.TotalCapitalCapQuote > 0
	if !capOK {
		problems = append(problems, fmt.Sprintf("total_capital_cap_quote %v phải là số dương hữu hạn", pc.TotalCapitalCapQuote))
	}
	// A cap above what the most pairs at the largest size could ever tie up
	// bounds nothing, and a typo of three zeros should be refused, not run.
	if ceiling := float64(MaxConcurrentPositionsCap) * maxNotionalQuote * capitalPerNotional; capOK && finite(ceiling) && pc.TotalCapitalCapQuote > ceiling {
		problems = append(problems, fmt.Sprintf("total_capital_cap_quote %.2f vượt trần %.2f (%d cặp × %.0f notional × %.2f vốn)", pc.TotalCapitalCapQuote, ceiling, MaxConcurrentPositionsCap, maxNotionalQuote, capitalPerNotional))
	}
	if !finite(capitalPerNotional) || capitalPerNotional < 1 {
		problems = append(problems, fmt.Sprintf("vốn trên mỗi đồng notional %v phải ≥ 1 (spot trọn notional)", capitalPerNotional))
		capOK = false
	}
	overrideKeys := make([]string, 0, len(pc.PairOverrides))
	for s := range pc.PairOverrides {
		overrideKeys = append(overrideKeys, s)
	}
	sort.Strings(overrideKeys)
	for _, s := range overrideKeys {
		if !inRun[s] {
			problems = append(problems, fmt.Sprintf("cấu hình riêng cho %s nhưng %s không được chọn", s, s))
		}
		if o := pc.PairOverrides[s]; o.Symbol != "" && o.Symbol != s {
			problems = append(problems, fmt.Sprintf("cấu hình riêng dưới khoá %s mang symbol %s", s, o.Symbol))
		}
	}
	for _, s := range pc.Symbols {
		c := pc.PairConfig(s)
		for _, p := range c.problems(maxNotionalQuote) {
			problems = append(problems, s+": "+p)
		}
		// A pair whose single position already exceeds the cap could never be
		// entered; a run that silently never trades it is refused instead.
		if capOK && finite(c.NotionalQuote) && c.NotionalQuote*capitalPerNotional > pc.TotalCapitalCapQuote {
			problems = append(problems, fmt.Sprintf("%s: một vị thế %.2f quote notional buộc %.2f quote vốn, vượt hạn mức vốn %.2f",
				s, c.NotionalQuote, c.NotionalQuote*capitalPerNotional, pc.TotalCapitalCapQuote))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", errConfig, strings.Join(problems, "; "))
	}
	return nil
}

// ConfigView is one pair's Config on the wire.
type ConfigView struct {
	Symbol                        string  `json:"symbol"`
	NotionalQuote                 float64 `json:"notional_quote"`
	MinNetAPRPct                  float64 `json:"min_net_apr_pct"`
	MinEntryBasisBps              float64 `json:"min_entry_basis_bps"`
	MaxHoldEpochs                 int     `json:"max_hold_epochs"`
	MinHoldEpochs                 int     `json:"min_hold_epochs"`
	TargetTakeProfitNetPct        float64 `json:"target_take_profit_net_pct"`
	MaxExitSpreadBps              float64 `json:"max_exit_spread_bps"`
	ExitNegativeFundingRateBps    float64 `json:"exit_negative_funding_rate_bps"`
	ExitNegativeConsecutiveEpochs int     `json:"exit_negative_consecutive_epochs"`
	CooldownSec                   float64 `json:"cooldown_sec"`
	ProjectionHoldDays            float64 `json:"projection_hold_days"`
	MaxBasisWidenBps              float64 `json:"max_basis_widen_bps"`
	MinTimeToSettleSec            float64 `json:"min_time_to_settle_sec"`
	DepthMultiple                 float64 `json:"depth_multiple"`
	MaxConsecutiveFailures        int     `json:"max_consecutive_failures"`
}

func (c Config) view() ConfigView {
	return ConfigView{
		Symbol: c.Symbol, NotionalQuote: c.NotionalQuote, MinNetAPRPct: c.MinNetAPRPct,
		MinEntryBasisBps: c.MinEntryBasisBps, MaxHoldEpochs: c.MaxHoldEpochs, MinHoldEpochs: c.MinHoldEpochs,
		TargetTakeProfitNetPct:     c.TargetTakeProfitNetPct,
		MaxExitSpreadBps:           c.MaxExitSpreadBps,
		ExitNegativeFundingRateBps: c.ExitNegativeFundingRateBps, ExitNegativeConsecutiveEpochs: c.ExitNegativeConsecutiveEpochs,
		CooldownSec: c.Cooldown.Seconds(), ProjectionHoldDays: c.ProjectionHoldDays,
		MaxBasisWidenBps: c.MaxBasisWidenBps, MinTimeToSettleSec: c.MinTimeToSettle.Seconds(),
		DepthMultiple: c.DepthMultiple, MaxConsecutiveFailures: c.MaxConsecutiveFailures,
	}
}

// PortfolioView is PortfolioConfig on the wire.
type PortfolioView struct {
	Symbols                []string              `json:"symbols"`
	MaxConcurrentPositions int                   `json:"max_concurrent_positions"`
	TotalCapitalCapQuote   float64               `json:"total_capital_cap_quote"`
	ScanIntervalSec        float64               `json:"scan_interval_sec"`
	AutoRebalance          bool                  `json:"auto_rebalance"`
	MarginBufferPct        float64               `json:"margin_buffer_pct"`
	RebalanceIntervalHours float64               `json:"rebalance_interval_hours"`
	LastRebalancedAtMs     int64                 `json:"last_rebalanced_at_ms"`
	NextRebalanceAtMs      int64                 `json:"next_rebalance_at_ms"`
	DefaultPairConfig      ConfigView            `json:"default_pair_config"`
	PairOverrides          map[string]ConfigView `json:"pair_overrides"`
}

func (pc PortfolioConfig) view() PortfolioView {
	v := PortfolioView{
		Symbols: append([]string{}, pc.Symbols...), MaxConcurrentPositions: pc.MaxConcurrentPositions,
		TotalCapitalCapQuote: pc.TotalCapitalCapQuote, ScanIntervalSec: pc.ScanInterval.Seconds(),
		AutoRebalance: pc.AutoRebalance, MarginBufferPct: pc.MarginBufferPct,
		RebalanceIntervalHours: pc.RebalanceIntervalHours, LastRebalancedAtMs: pc.LastRebalancedAtMs,
		NextRebalanceAtMs: pc.nextRebalanceAtMs(),
		DefaultPairConfig: pc.DefaultPairConfig.view(), PairOverrides: map[string]ConfigView{},
	}
	for s, c := range pc.PairOverrides {
		c.Symbol = s
		v.PairOverrides[s] = c.view()
	}
	return v
}

// nextRebalanceAtMs is when the size is next read from the account; 0 when
// auto-rebalance is off or nothing has been sized yet (the next scan does it).
func (pc PortfolioConfig) nextRebalanceAtMs() int64 {
	if !pc.AutoRebalance || pc.LastRebalancedAtMs <= 0 || !finite(pc.RebalanceIntervalHours) {
		return 0
	}
	return pc.LastRebalancedAtMs + int64(pc.RebalanceIntervalHours*float64(time.Hour/time.Millisecond))
}

// rebalanceDue reports whether a size read is owed at now. A run that has never
// sized is due at once, so a bot switched on with an empty form does not trade
// a week at the shipped 65 before its first look at the account.
func (pc PortfolioConfig) rebalanceDue(nowMs int64) bool {
	switch {
	case !pc.AutoRebalance:
		return false
	case pc.LastRebalancedAtMs <= 0:
		return true
	default:
		next := pc.nextRebalanceAtMs()
		return next > 0 && nowMs >= next
	}
}

// LogEntry is one line of the bot's own console. Symbol is empty for a line
// about the whole portfolio.
type LogEntry struct {
	AtMs      int64  `json:"at_ms"`
	Kind      string `json:"kind"`
	Symbol    string `json:"symbol"`
	MessageVI string `json:"message_vi"`
}

// logCapacity is how many lines the ring keeps — enough for a few scans of
// several pairs, since each pair logs a verdict only when it changes.
const logCapacity = 60

// ringLog keeps the newest logCapacity entries. Not safe on its own; the engine
// holds its lock around it.
type ringLog struct {
	entries [logCapacity]LogEntry
	next    int
	size    int
}

func (r *ringLog) add(e LogEntry) {
	r.entries[r.next] = e
	r.next = (r.next + 1) % logCapacity
	if r.size < logCapacity {
		r.size++
	}
}

// newestFirst copies the entries out, newest first.
func (r *ringLog) newestFirst() []LogEntry {
	out := make([]LogEntry, 0, r.size)
	for i := 1; i <= r.size; i++ {
		out = append(out, r.entries[(r.next-i+logCapacity)%logCapacity])
	}
	return out
}

// PositionView is a pair the bot opened (or adopted) and is managing.
type PositionView struct {
	Symbol     string  `json:"symbol"`
	IntentID   string  `json:"intent_id"`
	OpenedAtMs int64   `json:"opened_at_ms"`
	QtyCoin    float64 `json:"qty_coin"`
	// NotionalQuote is the notional the open was asked for, and CapitalQuote
	// the capital it ties up: NotionalQuote × capital per notional.
	NotionalQuote float64 `json:"notional_quote"`
	CapitalQuote  float64 `json:"capital_quote"`
	EntryBasisBps float64 `json:"entry_basis_bps"`
	// The two legs' average ENTRY fill prices, from the open's result or — for
	// an adopted pair — from the intent's cache file; 0 when unknown.
	SpotEntryAvgQuote float64 `json:"spot_entry_avg_quote"`
	PerpEntryAvgQuote float64 `json:"perp_entry_avg_quote"`
	// Adopted is a position this run found already open under an autotrade
	// intent id rather than opened itself — a previous run, a restart.
	Adopted bool `json:"adopted"`
	// SettlementsSinceOpen is how many settlements the venue has listed after
	// the open, as of the last scan (rule 6: counted, not derived).
	SettlementsSinceOpen int `json:"settlements_since_open"`

	// The pair MARKED TO MID at the pair's last scan: each leg's price move
	// from its entry fill to the mid the scan read, times the quantity. GROSS —
	// nothing of the exit's commission or slippage is taken off, and no funding
	// is in it (the portal reads that from the venue). nil when either entry
	// price or either mid is unknown; MarkedAtMs is the scan the mids are from.
	SpotLegDriftQuote *float64 `json:"spot_leg_drift_quote"`
	PerpLegDriftQuote *float64 `json:"perp_leg_drift_quote"`
	PairDriftQuote    *float64 `json:"pair_drift_quote"`
	MarkedAtMs        int64    `json:"marked_at_ms"`
	DriftLabelVI      string   `json:"drift_label_vi"`
}

// driftLabelVI travels with every marked-to-mid figure.
const driftLabelVI = "Tạm tính theo GIÁ GIỮA của lượt quét gần nhất: (mid spot − giá khớp vào spot) × khối lượng + (giá khớp vào perp − mid perp) × khối lượng. " +
	"CHƯA trừ phí và trượt giá khi đóng, KHÔNG gồm funding. Không phải lãi ròng."

// Radar is what the opportunity grid says about one pair, as a value.
type Radar string

const (
	RadarOff      Radar = "off"      // not selected in this run
	RadarPaused   Radar = "paused"   // selected, entries paused
	RadarScanning Radar = "scanning" // scanned, not eligible (or not scanned yet)
	RadarEligible Radar = "eligible" // every entry check passed
	RadarSkipped  Radar = "skipped"  // eligible, but a portfolio limit stood in the way
	RadarOpening  Radar = "opening"
	RadarHolding  Radar = "holding"
	RadarCooldown Radar = "cooldown"
	RadarHalted   Radar = "halted"
	// RadarUnproven is a pair the venue has not proven flat — a failed or
	// undecided read, legs the bot does not manage. It takes a place until a
	// reading proves it flat.
	RadarUnproven Radar = "unproven"
)

var radarVI = map[Radar]string{
	RadarOff:      "KHÔNG THAM GIA",
	RadarPaused:   "TẠM DỪNG",
	RadarScanning: "ĐANG QUÉT",
	RadarEligible: "ĐỦ ĐIỀU KIỆN",
	RadarSkipped:  "BỎ QUA",
	RadarOpening:  "ĐANG MỞ",
	RadarHolding:  "ĐANG GIỮ",
	RadarCooldown: "HỒI PHỤC",
	RadarHalted:   "DỪNG BẢO VỆ",
	RadarUnproven: "CHƯA PHẲNG",
}

// PairView is everything the page shows about one pair.
type PairView struct {
	Symbol       string     `json:"symbol"`
	State        State      `json:"state"`
	StateVI      string     `json:"state_vi"`
	StateSinceMs int64      `json:"state_since_ms"`
	InRun        bool       `json:"in_run"`
	Paused       bool       `json:"paused"`
	Overridden   bool       `json:"overridden"`
	Config       ConfigView `json:"config"`

	Radar   Radar  `json:"radar"`
	RadarVI string `json:"radar_vi"`
	// Rank is the pair's place among the eligible pairs of the last scan, by
	// Net APR; 0 when it was not eligible.
	Rank   int    `json:"rank"`
	SkipVI string `json:"skip_vi"`
	// SlotUsed is the pair taking a place in the portfolio's two counts, and
	// SlotReasonVI why: held, trading, halted with legs possibly on the
	// venue, or not proven flat.
	SlotUsed     bool   `json:"slot_used"`
	SlotReasonVI string `json:"slot_reason_vi"`

	// The venue's hedge verdict for the symbol at the pair's last read.
	HedgeStatus        HedgeStatus `json:"hedge_status"`
	HedgeReasonVI      string      `json:"hedge_reason_vi"`
	ResidualQtyCoin    float64     `json:"residual_qty_coin"`
	ToleranceQtyCoin   float64     `json:"tolerance_qty_coin"`
	HoldingReadAtMs    int64       `json:"holding_read_at_ms"`
	CooldownUntilMs    int64       `json:"cooldown_until_ms"`
	ReadFailures       int         `json:"read_failures"`
	TradeFailures      int         `json:"trade_failures"`
	HaltReasonVI       string      `json:"halt_reason_vi"`
	HaltSeq            int         `json:"halt_seq"`
	LastScanAtMs       int64       `json:"last_scan_at_ms"`
	CapitalPerNotional float64     `json:"capital_per_notional"`

	Signal   *SignalView   `json:"signal"`
	Position *PositionView `json:"position"`
}

// StatusView is everything the page shows about the bot.
type StatusView struct {
	State        State         `json:"state"`
	StateVI      string        `json:"state_vi"`
	StateSinceMs int64         `json:"state_since_ms"`
	Enabled      bool          `json:"enabled"`
	Portfolio    PortfolioView `json:"portfolio"`
	NowMs        int64         `json:"now_ms"`

	LastScanAtMs int64 `json:"last_scan_at_ms"`
	NextScanAtMs int64 `json:"next_scan_at_ms"`

	// Pairs lists every symbol the portal trades, in the portal's order.
	Pairs []PairView `json:"pairs"`
	// Positions are the pairs held, in the same order — the same objects as
	// in Pairs, repeated for a page that only needs the matrix.
	Positions []PositionView `json:"positions"`

	// SlotsUsed counts every pair taking a place: held, trading, halted with
	// legs possibly on the venue, and — while running — not proven flat by its
	// newest reading. CapitalCommittedQuote is the same count in capital.
	SlotsUsed             int     `json:"slots_used"`
	OpenPositions         int     `json:"open_positions"`
	NotionalDeployedQuote float64 `json:"notional_deployed_quote"`
	CapitalDeployedQuote  float64 `json:"capital_deployed_quote"`
	CapitalCommittedQuote float64 `json:"capital_committed_quote"`
	// UnsizedPairs are the pairs taking a place whose capital no reading states:
	// CapitalCommittedQuote counts each at a stand-in — the bot's own position
	// for a held pair, the configured notional otherwise — so the committed
	// figure is a floor while this is not empty, and no entry opens. Nothing is
	// read while the bot is off, so the list may name the last reading until
	// the next start.
	UnsizedPairs           []string `json:"unsized_pairs"`
	CapitalPerNotional     float64  `json:"capital_per_notional"`
	UnrealizedDriftQuote   float64  `json:"unrealized_drift_quote"`
	UnrealizedDriftPriced  int      `json:"unrealized_drift_priced"`
	UnrealizedDriftLabelVI string   `json:"unrealized_drift_label_vi"`

	HaltReasonVI string `json:"halt_reason_vi"`
	// HaltSeq counts every halt, of a pair or of the bot. A stop acknowledges
	// only the halts that existed when it was pressed.
	HaltSeq     int `json:"halt_seq"`
	HaltedPairs int `json:"halted_pairs"`
	// Busy names the operator action the bot is running right now — "stop",
	// "kill", "close_pair" — so the page does not offer a second one.
	Busy string `json:"busy"`

	Log      []LogEntry `json:"log"`
	NoticeVI string     `json:"notice_vi"`
}

// noticeVI travels with every status.
const noticeVI = "CHỈ TESTNET — không tiền thật (PLAN Q18). Bot tự đặt lệnh qua đúng đường MỞ/ĐÓNG của portal, mỗi lúc MỘT lệnh; " +
	"mọi con số là của tài khoản testnet (funding, sổ lệnh, phí), không phải của mainnet. " +
	"Bot không bao giờ tự làm phẳng: một cặp lệch hay bằng chứng không khớp là DỪNG BẢO VỆ cặp đó, các cặp khác chạy tiếp."
