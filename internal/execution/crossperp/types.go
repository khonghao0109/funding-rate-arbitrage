package crossperp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/coordinator"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/execution"
	"futures-arbitrage-scanner/internal/risk"
)

// The two legs. Their spelling is part of every derived ClientOrderID.
const (
	LegLong  execution.LegName = "long"
	LegShort execution.LegName = "short"
)

// Purpose names why an order exists; it is part of its derived id.
type Purpose string

const (
	PurposeOpen   Purpose = "open"
	PurposeReduce Purpose = "reduce"
	PurposeUnwind Purpose = "unwind"
	PurposeClose  Purpose = "close"
)

// clientOrderIDPrefix versions the derivation, and differs from Engine 1's
// "fa1" so the two engines' ids can never collide.
const clientOrderIDPrefix = "cp1"

// ClientOrderID derives every order id one intent can produce.
//
// The OPENING order of each leg (PurposeOpen, attempt "", round 0) is
// DETERMINISTIC: a process restarted with nothing but the intent id can rebuild
// it and ask the venue about it — and it is the one order here that can rest.
//
// Every order that SHRINKS a leg — reduce, unwind, close — also carries the
// call's attempt nonce and its round. A second Close of the same intent must not
// reuse the first one's ids: Bybit's orderLinkId is "always unique" for
// futures, so a reused id is answered 110072 and read back as the OLD order, and
// a retried emergency close would never send anything again (review 4.5k, B1).
// Those orders are reduce-only MARKET orders that never rest, so a restarted
// process has no need to find them by id: the venue's position says what they did.
//
// 29 characters of [a-z0-9]: inside Binance's 36-character
// `^[\.A-Z\:/a-z0-9_-]{1,36}$` and Bybit's 36 letters, digits, - and _.
func ClientOrderID(intentID string, purpose Purpose, leg execution.LegName, attempt string, round int) string {
	sum := sha256.Sum256([]byte(clientOrderIDPrefix + "|" + intentID + "|" + string(purpose) + "|" + string(leg) + "|" + attempt + "|" + strconv.Itoa(round)))
	return clientOrderIDPrefix + string(leg)[:1] + string(purpose)[:1] + hex.EncodeToString(sum[:12])
}

// newAttemptNonce is 48 random bits naming one Open or Close call.
func newAttemptNonce() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on the platforms this runs on; a clock
		// reading keeps ids distinct between calls if it ever does.
		return "t" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// Venue is one perpetual venue: the name the coordinator and the margin guard
// know it by, and its USDⓈ-M / USDT-linear broker.
type Venue struct {
	Name   string
	Broker broker.Broker
}

// LegSpec is one leg's venue with the rules and the book it is sized and priced
// on — both read from THAT venue (broker/rounding.go: "BTCUSDT's tick size on
// this market of this venue right now").
type LegSpec struct {
	Venue Venue
	Rules exchanges.Instrument
	Book  depth.Summary
}

// Intent is one decision to open one pair. It is a VALUE: Open reads nothing
// from the world except the two brokers.
type Intent struct {
	ID     string
	Symbol string

	// Long is where the perp is BOUGHT, Short where it is SOLD.
	Long  LegSpec
	Short LegSpec

	// NotionalQuote is the target size of ONE leg, in quote.
	NotionalQuote float64

	// SignalEntryCostPct is the one-way slippage of both legs, in PERCENT of
	// notional, when the signal was made. The books are re-priced against it
	// before anything is sent; see Config.MaxEntryCostWidenBps.
	SignalEntryCostPct float64

	// MaxEntryCostWidenBpsOverride, when non-nil, replaces Config's tolerance for
	// THIS intent alone.
	//
	// It exists because a MANUAL open has no earlier signal to widen from. Its
	// SignalEntryCostPct is 0, so every real book "widens" past it by whatever
	// that book's own cost is, and the shipped 5 bps refuses every press on any
	// pair thinner than a major — measured on the Bybit testnet's ADAUSDT, where
	// the two books price the entry at 12.1 bps and the open was refused before
	// sending. Engine 1 has had the same exemption since 4.4b: cmd/execportal's
	// openAs sets execution.Config.MaxEntryCostWidenBps to +Inf unless the
	// caller supplied a signal cost, because for a button press the cost the
	// decision was made at IS the cost the book prices now.
	//
	// +Inf means "no tolerance check"; NaN is refused. A caller that DID price
	// an entry leaves this nil and gets the shipped tolerance, so the signal
	// path is not weakened by the manual one's exemption.
	MaxEntryCostWidenBpsOverride *float64
}

