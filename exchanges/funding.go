package exchanges

import (
	"fmt"
	"time"
)

// Funding rate types and the shared normalization contract (step 2.2).
//
// The 2026-08-28 survey plus the 2026-09-03 live verification (step 2.1,
// cmd/fundingcheck, and the WS probes recorded in DATA-REQUIREMENTS §3) found
// that no two venues quote funding the same way: the interval arrives in
// hours (Binance, Hyperliquid via predictedFundings), MINUTES (Bybit),
// seconds (Gate), only as two timestamps (OKX), or not at all (Kraken,
// Paradex); one venue quotes a rate that is per-1h while the rest are per-8h;
// and one has no settlement instant at all. Details and citations:
// docs/DATA-REQUIREMENTS.md §3–§4.
//
// Each venue's normalize<Venue>Funding builder lives in that venue's
// <venue>_funding.go (CONVENTIONS §4) and is the only place its RATE and
// INTERVAL unit rules live. A connector (step 2.5) parses the venue payload
// and hands the raw numbers, in the venue's own units, to its builder;
// everything downstream sees seconds and fractions only. The builders demand
// the full identity envelope — symbol, source, receive time, venue time — so
// an unidentified or unstamped reading cannot be constructed by forgetting a
// post-build assignment.
//
// The optional facts some venues attach (caps, mark/index price, rate type,
// the estimated flag, OKX's following period) are set by the connector after
// the build, already unit-normalized — their fields carry Frac/AtMs suffixes,
// and a venue that quotes one in an exotic unit gets its conversion added in
// its _funding.go, next to the other unit rules, not inline in the connector.

type FundingModel string

const (
	// FundingDiscrete: the position must be open at the settlement timestamp
	// to pay or receive anything — 7h59m of an 8h period earns zero. Count
	// settlements crossed; never multiply a rate by holding time.
	FundingDiscrete FundingModel = "discrete"
	// FundingContinuous: accrual via a funding index with no settlement
	// instant (Paradex Funding V2). NextFundingAtMs does not exist for this
	// model and stays 0.
	FundingContinuous FundingModel = "continuous"
)

// FundingData is one funding reading from one venue, raw next to normalized.
// Shape decided in docs/DATA-REQUIREMENTS.md §4 and corrected by the step-2.1
// verification; field names carry units per docs/CONVENTIONS.md §1.
//
// The builders fill identity, the raw block, the normalized block and
// NextFundingAtMs. Every field after that is optional venue metadata the
// connector sets when its payload carries it (step 2.5): absent means the
// venue did not supply it — groups with a Has* flag say so explicitly, and
// MarkPrice/IndexPrice/VenueTimeMs use 0 for "not supplied" the way
// PriceData does.
type FundingData struct {
	Symbol string // normalized: BTCUSDT
	Source string // wire id: binance_futures, okx_futures, ...
	Model  FundingModel

	// RawRate is the value exactly as the venue returned it, and RawRateField
	// names the payload field it was read from ("r", "relative_funding_rate",
	// ...). They exist so a number on the dashboard can be traced back to a
	// venue message during debugging — never for computation.
	RawRate      float64
	RawRateField string

	// The normalized block — the only fields the signal layer may read.
	// RatePerIntervalFrac is the rate for exactly ONE settlement interval of
	// this venue; IntervalSec is that interval in seconds, whatever unit the
	// venue quoted; RatePer8hFrac is the cross-venue comparison figure;
	// APRFrac counts the real number of settlements in a year — ×1095 for 8h,
	// ×8760 for 1h. For the continuous model there is no settlement:
	// RatePerIntervalFrac/IntervalSec describe the venue's QUOTE WINDOW, and
	// settlement-counting logic must branch on Model, not divide by
	// IntervalSec.
	RatePerIntervalFrac float64
	IntervalSec         int64
	RatePer8hFrac       float64
	APRFrac             float64

	// NextFundingAtMs is the upcoming settlement, absolute epoch ms. 0 means
	// no settlement instant exists or none was supplied: Model says which —
	// continuous never has one; a discrete reading with 0 came from an
	// endpoint that publishes none. IsEstimated marks a rate still moving
	// inside its window, as opposed to one locked for the current period.
	NextFundingAtMs int64
	IsEstimated     bool

	// The period after the next — only OKX ever publishes it, and since
	// method=current_period it arrives empty there too (measured 2026-09-03,
	// docs/DATA-REQUIREMENTS.md §3.3). Never parse "" as 0: absent is absent.
	FollowingRateFrac float64
	FollowingAtMs     int64
	HasFollowingRate  bool

	// Venue-published bounds on the rate, when the venue states them
	// (Binance fundingInfo cap/floor, OKX min/maxFundingRate, Bybit
	// fundingCap). A rate pinned at its cap is a regime signal, not noise.
	//
	// The two flags are SEPARATE because one venue publishes only half the
	// pair: Bybit's ticker carries fundingCap and no floor at all (measured
	// 2026-09-04). A single flag would make its unset floor read as a floor of
	// exactly 0 — "funding can never be negative" — which is false and would
	// make a phase-3 regime check treat every negative rate as pinned.
	RateCapFrac   float64
	RateFloorFrac float64
	HasCap        bool
	HasFloor      bool

	// MarkPrice is what funding is actually charged on at settlement — never
	// the entry price. IndexPrice rides along when the venue sends it.
	MarkPrice  float64
	IndexPrice float64

	// RateType: Binance labels dividend-driven rates "Special"; backtests
	// must filter them (docs/DATA-REQUIREMENTS.md §7.9).
	RateType string

	// Same clock contract as PriceData — see the header of types.go.
	// VenueTimeMs is the venue's own stamp, 0 when it publishes none; RecvAt
	// is stamped at the socket read in runSession and is the only basis for
	// staleness.
	VenueTimeMs int64
	RecvAt      time.Time
}

// fundingReading is the identity envelope plus the rate — the part every
// venue's builder needs, in every venue's units.
//
// It exists because the builders originally took these as positional
// parameters, which put two adjacent int64s (a venue timestamp and an
// interval) next to each other in every signature: transposing them compiles,
// and produces a settlement stamped in 1970 or an interval of 1.7 trillion
// seconds. Step 2.2 recorded that as debt to repay when 2.5 wrote the first
// real call sites; this is that repayment. Every builder now takes ONE struct
// whose fields are named at the call site.
type fundingReading struct {
	Symbol      string // normalized: BTCUSDT
	Source      string // wire id: binance_futures, ...
	RecvAt      time.Time
	VenueTimeMs int64   // the venue's own clock; 0 when it publishes none
	RateFrac    float64 // the rate as the venue quotes it, fractional
}

const (
	secPerHour  = 3600
	secPer8h    = 8 * secPerHour
	secPerYear  = 365 * 24 * secPerHour
	msPerSecond = 1000
)

// deriveFundingRates fills the comparison figures from the per-interval rate.
// It is the backstop for every builder: a non-positive interval is refused,
// because dividing by it would turn one bad message into an Inf/NaN APR that
// poisons every consumer downstream.
func deriveFundingRates(f FundingData) (FundingData, error) {
	if f.IntervalSec <= 0 {
		return FundingData{}, fmt.Errorf("funding %s/%s: non-positive IntervalSec %d", f.Source, f.Symbol, f.IntervalSec)
	}
	f.RatePer8hFrac = f.RatePerIntervalFrac * secPer8h / float64(f.IntervalSec)
	f.APRFrac = f.RatePerIntervalFrac * secPerYear / float64(f.IntervalSec)
	return f, nil
}
