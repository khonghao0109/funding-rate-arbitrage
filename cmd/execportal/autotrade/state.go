// Package autotrade is the Binance-TESTNET auto-trader of cmd/execportal
// (PLAN §7.1 Q18): a loop that reads both testnet markets, decides whether to
// hold Strategy 1's pair, and asks the portal to open or close it.
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
//   - Its signal is the TESTNET's own funding, books and account fees, read
//     through the Market it is handed. Not cmd/scanner, not the step-3.5
//     journal, not strategy.EvaluateEntry: the gate's decisions stay the gate's.
//     What it borrows from internal/strategy is the ARITHMETIC — RoundTripCost
//     and NetAPR, the only functions in the repository allowed to say "net".
//   - ONE position, on one symbol, and the invariant is execution's: both legs
//     open within the coarser step, or both flat. A hedge status that is
//     neither — unhedged, two proofs that disagree, a read that could not decide
//     — halts the bot. It never squares anything itself: that is LÀM PHẲNG,
//     pressed by a person (the never-auto-reconcile rule).
//
// # The numbers it acts on (CLAUDE.md rules 2, 3, 6)
//
// Entry is priced by strategy.NetAPR on the mean of the SETTLED rates of the
// last seven days, at the cadence measured from their stamps, over the hold the
// bot actually plans — MaxHoldEpochs settlements, or ProjectionHoldDays when it
// holds while funding stays positive — with a round trip of taker fees read
// from THIS ACCOUNT and slippage estimated from the two books just read. The
// forming rate (premiumIndex lastFundingRate) is shown and must be positive, but
// nothing is projected on it. Exits count settlements the venue lists after the
// open and read the rate each one settled at; nothing multiplies an APR by a
// duration.
//
// On a testnet those inputs are the testnet's: its funding, its thin books and
// its fees (spot 0, futures taker 4 bps, measured 2026-09-15). A figure here is
// a statement about the testnet account, never about what mainnet would pay.
package autotrade

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// State is the machine's state, as a value.
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
}

// Config is one run's parameters. Durations carry their unit in the type;
// every other figure names it (rule 4).
type Config struct {
	Symbol        string
	NotionalQuote float64

	// MinNetAPRPct is the entry floor on strategy.NetAPR, in PERCENT a year on
	// ONE LEG's notional.
	MinNetAPRPct float64

	// MaxHoldEpochs closes the pair once this many settlements have been
	// listed by the venue after the open. 0 holds for as long as each
	// settlement pays — the operator's choice of 2026-09-15 — and projects the
	// entry over ProjectionHoldDays instead.
	MaxHoldEpochs int

	ScanInterval time.Duration
	Cooldown     time.Duration

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

	// MaxConsecutiveFailures halts the bot after this many failed reads in a
	// row, or this many failed trades in a row — two counters, so a clean read
	// between two failed opens does not reset the count of failed opens.
	MaxConsecutiveFailures int
}

// The shipped values. Symbol is filled by the portal from its own allow-list.
const (
	DefaultNotionalQuote      = 65.0
	DefaultMinNetAPRPct       = 5.0
	DefaultMaxHoldEpochs      = 0
	DefaultScanInterval       = 5 * time.Second
	DefaultCooldown           = 60 * time.Second
	DefaultProjectionHoldDays = 30.0
	DefaultMaxBasisWidenBps   = 30.0
	DefaultMinTimeToSettle    = 5 * time.Minute
	DefaultDepthMultiple      = 2.0
	DefaultMaxFailures        = 3
)