// entryCostWidenBps is the tolerance this intent is judged on.
func (i Intent) entryCostWidenBps(cfg Config) float64 {
	if i.MaxEntryCostWidenBpsOverride != nil {
		return *i.MaxEntryCostWidenBpsOverride
	}
	return cfg.MaxEntryCostWidenBps
}

// LockHolder is what Open asks the coordinator before sending anything.
type LockHolder interface {
	Holds(symbol string, engine coordinator.EngineID, intentID string) bool

	// MarkOrdersSent records durably that orders are about to be sent under the
	// lock (coordinator.MarkOrdersSent). An error means nothing may be sent.
	MarkOrdersSent(symbol string, engine coordinator.EngineID, intentID string) error
}

// EntryGate is the margin guard's refusal (risk.MarginGuard).
type EntryGate interface {
	AllowOpen(req risk.OpenRequest) error
}

// Config is the executor's parameters, units in the names (CLAUDE.md rule 4).
type Config struct {
	// MaxSlippageBps is how far past the touch a leg's marketable limit may
	// sit. 0 is a real setting ("the touch"); negative is refused.
	MaxSlippageBps float64

	// MaxEntryCostWidenBps is how much worse than SignalEntryCostPct the books
	// may price the entry before the intent is abandoned. 0 is a real setting.
	MaxEntryCostWidenBps float64

	// LegTimeout is how long an opening leg works before its remainder is
	// cancelled. A GUESS, written down as one: no cross-venue fill has been timed.
	LegTimeout time.Duration

	// OrderSettleTimeout bounds how long one order is read back for after a
	// cancel or a closing send, waiting for the venue to say it is finished.
	OrderSettleTimeout time.Duration

	// PositionSettleTimeout bounds how long the venues' positions are re-read
	// until they agree with what the orders filled — a position can trail the
	// fill that moved it.
	PositionSettleTimeout time.Duration

	// UnwindTimeout bounds every close-out: an unwind, a reduction, a Close.
	UnwindTimeout time.Duration

	// PollEvery is the read-back cadence.
	PollEvery time.Duration

	// AmbiguousSendQuiet is how long, from the moment a send whose answer was lost
	// RETURNED, the venue keeps being asked about that order — while both legs are
	// already being flattened. It bounds the ASKING, never the conclusion: a "no
	// such order" at any time proves nothing, and an opening order still unseen
	// when it ends leaves the result loud and the lock held (review 4.5k, N2). An
	// opening order of Engine 2 is NEVER resent (review 4.5k, B3): Binance can
	// answer -2013 before its backend has processed a request ("execution status
	// unknown", -1006/-1007), its newClientOrderId is unique only "among open
	// orders" so a resend of a filled original is accepted as a second order, and
	// a signed request stays acceptable for its whole recvWindow. The default is
	// broker's DefaultRecvWindowMs plus one second — a bound on the request, not
	// a measurement of any backend.
	AmbiguousSendQuiet time.Duration

	// MaxCloseRounds bounds the reduce-only orders one leg may send in one
	// close-out.
	MaxCloseRounds int

	// MaxBookAge is how stale the older of the two books may be.
	MaxBookAge time.Duration

	Now func() time.Time
}

// DefaultConfig is the shipped parameter set. The durations are Engine 1's
// shipped values and are as unmeasured for two venues as they were for one.
func DefaultConfig() Config {
	return Config{
		MaxSlippageBps:        execution.DefaultMaxSlippageBps,
		MaxEntryCostWidenBps:  5,
		LegTimeout:            execution.DefaultLegTimeout,
		OrderSettleTimeout:    5 * time.Second,
		PositionSettleTimeout: 5 * time.Second,
		UnwindTimeout:         30 * time.Second,
		PollEvery:             200 * time.Millisecond,
		AmbiguousSendQuiet:    time.Duration(broker.DefaultRecvWindowMs)*time.Millisecond + time.Second,
		MaxCloseRounds:        4,
		MaxBookAge:            60 * time.Second,
		Now:                   time.Now,
	}
}

