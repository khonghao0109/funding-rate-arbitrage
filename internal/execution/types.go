package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/risk"
)

// LegName names one side of the pair. There are exactly two, and the spelling
// is part of the ClientOrderID, so changing one changes every id a restarted
// process would look for — see LegClientOrderID.
type LegName string

const (
	LegSpot LegName = "spot"
	LegPerp LegName = "perp"
)

// Outcome is the invariant, as a value. There are two.
type Outcome string

const (
	// OutcomeBothOpen: both legs hold quantity, within the coarser step.
	OutcomeBothOpen Outcome = "both_open"
	// OutcomeBothFlat: nothing this call opened is still held.
	OutcomeBothFlat Outcome = "both_flat"
)

// Intent is one decision to open one pair, and everything needed to act on it.
//
// It is a VALUE, complete at construction: the caller assembles the books, the
// prices and the venue rules, and Open reads nothing from the world except the
// broker. That is what makes every path testable against a fake.
type Intent struct {
	// ID identifies this intent for as long as it matters, including across a
	// crash. Both ClientOrderIDs are derived from it (LegClientOrderID), which
	// is what lets a restarted process ask the venue about orders it has no
	// other record of — the foundation of step 5.3.
	ID string

	Symbol string

	// The venue rules, as the venues published them. Not a second copy of
	// stepSize/tickSize/minNotional — the same exchanges.Instrument that
	// internal/instruments has filled since step 2.3.
	SpotInstrument exchanges.Instrument
	PerpInstrument exchanges.Instrument

	// The books that will price the fills, and the prices that size them.
	SpotBook depth.Summary
	PerpBook depth.Summary

	SpotPriceQuote float64
	PerpPriceQuote float64

	// NotionalQuote is the target size, anchored to the SPOT leg exactly as
	// instruments.SizeDeltaNeutral anchors it.
	NotionalQuote float64

	// SignalEntryCostPct is the one-way entry cost that was current when the
	// signal was made, in PERCENT of notional. The book is re-checked against
	// it immediately before placing; see Config.MaxEntryCostWidenBps.
	SignalEntryCostPct float64

	// PerpMarginFrac is the collateral posted on the perp leg as a fraction of
	// notional — a DECISION, not a venue fact. PerpBracket is that venue's
	// maintenance schedule; an unverified one is refused, never assumed zero.
	PerpMarginFrac float64
	PerpBracket    risk.Bracket
}

// Config is the parameters, each with its unit in the name where the type does
// not already carry one (CLAUDE.md rule 4).
type Config struct {
	// MaxEntryCostWidenBps is how much worse than the signal's own entry cost
	// the book may have become before the intent is abandoned. In BASIS POINTS
	// of notional. 0 means "no widening tolerated at all", which is a real and
	// usable setting, so there is no magic default hidden here — NewOpener
	// fills an UNSET config from DefaultConfig explicitly.
	MaxEntryCostWidenBps float64

	// LegTimeout is how long one leg may work before its remainder is
	// cancelled.
	LegTimeout time.Duration

	// UnwindTimeout bounds the close-out. It is deliberately separate from and
	// usually longer than LegTimeout: giving up on an unwind leaves exactly the
	// state this package exists to prevent.
	UnwindTimeout time.Duration

	// PollEvery is how often an open order is re-read from the venue while
	// working.
	PollEvery time.Duration

	// MaxResendPerLeg bounds resends, and a resend happens ONLY down the
	// "the venue positively does not have it" branch (broker.ErrOrderNotFound).
	MaxResendPerLeg int

	// MaxBookAge is how stale the authorising book may be. A book older than
	// this is not evidence about the current market.
	MaxBookAge time.Duration

	// Now is the clock, injectable so the unwind deadline is testable.
	Now func() time.Time
}

// DefaultConfig is the shipped parameter set.
func DefaultConfig() Config {
	return Config{
		MaxEntryCostWidenBps: 5,
		LegTimeout:           10 * time.Second,
		UnwindTimeout:        30 * time.Second,
		PollEvery:            200 * time.Millisecond,
		MaxResendPerLeg:      1,
		MaxBookAge:           60 * time.Second,
		Now:                  time.Now,
	}
}

// LegResult is what one leg did, GROSS.
type LegResult struct {
	Leg           LegName
	Market        broker.Market
	Symbol        string
	ClientOrderID string
	VenueOrderID  string
	Status        broker.OrderStatus

	// FilledQtyCoin and AvgFillPriceQuote are the venue's own figures with
	// nothing deducted (CLAUDE.md rule 2). The word "net" appears nowhere in
	// this package.
	FilledQtyCoin     float64
	AvgFillPriceQuote float64

	// FeeQuote is the commission the VENUE reported, 0 meaning NOT STATED
	// rather than free.
	//
	// It is 0 on every path today, and that is a limitation worth naming
	// rather than a bug: broker.Order carries no commission field, because
	// neither venue puts one on the order answer this package reads. Spot
	// reports it per fill inside the order response's `fills` array and
	// futures only through a separate userTrades call, and reading either is
	// step 4.5 work (realised PnL), not 4.4a. Until then the figure the caller
	// should use for cost is strategy's, not this one.
	FeeQuote float64

	// UnwoundQtyCoin is how much of this leg was closed again by the unwind,
	// 0 when nothing was.
	UnwoundQtyCoin float64
}