// DefaultConfig is the shipped parameter set for one symbol.
func DefaultConfig(symbol string) Config {
	return Config{
		Symbol: symbol, NotionalQuote: DefaultNotionalQuote, MinNetAPRPct: DefaultMinNetAPRPct,
		MaxHoldEpochs: DefaultMaxHoldEpochs, ScanInterval: DefaultScanInterval, Cooldown: DefaultCooldown,
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
)

// errConfig marks a refused configuration.
var errConfig = errors.New("autotrade: cấu hình bị từ chối")

// Validate refuses a configuration no run may start with. maxNotionalQuote is
// the portal's own ceiling per leg.
func (c Config) Validate(maxNotionalQuote float64) error {
	var problems []string
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	if strings.TrimSpace(c.Symbol) == "" {
		problems = append(problems, "không có symbol")
	}
	if !finite(c.NotionalQuote) || c.NotionalQuote <= 0 || c.NotionalQuote > maxNotionalQuote {
		problems = append(problems, fmt.Sprintf("notional_quote %v phải trong (0, %.0f]", c.NotionalQuote, maxNotionalQuote))
	}
	if !finite(c.MinNetAPRPct) || math.Abs(c.MinNetAPRPct) > maxAPRPctBound {
		problems = append(problems, fmt.Sprintf("min_net_apr_pct %v phải là số hữu hạn trong ±%.0f", c.MinNetAPRPct, maxAPRPctBound))
	}
	if c.MaxHoldEpochs < 0 || c.MaxHoldEpochs > maxHoldEpochsCap {
		problems = append(problems, fmt.Sprintf("max_hold_epochs %d phải trong [0, %d] (0 = giữ khi funding còn dương)", c.MaxHoldEpochs, maxHoldEpochsCap))
	}
	if c.ScanInterval < minScanInterval {
		problems = append(problems, fmt.Sprintf("chu kỳ quét %s ngắn hơn %s", c.ScanInterval, minScanInterval))
	}
	if c.Cooldown < 0 {
		problems = append(problems, "thời gian hồi phục âm")
	}
	if c.MaxHoldEpochs == 0 && (!finite(c.ProjectionHoldDays) || c.ProjectionHoldDays <= 0) {
		problems = append(problems, fmt.Sprintf("giữ khi funding dương cần số ngày dự phóng dương, nhận %v", c.ProjectionHoldDays))
	}
	if !finite(c.MaxBasisWidenBps) || c.MaxBasisWidenBps <= 0 {
		problems = append(problems, fmt.Sprintf("ngưỡng basis giãn %v bps phải dương", c.MaxBasisWidenBps))
	}
	if c.MinTimeToSettle < 0 {
		problems = append(problems, "khoảng cách tối thiểu tới mốc settle âm")
	}
	if !finite(c.DepthMultiple) || c.DepthMultiple < 1 {
		problems = append(problems, fmt.Sprintf("bội số độ sâu %v phải ≥ 1", c.DepthMultiple))
	}
	if c.MaxConsecutiveFailures < 1 {
		problems = append(problems, "số lỗi liên tiếp tối đa phải ≥ 1")
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", errConfig, strings.Join(problems, "; "))
	}
	return nil
}

// ConfigView is Config on the wire.
type ConfigView struct {
	Symbol                 string  `json:"symbol"`
	NotionalQuote          float64 `json:"notional_quote"`
	MinNetAPRPct           float64 `json:"min_net_apr_pct"`
	MaxHoldEpochs          int     `json:"max_hold_epochs"`
	ScanIntervalSec        float64 `json:"scan_interval_sec"`
	CooldownSec            float64 `json:"cooldown_sec"`
	ProjectionHoldDays     float64 `json:"projection_hold_days"`
	MaxBasisWidenBps       float64 `json:"max_basis_widen_bps"`
	MinTimeToSettleSec     float64 `json:"min_time_to_settle_sec"`
	DepthMultiple          float64 `json:"depth_multiple"`
	MaxConsecutiveFailures int     `json:"max_consecutive_failures"`
}

func (c Config) view() ConfigView {
	return ConfigView{
		Symbol: c.Symbol, NotionalQuote: c.NotionalQuote, MinNetAPRPct: c.MinNetAPRPct, MaxHoldEpochs: c.MaxHoldEpochs,
		ScanIntervalSec: c.ScanInterval.Seconds(), CooldownSec: c.Cooldown.Seconds(), ProjectionHoldDays: c.ProjectionHoldDays,
		MaxBasisWidenBps: c.MaxBasisWidenBps, MinTimeToSettleSec: c.MinTimeToSettle.Seconds(),
		DepthMultiple: c.DepthMultiple, MaxConsecutiveFailures: c.MaxConsecutiveFailures,
	}
}

// LogEntry is one line of the bot's own console.
type LogEntry struct {
	AtMs      int64  `json:"at_ms"`
	Kind      string `json:"kind"`
	MessageVI string `json:"message_vi"`
}

// logCapacity is how many lines the ring keeps.
const logCapacity = 20

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

// PositionView is the pair the bot opened (or adopted) and is managing.
type PositionView struct {
	IntentID      string  `json:"intent_id"`
	OpenedAtMs    int64   `json:"opened_at_ms"`
	QtyCoin       float64 `json:"qty_coin"`
	EntryBasisBps float64 `json:"entry_basis_bps"`
	// Adopted is a position this run found already open under an autotrade
	// intent id rather than opened itself — a previous run, a restart.
	Adopted bool `json:"adopted"`
	// SettlementsSinceOpen is how many settlements the venue has listed after
	// the open, as of the last scan (rule 6: counted, not derived).
	SettlementsSinceOpen int `json:"settlements_since_open"`
}

// StatusView is everything the page shows about the bot.
type StatusView struct {
	State        State      `json:"state"`
	StateVI      string     `json:"state_vi"`
	StateSinceMs int64      `json:"state_since_ms"`
	Enabled      bool       `json:"enabled"`
	Config       ConfigView `json:"config"`
	NowMs        int64      `json:"now_ms"`

	LastScanAtMs    int64 `json:"last_scan_at_ms"`
	NextScanAtMs    int64 `json:"next_scan_at_ms"`
	CooldownUntilMs int64 `json:"cooldown_until_ms"`

	Signal   *SignalView   `json:"signal"`
	Position *PositionView `json:"position"`

	ReadFailures  int    `json:"read_failures"`
	TradeFailures int    `json:"trade_failures"`
	HaltReasonVI  string `json:"halt_reason_vi"`
	// Busy names the operator action the bot is running right now — "stop",
	// "kill" — so the page does not offer a second one.
	Busy string `json:"busy"`

	Log      []LogEntry `json:"log"`
	NoticeVI string     `json:"notice_vi"`
}

// noticeVI travels with every status.
const noticeVI = "CHỈ TESTNET — không tiền thật (PLAN Q18). Bot tự đặt lệnh qua đúng đường MỞ/ĐÓNG của portal; " +
	"mọi con số là của tài khoản testnet (funding, sổ lệnh, phí), không phải của mainnet. " +
	"Bot không bao giờ tự làm phẳng: trạng thái lệch hay bằng chứng không khớp là DỪNG BẢO VỆ."