// Outcome is what the venues' positions read when a call returned. Whether that
// reading is FINAL is the error's to say: a loud one (wrapping
// execution.ErrUnwindIncomplete or ErrFlatEvidenceConflict) means an order that
// may still execute stands beside it — flat venues included (review 4.5k round 3,
// minor 6).
type Outcome string

const (
	// OutcomeBothOpen: both legs hold the pair within the coarser step.
	OutcomeBothOpen Outcome = "both_open"
	// OutcomeBothFlat: nothing of the pair is held, and nothing needed closing.
	OutcomeBothFlat Outcome = "both_flat"
	// OutcomeUnwoundFlat: something filled and was closed again; both flat.
	OutcomeUnwoundFlat Outcome = "unwound_flat"
	// OutcomeUnresolved: the positions are neither of the above, or could not be
	// read. Always returned with a loud error that says why and prints both
	// venues' positions.
	OutcomeUnresolved Outcome = "unresolved"
)

// LegResult is what one leg did, GROSS.
type LegResult struct {
	Leg   execution.LegName
	Venue string
	Side  broker.Side

	ClientOrderID string
	VenueOrderID  string
	Status        broker.OrderStatus

	// OrderConfirmed: the venue said the opening order can no longer change.
	OrderConfirmed bool

	// The venue's own figures, nothing deducted (rule 2).
	FilledQtyCoin     float64
	AvgFillPriceQuote float64
	LimitPriceQuote   float64

	// ClosedQtyCoin is what unwind, reduce or close orders took off this leg.
	ClosedQtyCoin float64

	// VenuePositionQtyCoin is the leg's venue position at the end, SIGNED.
	VenuePositionQtyCoin float64
	VenuePositionRead    bool

	// FilledAtMs is when THIS PROCESS saw the opening order finish filling, on
	// Config.Now, so the two legs' stamps differ by a window and not a skew.
	FilledAtMs int64
}

// Result is one Open.
type Result struct {
	IntentID string
	Symbol   string
	Outcome  Outcome

	Long  LegResult
	Short LegResult

	TargetQtyCoin    float64
	CommonStepCoin   float64
	ToleranceQtyCoin float64

	// DeltaImbalanceQtyCoin is |long held − short held| at the end, from the
	// venues' positions when they were read.
	DeltaImbalanceQtyCoin float64

	RefMidQuote  float64
	BookAgeMs    int64
	EntryCostPct float64
	WidenBps     float64

	ReducedToMatch bool

	// UnhedgedWindow is the gap between the two fills, or — on a run that
	// unwound — from the first fill to the end of the unwind.
	UnhedgedWindow time.Duration

	// UnwindDuration is how long the close-out took. On a fake broker this is
	// microseconds and says nothing about a venue.
	UnwindDuration time.Duration

	// AttemptNonce is the part of this call's shrinking orders' ids that makes
	// them its own (ClientOrderID).
	AttemptNonce string

	// PendingOrders are this call's reduce-only orders still not proven finished
	// when it returned (PendingOrder). Never empty beside a quiet flat outcome.
	PendingOrders []PendingOrder

	// EvidenceVI states what the orders said and what the venues' positions
	// said, and whether they agreed.
	EvidenceVI string
	ReasonVI   string
}

// Hedged reports whether both legs ended open.
func (r Result) Hedged() bool { return r.Outcome == OutcomeBothOpen }