// NotionalQuote is what actually traded on this leg, GROSS.
func (l LegResult) NotionalQuote() float64 { return l.FilledQtyCoin * l.AvgFillPriceQuote }

// Result is the outcome of one Open. The invariant is Outcome.
type Result struct {
	IntentID string
	Outcome  Outcome

	Spot LegResult
	Perp LegResult

	// TargetQtyCoin is the common quantity both legs were sized to.
	TargetQtyCoin float64

	// ResidualQtyCoin is |spot filled - perp filled| after everything. On
	// OutcomeBothOpen it is within the coarser step; on OutcomeBothFlat both
	// sides are zero and so is this.
	ResidualQtyCoin float64

	// BookAgeMs is how old the book that authorised the entry was, in
	// milliseconds. 4.4a is a REST snapshot, so this is not decoration: it is
	// the measure of how much the decision could already be wrong.
	BookAgeMs int64

	// UnwindDuration is how long the close-out took, measured on Config.Now.
	// Zero when no unwind was needed. This is the number PLAN 4.4's acceptance
	// asks for — "leg 2 fails, leg 1 closes within a few seconds".
	UnwindDuration time.Duration

	// ReasonVI says why, in the operator's language, for every outcome that is
	// not a plain success.
	ReasonVI string
}

// Hedged reports whether both legs ended open.
func (r Result) Hedged() bool { return r.Outcome == OutcomeBothOpen }

// The refusals. Each is its own sentinel because they call for different
// actions: a book that widened will narrow again, a size under the venue
// minimum will not.
var (
	// ErrRefusedBeforePlacing marks every refusal that happened before ANY
	// order was sent. It wraps the specific ones below, so a caller that only
	// wants to know "did anything reach the venue" can ask that alone.
	ErrRefusedBeforePlacing = errors.New("execution: refused before anything was placed")

	ErrSizeBelowMinimum = fmt.Errorf("%w: the rounded size is under a venue minimum", ErrRefusedBeforePlacing)
	ErrBookWidened      = fmt.Errorf("%w: the book widened past the authorised entry cost", ErrRefusedBeforePlacing)
	ErrBookStale        = fmt.Errorf("%w: the authorising book is too old to be evidence", ErrRefusedBeforePlacing)
	ErrMarginUnverified = fmt.Errorf("%w: the perp venue's maintenance bracket is unverified", ErrRefusedBeforePlacing)
	ErrIntentInvalid    = fmt.Errorf("%w: the intent does not describe a position", ErrRefusedBeforePlacing)

	// ErrUnwindIncomplete is the one error that means the invariant may NOT
	// hold. It is returned only when the unwind itself could not be completed
	// or confirmed, and it is deliberately loud: this is the state the whole
	// package exists to prevent, and a caller seeing it must stop and involve
	// a human rather than open anything else.
	ErrUnwindIncomplete = errors.New("execution: UNWIND INCOMPLETE — the account may be unhedged, stop and reconcile by hand")
)

// clientOrderIDPrefix versions the derivation. If the scheme ever changes, the
// prefix changes with it, so ids minted by two versions cannot be confused —
// and a 5.3 recovery that finds neither knows which scheme it was looking for.
const clientOrderIDPrefix = "fa1"

// LegClientOrderID derives a leg's ClientOrderID from the intent id.
//
// DETERMINISTIC on purpose (see doc.go): a process killed between placing a leg
// and recording it comes back knowing only the intent id, and must be able to
// reconstruct exactly what it called the order in order to ask the venue about
// it. A random id would name a position the restarted process cannot see.
//
// The hash keeps the result inside Binance's documented 36-character limit and
// its `^[\.A-Z\:/a-z0-9_-]{1,36}$` charset whatever the caller's intent id
// looks like, while staying collision-free in practice: the leg name is mixed
// in BEFORE hashing, so the two legs of one intent differ everywhere rather
// than by a suffix.
// https://developers.binance.com/docs/binance-spot-api-docs/rest-api/trading-endpoints
func LegClientOrderID(intentID string, leg LegName) string {
	sum := sha256.Sum256([]byte(clientOrderIDPrefix + "|" + intentID + "|" + string(leg)))
	return clientOrderIDPrefix + string(leg)[:1] + hex.EncodeToString(sum[:12])
}