// The refusals before anything is sent. Every one wraps ErrRefusedBeforePlacing.
var (
	ErrRefusedBeforePlacing = errors.New("crossperp: từ chối trước khi gửi lệnh nào")

	ErrIntentInvalid       = fmt.Errorf("%w: ý định không mô tả một cặp perp–perp", ErrRefusedBeforePlacing)
	ErrLockNotHeld         = fmt.Errorf("%w: Động cơ 2 không giữ khóa symbol cho ý định này", ErrRefusedBeforePlacing)
	ErrMarginGate          = fmt.Errorf("%w: van ký quỹ từ chối", ErrRefusedBeforePlacing)
	ErrBookStale           = fmt.Errorf("%w: sổ lệnh quá cũ để làm bằng chứng", ErrRefusedBeforePlacing)
	ErrNoBestPrice         = fmt.Errorf("%w: sổ không có giá tốt nhất ở phía cần khớp", ErrRefusedBeforePlacing)
	ErrBookWidened         = fmt.Errorf("%w: sổ đã rộng quá chi phí vào được phép", ErrRefusedBeforePlacing)
	ErrIncommensurateSteps = fmt.Errorf("%w: bước khối lượng của hai sàn không chia hết cho nhau", ErrRefusedBeforePlacing)
	ErrSizeBelowMinimum    = fmt.Errorf("%w: cỡ chung dưới mức tối thiểu của một sàn", ErrRefusedBeforePlacing)
	ErrVenueNotFlat        = fmt.Errorf("%w: một sàn đã có vị thế hoặc lệnh treo trên symbol này", ErrRefusedBeforePlacing)
	ErrVenueUnreadable     = fmt.Errorf("%w: không đọc được vị thế sàn để chứng minh đang phẳng", ErrRefusedBeforePlacing)
	ErrOrdersNotMarked     = fmt.Errorf("%w: không ghi bền được dấu 'sắp gửi lệnh' vào khóa", ErrRefusedBeforePlacing)
)

// The outcomes that are not a hedge but are proven: nothing is held.
var (
	// ErrNotOpened: both legs ended flat without anything to unwind.
	ErrNotOpened = errors.New("crossperp: không mở được cặp — cả hai chân phẳng")
	// ErrOpenUnwound: something filled and was unwound to flat.
	ErrOpenUnwound = errors.New("crossperp: cặp không mở trọn — đã gỡ về phẳng")
)

// ErrPositionsUnverified: both legs' ORDERS say the pair is open and hedged, and
// the venues' positions could not be read to confirm it. The pair is recorded;
// only one piece of evidence exists (review 4.5k, m4).
var ErrPositionsUnverified = errors.New("crossperp: cặp mở theo lệnh khớp nhưng CHƯA đọc được vị thế sàn để xác nhận — chỉ có một bằng chứng")

// errReductionUnresolved: a reduction toward a size above zero sent an order it
// could not prove finished. Two reductions of the same size can both execute
// without either flipping the position, so the reduction stops and the pair is
// unwound to zero instead, where a late one is harmless (review 4.5k, B2).
var errReductionUnresolved = errors.New("crossperp: một lệnh thu nhỏ chưa chứng minh được là đã kết thúc — dừng thu nhỏ")

// The loud ones. Each wraps one of internal/execution's, so every caller that
// stops for Engine 1 stops for Engine 2.
var (
	// ErrLegAmbiguous: an order could not be proven finished at its venue, so it
	// may still fill after the call returned.
	ErrLegAmbiguous = fmt.Errorf("%w: một lệnh chưa chứng minh được là đã kết thúc trên sàn", execution.ErrUnwindIncomplete)

	// ErrFillEvidenceConflict: the orders' fills and the venues' positions
	// disagree past the settle deadline. Nothing more is sent.
	ErrFillEvidenceConflict = fmt.Errorf("%w: lệnh khớp và vị thế sàn nói khác nhau", execution.ErrFlatEvidenceConflict)

	// ErrCloseRefused: a close refused before sending anything.
	ErrCloseRefused = errors.New("crossperp: đóng bị từ chối trước khi gửi lệnh nào — hai chân còn nguyên")

	// ErrPositionDisagrees: a venue holds the wrong direction for this pair.
	ErrPositionDisagrees = fmt.Errorf("%w: vị thế sàn không phải hình dạng của cặp này", ErrCloseRefused)

	// ErrAdoptRefused: Adopt recorded nothing — the request, the lock or the
	// venues do not describe a pair of this engine it may take over.
	ErrAdoptRefused = errors.New("crossperp: không nhận cặp — không ghi nhận gì")
)
