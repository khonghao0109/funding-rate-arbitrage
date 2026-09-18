package risk

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// The dual margin guard for the cross-venue perp–perp engine (Engine 2),
// 2026-09-17 — docs/designs/cross-perp-engine-design.md §4.
//
// # Why an account-level ratio, and what it cannot see
//
// Evaluate (margin.go) prices ONE short perp leg at isolated margin before a
// position exists. This file watches what each VENUE says about the whole
// account while positions exist, because Engine 2's two legs sit on two venues
// whose margin is not fungible: a move that liquidates the losing leg leaves the
// winning leg's gain on the other venue, and the pair is then one naked perp.
//
// The ratio is maintenance margin ÷ margin balance, as a FRACTION; the venue
// liquidates at 1.
//
//   - Binance USDⓈ-M publishes no ratio in GET /fapi/v3/account (PLAN 4.5i,
//     spec correction 3). It is DERIVED from totalMaintMargin ÷
//     totalMarginBalance, both "USDT only in single-asset mode; USD-denominated
//     in multi-assets mode" (Account Information V3, read 2026-09-17) and both
//     from one answer, so they share a unit whatever the account's mode.
//   - Bybit UTA PUBLISHES accountMMRate, "Account MM rate"
//     (/v5/account/wallet-balance, read 2026-09-17). The glossary page that
//     defines it did not load from this environment (two timeouts, 2026-09-17)
//     and the testnet wallet has never held a position, so its UNIT is not
//     verified live. PublishedMarginReading therefore cross-checks it against
//     totalMaintenanceMargin ÷ totalMarginBalance from the same answer, and a
//     gap past MarginEvidenceToleranceFrac is a named conflict: two points is
//     tight enough to catch a percent-for-fraction slip at every level that
//     could change a tier (the lowest starts at 50%) — the same defence the
//     Bybit risk-limit unit trap needed (CLAUDE.md trap table).
//
// "All account wide fields are not applicable to isolated margin" (Bybit), and
// a Binance position on isolated margin is liquidated against its own wallet,
// not the account's. The venue readers REFUSE a reading on such an account
// (ErrMarginNotApplicable) rather than report a healthy number that does not
// describe the positions at risk.
//
// # Three tiers, and a fourth state
//
//	yellow ≥ 50%  Engine 2 may not open on that venue; every other open may
//	orange ≥ 60%  no engine may open anywhere
//	red    ≥ 65%  LATCHED: no engine opens, and Engine 2's pairs are closed one
//	              at a time — the pair with the most exposure on the venue with
//	              the highest ratio first, the leg on the most stressed venue OF
//	              THAT PAIR first — re-reading every venue after each close, for
//	              as long as some venue's FRESH evidence is at or above
//	              ReleaseFrac and a pair is left
//	unknown       no reading, a stale one, or a failed read: no engine may open
//	              on that venue, and NOTHING is closed because of it — not to
//	              start an emergency and not to continue one (review 4.5k, M3):
//	              a close costs a round trip, and a reading that does not exist
//	              is not a reason to pay one
//
// A CONFLICT — the venue's published ratio and the ratio of its own two totals
// more than MarginEvidenceToleranceFrac apart — is its own state. It is never
// averaged and never picked between; each decision takes the figure that errs
// SAFE for it (review 4.5k, M8):
//
//   - opens on that venue are refused, as for unknown;
//   - the system-wide block on opens uses the largest HIGHER figure of the
//     conflicting answers within StaleAfter of the newest one — a 90% published
//     against an 86% derived blocks every open — so one high print does not block
//     every venue for as long as the venue keeps answering in conflict at 10%
//     (review 4.5k, N6), and neither a flap nor a peak followed by lower orange
//     answers opens the system on one low print (round 3, M6). A failed read
//     keeps the figures: staleness never lowers a block;
//   - an emergency close STARTS and CONTINUES only on the LOWER figure, so a
//     close is never paid for on a number the venue's own totals contradict;
//   - which venue a close relieves first is ranked on the HIGHER figure;
//   - the red latch may be acknowledged, and a running emergency stops, once
//     BOTH figures are below ReleaseFrac.
//
// The red latch does not clear when the ratio recovers. An operator
// acknowledges the sequence number they were shown, and only while every venue's
// fresh evidence — both figures of a conflict — reads below ReleaseFrac: the
// portal's halt semantics (PLAN 4.5d), for the same reason: a condition that
// heals between two page refreshes is one nobody saw.
//
// # What the guard does not do
//
// It places no order and imports no broker — internal/strategy imports this
// package, and the gate's own binary must not link a credential (broker
// boundary_test.go). It DECIDES which pair to close and when; an EmergencyCloser
// executes (internal/execution/crossperp). It never touches Engine 1, whose spot
// long cannot be liquidated and whose perp short has its own exits (PLAN
// 4.5d–4.5g): a red venue held up by Engine 1's margin reports that no Engine-2
// pair is left, and stops. The design's "shorten the exit scan to 5 seconds at
// orange" belongs to an Engine-2 exit loop that does not exist yet; Snapshot
// exposes the tier for it.

// MarginTier is a venue's margin state as the guard acts on it.
type MarginTier string

const (
	MarginTierUnknown MarginTier = "unknown"
	MarginTierGreen   MarginTier = "green"
	MarginTierYellow  MarginTier = "yellow"
	MarginTierOrange  MarginTier = "orange"
	MarginTierRed     MarginTier = "red"
)

// MarginThresholds are the tier boundaries, each a FRACTION of the ratio at
// which the venue liquidates (1). A ratio AT a boundary is in the higher tier.
type MarginThresholds struct {
	YellowFrac float64
	OrangeFrac float64
	RedFrac    float64

	// ReleaseFrac is where the emergency close stops and below which the red
	// latch may be acknowledged. The design's "< 50%".
	ReleaseFrac float64
}

// DefaultMarginThresholds is the design's table: 50 / 60 / 65, release under 50.
func DefaultMarginThresholds() MarginThresholds {
	return MarginThresholds{YellowFrac: 0.50, OrangeFrac: 0.60, RedFrac: 0.65, ReleaseFrac: 0.50}
}

func (t MarginThresholds) validate() error {
	inRange := func(v float64) bool { return v > 0 && v < 1 }
	switch {
	case !inRange(t.YellowFrac) || !inRange(t.OrangeFrac) || !inRange(t.RedFrac) || !inRange(t.ReleaseFrac):
		return fmt.Errorf("risk: every margin threshold must be a fraction inside (0, 1) — the venue liquidates at 1: %+v", t)
	case !(t.YellowFrac <= t.OrangeFrac && t.OrangeFrac <= t.RedFrac):
		return fmt.Errorf("risk: margin thresholds must rise yellow ≤ orange ≤ red: %+v", t)
	case t.ReleaseFrac > t.RedFrac:
		return fmt.Errorf("risk: ReleaseFrac %v is above RedFrac %v — the emergency close would stop while still red", t.ReleaseFrac, t.RedFrac)
	}
	return nil
}

// TierOf places one ratio. NaN and a negative ratio are not ratios: unknown.
func (t MarginThresholds) TierOf(ratioFrac float64) MarginTier {
	switch {
	case math.IsNaN(ratioFrac) || ratioFrac < 0:
		return MarginTierUnknown
	case ratioFrac >= t.RedFrac:
		return MarginTierRed
	case ratioFrac >= t.OrangeFrac:
		return MarginTierOrange
	case ratioFrac >= t.YellowFrac:
		return MarginTierYellow
	}
	return MarginTierGreen
}

// MarginEvidenceToleranceFrac is how far a venue's PUBLISHED ratio may sit from
// the ratio of the two totals it published beside it, as a fraction (0.02 = two
// percentage points). See the file comment for why two points.
const MarginEvidenceToleranceFrac = 0.02

// The refusals a reading can end in. Every one of them makes the venue UNKNOWN.
var (
	ErrMarginNotPublished     = errors.New("risk: sàn không công bố tỷ lệ ký quỹ — KHÔNG BIẾT, không bao giờ là 'an toàn'")
	ErrMarginNotApplicable    = errors.New("risk: tỷ lệ ký quỹ cấp tài khoản không mô tả các vị thế này (ký quỹ isolated)")
	ErrMarginReadingInvalid   = errors.New("risk: số liệu ký quỹ không dựng được tỷ lệ")
	ErrMarginEvidenceConflict = errors.New("risk: HAI BẰNG CHỨNG KÝ QUỸ KHÔNG KHỚP — tỷ lệ sàn công bố và tỷ lệ suy ra từ hai tổng của cùng câu trả lời nói khác nhau")

	// ErrOpenBlockedByMargin wraps every AllowOpen refusal.
	ErrOpenBlockedByMargin = errors.New("risk: van ký quỹ từ chối mở vị thế mới")

	// ErrNothingToAcknowledge is an acknowledgement with no red latch set.
	ErrNothingToAcknowledge = errors.New("risk: không có van đỏ nào đang chốt")
)

// MarginReading is one venue's account-level margin at one instant.
type MarginReading struct {
	Venue string

	// MaintenanceMarginRatioFrac is maintenance margin ÷ margin balance, a
	// FRACTION: 0.5 is 50% and the venue liquidates at 1. +Inf means a
	// maintenance requirement against a margin balance at or below zero — an
	// account at or past liquidation.
	MaintenanceMarginRatioFrac float64

	// The two totals, in QuoteAssetVI. REPORTED beside the ratio; the tier is
	// decided on the ratio alone.
	MaintenanceMarginQuote float64
	MarginBalanceQuote     float64
	QuoteAssetVI           string

	// SourceVI says whether the venue published the ratio or it was derived
	// here, from which endpoint, and what it was checked against.
	SourceVI string

	// PublishedRatioFrac and DerivedRatioFrac are the two pieces of evidence
	// behind a published reading, and EvidenceConflict says they disagreed. A
	// conflicting reading is returned WITH its error, so the guard can take the
	// safe side of each decision without ever averaging them.
	PublishedRatioFrac float64
	DerivedRatioFrac   float64
	EvidenceConflict   bool

	ReadAtMs int64
}

// DerivedMarginReading builds a reading from the two account totals — the only
// form Binance USDⓈ-M V3 offers.
func DerivedMarginReading(venue, quoteAssetVI string, maintenanceMarginQuote, marginBalanceQuote float64, sourceVI string, readAtMs int64) (MarginReading, error) {
	out := MarginReading{Venue: venue, MaintenanceMarginQuote: maintenanceMarginQuote, MarginBalanceQuote: marginBalanceQuote,
		QuoteAssetVI: quoteAssetVI, SourceVI: sourceVI, ReadAtMs: readAtMs}
	ratio, err := maintenanceRatioFrac(maintenanceMarginQuote, marginBalanceQuote)
	if err != nil {
		return out, err
	}
	out.MaintenanceMarginRatioFrac, out.DerivedRatioFrac = ratio, ratio
	return out, nil
}

// PublishedMarginReading builds a reading from a ratio the VENUE published, and
// cross-checks it against the two totals the venue published beside it.
//
// published=false is the venue sending the ratio as "" — Bybit's
// Wallet.RatesPublished — and is ErrMarginNotPublished: a blank is not zero.
func PublishedMarginReading(venue, quoteAssetVI string, publishedRatioFrac float64, published bool,
	maintenanceMarginQuote, marginBalanceQuote float64, sourceVI string, readAtMs int64) (MarginReading, error) {

	out := MarginReading{Venue: venue, MaintenanceMarginQuote: maintenanceMarginQuote, MarginBalanceQuote: marginBalanceQuote,
		QuoteAssetVI: quoteAssetVI, SourceVI: sourceVI, ReadAtMs: readAtMs}
	if !published {
		return out, fmt.Errorf("%w: %s", ErrMarginNotPublished, sourceVI)
	}
	if math.IsNaN(publishedRatioFrac) || math.IsInf(publishedRatioFrac, 0) || publishedRatioFrac < 0 {
		return out, fmt.Errorf("%w: tỷ lệ sàn công bố %v", ErrMarginReadingInvalid, publishedRatioFrac)
	}
	out.MaintenanceMarginRatioFrac = publishedRatioFrac
	derived, err := maintenanceRatioFrac(maintenanceMarginQuote, marginBalanceQuote)
	if err != nil {
		return out, fmt.Errorf("%w (hai tổng đi kèm tỷ lệ công bố %.6f không dùng được để đối chiếu)", err, publishedRatioFrac)
	}
	out.PublishedRatioFrac, out.DerivedRatioFrac = publishedRatioFrac, derived
	conflict := math.IsInf(derived, 1) && publishedRatioFrac < 1 ||
		!math.IsInf(derived, 1) && math.Abs(derived-publishedRatioFrac) > MarginEvidenceToleranceFrac
	if conflict {
		out.EvidenceConflict = true
		return out, fmt.Errorf("%w: sàn công bố %.6f, hai tổng cho %.6f ÷ %.6f = %.6f — lệch quá %.0f điểm phần trăm; %s",
			ErrMarginEvidenceConflict, publishedRatioFrac, maintenanceMarginQuote, marginBalanceQuote, derived,
			MarginEvidenceToleranceFrac*100, sourceVI)
	}
	out.SourceVI = fmt.Sprintf("%s (đối chiếu hai tổng: %.6f, trong %.0f điểm %%)", sourceVI, derived, MarginEvidenceToleranceFrac*100)
	return out, nil
}

func maintenanceRatioFrac(maintenanceMarginQuote, marginBalanceQuote float64) (float64, error) {
	switch {
	case math.IsNaN(maintenanceMarginQuote) || math.IsInf(maintenanceMarginQuote, 0) || maintenanceMarginQuote < 0:
		return 0, fmt.Errorf("%w: ký quỹ duy trì %v", ErrMarginReadingInvalid, maintenanceMarginQuote)
	case math.IsNaN(marginBalanceQuote) || math.IsInf(marginBalanceQuote, 0):
		return 0, fmt.Errorf("%w: số dư ký quỹ %v", ErrMarginReadingInvalid, marginBalanceQuote)
	case marginBalanceQuote > 0:
		return maintenanceMarginQuote / marginBalanceQuote, nil
	case maintenanceMarginQuote == 0:
		// Nothing needs maintaining, so nothing can be liquidated, whatever
		// the balance.
		return 0, nil
	}
	return math.Inf(1), nil
}

// MarginReader reads one venue's account margin. Implementations live beside
// the credentials (internal/execution/crossperp), never in this package.
type MarginReader interface {
	// VenueName is the stable name the guard reports and gates on. It must
	// match the names engines pass in OpenRequest.Venues.
	VenueName() string
	ReadMargin(ctx context.Context) (MarginReading, error)
}

// ExposedPair is one open Engine-2 pair as the emergency close ranks it.
type ExposedPair struct {
	PairID string
	Symbol string

	// ExposureQuoteByVenue is each leg's size in quote on its venue, GROSS.
	// ExposureBasisVI says what price it was valued at — a mark price, or the
	// entry fill — because the two rank differently after a move.
	ExposureQuoteByVenue map[string]float64
	ExposureBasisVI      string

	// CloseBlockedVI, when set, says why this pair must NOT be closed
	// automatically — its own evidence conflicts. The guard raises it and moves on.
	CloseBlockedVI string
}

// EmergencyCloser executes what the guard decides.
type EmergencyCloser interface {
	// OpenPairs lists Engine 2's open pairs. An error is not an empty list.
	OpenPairs(ctx context.Context) ([]ExposedPair, error)

	// CloseForMargin closes both legs of one pair, the leg on stressedVenue
	// first. It returns nil only when the venues read the pair flat.
	CloseForMargin(ctx context.Context, pairID, stressedVenue string) error
}

// MarginEventKind names what the guard saw or did.
type MarginEventKind string

const (
	MarginEventReadFailed       MarginEventKind = "read_failed"
	MarginEventTierChanged      MarginEventKind = "tier_changed"
	MarginEventEmergencyStarted MarginEventKind = "emergency_started"
	MarginEventPairClosing      MarginEventKind = "pair_closing"
	MarginEventPairClosed       MarginEventKind = "pair_closed"
	MarginEventPairCloseFailed  MarginEventKind = "pair_close_failed"
	MarginEventNothingToClose   MarginEventKind = "nothing_to_close"
	MarginEventBelowRelease     MarginEventKind = "below_release"
	MarginEventAcknowledged     MarginEventKind = "acknowledged"
	MarginEventCloseBlocked     MarginEventKind = "close_blocked"
	MarginEventStoppedOnUnknown MarginEventKind = "stopped_on_unknown"
)

// MarginEvent is one alarm-worthy fact, in the operator's language.
type MarginEvent struct {
	AtMs         int64
	Kind         MarginEventKind
	Venue        string
	Tier         MarginTier
	RatioFrac    float64
	PairID       string
	EmergencySeq uint64
	MessageVI    string
	Err          error
}

// OpenRequest is what an engine asks before it sends an opening order.
type OpenRequest struct {
	Engine string

	// CrossVenuePerp marks the levered perp–perp engine, which the YELLOW tier
	// stops on the venue it names. Engine 1 leaves it false.
	CrossVenuePerp bool

	// Venues are the guard's names of every venue the open would place on.
	Venues []string
}

// MarginGuardConfig holds the guard's parameters, each unit in its type or name.
type MarginGuardConfig struct {
	// Interval is the read cadence — the design's five seconds.
	Interval time.Duration

	// ReadTimeout bounds one venue's read, so one slow venue cannot stall the
	// other's.
	ReadTimeout time.Duration

	// StaleAfter is how old a successful reading may be and still decide a
	// tier. Past it the venue is UNKNOWN.
	StaleAfter time.Duration

	Thresholds MarginThresholds

	// MaxEmergencyClosesPerTick bounds how many pairs one tick may try to close,
	// so a closer that keeps failing cannot spin inside a tick.
	MaxEmergencyClosesPerTick int

	// CloseTimeout bounds how long the guard WAITS to start one pair's
	// emergency close — the reads before it, and the engine's wait for an
	// operation already running on that symbol. It does not cut a close short
	// once orders are going out: the executor's close-outs run on budgets of
	// their own, because a close abandoned half way is the naked leg it exists
	// to prevent (review 4.5k round 3, minor 5).
	CloseTimeout time.Duration

	// Now is the clock; it is called from several goroutines at once.
	Now func() time.Time

	// OnEvent receives every event, never while the guard holds its lock, so
	// it may call Snapshot. nil keeps events only in Snapshot's recent list.
	OnEvent func(MarginEvent)
}

// DefaultMarginGuardConfig is the design's cadence and table.
func DefaultMarginGuardConfig() MarginGuardConfig {
	return MarginGuardConfig{
		Interval:                  5 * time.Second,
		ReadTimeout:               4 * time.Second,
		StaleAfter:                15 * time.Second,
		Thresholds:                DefaultMarginThresholds(),
		MaxEmergencyClosesPerTick: 20,
		CloseTimeout:              60 * time.Second,
		Now:                       time.Now,
	}
}

// recentEventsKept bounds the in-memory event list Snapshot returns.
const recentEventsKept = 64

type venueMargin struct {
	name   string
	reader MarginReader

	reading    MarginReading
	hasReading bool
	err        error
	errAtMs    int64
	lastTier   MarginTier

	// conflict is the newest answer when it was a conflicting reading.
	conflict    MarginReading
	hasConflict bool

	// conflictHighs are the HIGHER figures of the conflicting answers since the
	// last clean reading, each with its stamp; the system-wide block stands on the
	// largest of those within StaleAfter of the newest. One high print must not
	// block every venue for as long as the venue keeps answering in conflict at
	// 10% (review 4.5k, N6), a venue flapping between a high and a low
	// contradiction must not open the system on each low one, and every orange
	// answer of the window counts, not only its peak (round 3, M6). A clean reading
	// clears them; a failed read keeps them, so the block stays up when the venue
	// then stops answering: staleness never lowers a block.
	conflictHighs []ratioAt
}

// ratioAt is one figure and when it was stated.
type ratioAt struct {
	ratioFrac float64
	atMs      int64
}

// conflictBlockFracLocked is the figure the system-wide block stands on.
func (v *venueMargin) conflictBlockFracLocked() float64 {
	high := 0.0
	for _, r := range v.conflictHighs {
		high = math.Max(high, r.ratioFrac)
	}
	return high
}

// MarginGuard watches every venue's account margin and gates and unwinds on it.
// Safe for concurrent use: Tick serialises with itself, and AllowOpen, Snapshot
// and Acknowledge only take the state lock, which no venue call ever holds.
type MarginGuard struct {
	cfg    MarginGuardConfig
	closer EmergencyCloser
	venues []*venueMargin // fixed at construction

	tickMu sync.Mutex

	mu               sync.Mutex
	byName           map[string]*venueMargin
	emergency        bool
	emergencySeq     uint64
	belowReleaseSeen uint64 // the emergency sequence below_release was last reported for
	reported         map[string]uint64
	recent           []MarginEvent
}

// NewMarginGuard builds a guard over the given venues. closer may be nil: the
// guard then still gates every open and raises every alarm, and a red venue
// reports that nothing can close its pairs.
func NewMarginGuard(cfg MarginGuardConfig, readers []MarginReader, closer EmergencyCloser) (*MarginGuard, error) {
	d := DefaultMarginGuardConfig()
	if cfg.Interval <= 0 {
		cfg.Interval = d.Interval
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = d.ReadTimeout
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = d.StaleAfter
	}
	if cfg.Thresholds == (MarginThresholds{}) {
		cfg.Thresholds = d.Thresholds
	}
	if cfg.MaxEmergencyClosesPerTick <= 0 {
		cfg.MaxEmergencyClosesPerTick = d.MaxEmergencyClosesPerTick
	}
	if cfg.CloseTimeout <= 0 {
		cfg.CloseTimeout = d.CloseTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if err := cfg.Thresholds.validate(); err != nil {
		return nil, err
	}
	if len(readers) == 0 {
		return nil, errors.New("risk: a margin guard with no venue to read guards nothing")
	}
	g := &MarginGuard{cfg: cfg, closer: closer, byName: map[string]*venueMargin{}, reported: map[string]uint64{}}
	for _, r := range readers {
		if r == nil {
			return nil, errors.New("risk: nil margin reader")
		}
		name := strings.TrimSpace(r.VenueName())
		if name == "" {
			return nil, errors.New("risk: a margin reader with no venue name cannot be gated on")
		}
		if _, dup := g.byName[name]; dup {
			return nil, fmt.Errorf("risk: two margin readers for venue %q", name)
		}
		v := &venueMargin{name: name, reader: r, lastTier: MarginTierUnknown}
		g.venues = append(g.venues, v)
		g.byName[name] = v
	}
	return g, nil
}

// Run ticks at cfg.Interval until ctx ends.
func (g *MarginGuard) Run(ctx context.Context) error {
	g.Tick(ctx)
	t := time.NewTicker(g.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			g.Tick(ctx)
		}
	}
}

// MarginTickReport is what one Tick saw and did.
type MarginTickReport struct {
	AtMs          int64
	Venues        []MarginVenueView
	Emergency     bool
	EmergencySeq  uint64
	ClosedPairIDs []string
	FailedPairIDs []string
}

// Tick reads every venue once and acts on what it read.
func (g *MarginGuard) Tick(ctx context.Context) MarginTickReport {
	g.tickMu.Lock()
	defer g.tickMu.Unlock()

	g.readAll(ctx)

	var events []MarginEvent
	g.mu.Lock()
	nowMs := g.cfg.Now().UnixMilli()
	if !g.emergency {
		for _, v := range g.venues {
			ratio, why, red := g.freshRedLocked(v, nowMs)
			if !red {
				continue
			}
			g.emergency = true
			g.emergencySeq++
			// Keys of a finished emergency can never match again (review 4.5k, m-d).
			g.reported = map[string]uint64{}
			events = append(events, MarginEvent{AtMs: nowMs, Kind: MarginEventEmergencyStarted, Venue: v.name,
				Tier: MarginTierRed, RatioFrac: ratio, EmergencySeq: g.emergencySeq,
				MessageVI: fmt.Sprintf("NGẮT ĐỎ #%d: %s ở %.2f%% ≥ %.0f%%%s — cấm mọi lệnh mở, đóng các cặp Động cơ 2",
					g.emergencySeq, v.name, ratio*100, g.cfg.Thresholds.RedFrac*100, why)})
			break
		}
	}
	inEmergency := g.emergency
	g.mu.Unlock()
	g.dispatch(events)

	report := MarginTickReport{}
	if inEmergency {
		report.ClosedPairIDs, report.FailedPairIDs = g.runEmergency(ctx)
	}
	snap := g.Snapshot()
	report.AtMs, report.Venues = snap.AtMs, snap.Venues
	report.Emergency, report.EmergencySeq = snap.Emergency, snap.EmergencySeq
	return report
}

// readAll reads every venue at once, each under its own timeout, and records
// what came back. No lock is held while a venue is being asked.
func (g *MarginGuard) readAll(ctx context.Context) {
	type outcome struct {
		reading MarginReading
		err     error
		atMs    int64
	}
	results := make([]outcome, len(g.venues))
	var wg sync.WaitGroup
	for i, v := range g.venues {
		wg.Add(1)
		go func(i int, reader MarginReader) {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, g.cfg.ReadTimeout)
			defer cancel()
			r, err := reader.ReadMargin(rctx)
			results[i] = outcome{reading: r, err: err, atMs: g.cfg.Now().UnixMilli()}
		}(i, v.reader)
	}
	wg.Wait()

	var events []MarginEvent
	g.mu.Lock()
	for i, v := range g.venues {
		res := results[i]
		err := res.err
		if err == nil && res.reading.Venue != "" && res.reading.Venue != v.name {
			err = fmt.Errorf("%w: bộ đọc của %q trả số đọc mang tên %q", ErrMarginReadingInvalid, v.name, res.reading.Venue)
		}
		if err == nil && (math.IsNaN(res.reading.MaintenanceMarginRatioFrac) || res.reading.MaintenanceMarginRatioFrac < 0) {
			err = fmt.Errorf("%w: tỷ lệ %v", ErrMarginReadingInvalid, res.reading.MaintenanceMarginRatioFrac)
		}
		if err != nil {
			changed := v.err == nil || v.err.Error() != err.Error()
			v.err, v.errAtMs = err, res.atMs
			v.hasConflict = false
			if errors.Is(err, ErrMarginEvidenceConflict) && res.reading.EvidenceConflict {
				c := res.reading
				c.Venue = v.name
				if c.ReadAtMs <= 0 {
					c.ReadAtMs = res.atMs
				}
				v.conflict, v.hasConflict = c, true
				kept := v.conflictHighs[:0]
				for _, r := range v.conflictHighs {
					if c.ReadAtMs-r.atMs <= g.cfg.StaleAfter.Milliseconds() {
						kept = append(kept, r)
					}
				}
				v.conflictHighs = append(kept, ratioAt{ratioFrac: conflictHighFrac(c), atMs: c.ReadAtMs})
			}
			if changed {
				events = append(events, MarginEvent{AtMs: res.atMs, Kind: MarginEventReadFailed, Venue: v.name,
					Tier: MarginTierUnknown, Err: err,
					MessageVI: fmt.Sprintf("không đọc được ký quỹ %s — KHÔNG BIẾT: cấm mở trên sàn này, không đóng gì vì nó: %v", v.name, err)})
			}
		} else {
			r := res.reading
			r.Venue = v.name
			if r.ReadAtMs <= 0 {
				r.ReadAtMs = res.atMs
			}
			v.reading, v.hasReading, v.err, v.errAtMs = r, true, nil, 0
			v.hasConflict, v.conflictHighs = false, nil
		}
		if tier, why := g.tierLocked(v, res.atMs); tier != v.lastTier {
			events = append(events, MarginEvent{AtMs: res.atMs, Kind: MarginEventTierChanged, Venue: v.name, Tier: tier,
				RatioFrac: v.reading.MaintenanceMarginRatioFrac, EmergencySeq: g.emergencySeq,
				MessageVI: fmt.Sprintf("%s: %s → %s %s", v.name, v.lastTier, tier, why)})
			v.lastTier = tier
		}
	}
	g.mu.Unlock()
	g.dispatch(events)
}

// tierLocked is a venue's EFFECTIVE tier now: unknown unless its newest
// successful reading is also its newest answer and still fresh.
func (g *MarginGuard) tierLocked(v *venueMargin, nowMs int64) (MarginTier, string) {
	switch {
	case v.err != nil:
		return MarginTierUnknown, "(lần đọc gần nhất hỏng: " + v.err.Error() + ")"
	case !v.hasReading:
		return MarginTierUnknown, "(chưa có lần đọc nào)"
	}
	age := nowMs - v.reading.ReadAtMs
	if age > g.cfg.StaleAfter.Milliseconds() {
		return MarginTierUnknown, fmt.Sprintf("(số đọc đã cũ %d ms > %d ms)", age, g.cfg.StaleAfter.Milliseconds())
	}
	return g.cfg.Thresholds.TierOf(v.reading.MaintenanceMarginRatioFrac),
		fmt.Sprintf("(%.2f%% — %s)", v.reading.MaintenanceMarginRatioFrac*100, v.reading.SourceVI)
}

// lastReadTierLocked is the tier of the newest SUCCESSFUL reading, however old.
// A venue last read orange keeps the whole system closed to new positions until
// a fresh reading says otherwise: staleness never lowers a block.
func (g *MarginGuard) lastReadTierLocked(v *venueMargin) MarginTier {
	if !v.hasReading {
		return MarginTierUnknown
	}
	return g.cfg.Thresholds.TierOf(v.reading.MaintenanceMarginRatioFrac)
}

// freshRatioLocked is the figure a venue's newest FRESH answer states for the
// decisions that ACT: a clean reading's ratio, or — for a conflict — the LOWER
// of its two figures. ok is false when the venue has no fresh answer of either
// kind.
func (g *MarginGuard) freshRatioLocked(v *venueMargin, nowMs int64) (ratioFrac float64, whyVI string, ok bool) {
	stale := g.cfg.StaleAfter.Milliseconds()
	switch {
	case v.err == nil && v.hasReading && nowMs-v.reading.ReadAtMs <= stale:
		return v.reading.MaintenanceMarginRatioFrac, "", true
	case v.hasConflict && nowMs-v.conflict.ReadAtMs <= stale:
		return conflictLowFrac(v.conflict), fmt.Sprintf(" (bằng chứng mâu thuẫn: công bố %.2f%%, suy ra %.2f%% — lấy số THẤP hơn)",
			v.conflict.PublishedRatioFrac*100, v.conflict.DerivedRatioFrac*100), true
	}
	return 0, "", false
}

// freshHighRatioLocked is the figure a venue's newest FRESH answer states for the
// decisions that must err HIGH: a clean reading's ratio, or the HIGHER figure of
// a conflict. ok is false when the venue has no fresh answer of either kind.
func (g *MarginGuard) freshHighRatioLocked(v *venueMargin, nowMs int64) (float64, bool) {
	stale := g.cfg.StaleAfter.Milliseconds()
	switch {
	case v.err == nil && v.hasReading && nowMs-v.reading.ReadAtMs <= stale:
		return v.reading.MaintenanceMarginRatioFrac, true
	case v.hasConflict && nowMs-v.conflict.ReadAtMs <= stale:
		return conflictHighFrac(v.conflict), true
	}
	return 0, false
}

// freshRedLocked reports whether a venue's fresh evidence says red — on a
// conflict, only when BOTH figures do.
func (g *MarginGuard) freshRedLocked(v *venueMargin, nowMs int64) (float64, string, bool) {
	ratio, why, ok := g.freshRatioLocked(v, nowMs)
	return ratio, why, ok && ratio >= g.cfg.Thresholds.RedFrac
}

func conflictLowFrac(r MarginReading) float64 {
	return math.Min(r.PublishedRatioFrac, r.DerivedRatioFrac)
}
func conflictHighFrac(r MarginReading) float64 {
	return math.Max(r.PublishedRatioFrac, r.DerivedRatioFrac)
}

// AllowOpen answers an engine about to send an opening order. nil means allowed;
// every refusal wraps ErrOpenBlockedByMargin and names the venue and the number.
func (g *MarginGuard) AllowOpen(req OpenRequest) error {
	if len(req.Venues) == 0 {
		return fmt.Errorf("%w: yêu cầu mở không nêu sàn nào — không kiểm được gì", ErrOpenBlockedByMargin)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	nowMs := g.cfg.Now().UnixMilli()
	if g.emergency {
		return fmt.Errorf("%w: NGẮT ĐỎ #%d đang chốt — chờ người vận hành xác nhận", ErrOpenBlockedByMargin, g.emergencySeq)
	}
	for _, v := range g.venues {
		// The newest SUCCESSFUL reading decides the system-wide block even when
		// it has gone stale or a later read failed: staleness never lowers a block.
		if tier := g.lastReadTierLocked(v); tier == MarginTierOrange || tier == MarginTierRed {
			return fmt.Errorf("%w: %s đọc gần nhất ở mức %s (%.2f%% ≥ %.0f%%) — cấm mở mới trên TOÀN hệ thống",
				ErrOpenBlockedByMargin, v.name, tier, v.reading.MaintenanceMarginRatioFrac*100, g.cfg.Thresholds.OrangeFrac*100)
		}
		// A conflicting answer blocks on its HIGHER figure, until a clean reading
		// or a newer conflicting answer replaces it (review 4.5k, M8 and N6).
		if high := v.conflictBlockFracLocked(); g.cfg.Thresholds.TierOf(high) == MarginTierOrange || g.cfg.Thresholds.TierOf(high) == MarginTierRed {
			return fmt.Errorf("%w: %s có bằng chứng MÂU THUẪN với số cao hơn %.2f%% ≥ %.0f%% — cấm mở mới trên TOÀN hệ thống",
				ErrOpenBlockedByMargin, v.name, high*100, g.cfg.Thresholds.OrangeFrac*100)
		}
	}
	for _, name := range req.Venues {
		v, ok := g.byName[name]
		if !ok {
			return fmt.Errorf("%w: sàn %q không được van ký quỹ theo dõi — không chứng minh được là an toàn", ErrOpenBlockedByMargin, name)
		}
		tier, why := g.tierLocked(v, nowMs)
		switch {
		case tier == MarginTierUnknown:
			return fmt.Errorf("%w: ký quỹ %s KHÔNG BIẾT %s", ErrOpenBlockedByMargin, name, why)
		case tier == MarginTierYellow && req.CrossVenuePerp:
			return fmt.Errorf("%w: %s ở mức vàng (%.2f%% ≥ %.0f%%) — Động cơ 2 không mở thêm trên sàn này",
				ErrOpenBlockedByMargin, name, v.reading.MaintenanceMarginRatioFrac*100, g.cfg.Thresholds.YellowFrac*100)
		}
	}
	return nil
}

// runEmergency closes Engine 2's pairs one at a time while some venue has a
// FRESH reading at or above ReleaseFrac, an untried closable pair is left, and
// the per-tick bound is not reached. Unknown alone never keeps it closing.
func (g *MarginGuard) runEmergency(ctx context.Context) (closed, failed []string) {
	seq := g.currentSeq()
	if stop := g.emergencyShouldStop(seq); stop {
		return nil, nil
	}
	if g.closer == nil {
		g.dispatchOnce("no-closer", seq, g.event(MarginEventNothingToClose, seq, "", "không có bộ đóng nào được cấu hình — chỉ còn chặn mở và báo động", nil))
		return nil, nil
	}
	tried := map[string]bool{}
	for n := 0; n < g.cfg.MaxEmergencyClosesPerTick; n++ {
		if ctx.Err() != nil {
			return closed, failed
		}
		pairs, err := g.closer.OpenPairs(ctx)
		if err != nil {
			g.dispatch([]MarginEvent{g.event(MarginEventPairCloseFailed, seq, "", "không liệt kê được các cặp Động cơ 2 đang mở", err)})
			return closed, failed
		}
		var candidates []ExposedPair
		for _, p := range pairs {
			switch {
			case tried[p.PairID]:
			case p.CloseBlockedVI != "":
				g.dispatchOnce("blocked:"+p.PairID, seq, g.event(MarginEventCloseBlocked, seq, p.PairID,
					fmt.Sprintf("KHÔNG đóng tự động %s (%s): %s — người vận hành xử lý", p.PairID, p.Symbol, p.CloseBlockedVI), nil))
			default:
				candidates = append(candidates, p)
			}
		}
		if len(candidates) == 0 {
			msg := "không còn cặp Động cơ 2 nào đóng được mà ký quỹ vẫn chưa về dưới ngưỡng nhả — phần còn lại do vị thế khác giữ; người vận hành phải xử lý"
			if len(tried) > 0 {
				msg = fmt.Sprintf("đã thử đóng %d cặp trong nhịp này — nhịp sau thử lại", len(tried))
			}
			g.dispatchOnce("nothing:"+msg, seq, g.event(MarginEventNothingToClose, seq, "", msg, nil))
			return closed, failed
		}
		ranked, venueOrder := g.rankForMargin(candidates)
		target := ranked[0]
		stressed := stressedVenueOf(target, venueOrder)
		tried[target.PairID] = true
		firstVI := "chân lớn hơn trước"
		if stressed != "" {
			firstVI = "chân trên " + stressed + " trước"
		}
		g.dispatch([]MarginEvent{g.event(MarginEventPairClosing, seq, target.PairID,
			fmt.Sprintf("đóng khẩn cấp %s (%s), %s — phơi nhiễm %s", target.PairID, target.Symbol, firstVI, exposureVI(target)), nil)})

		cctx, cancel := context.WithTimeout(ctx, g.cfg.CloseTimeout)
		err = g.closer.CloseForMargin(cctx, target.PairID, stressed)
		cancel()
		if err != nil {
			failed = append(failed, target.PairID)
			g.dispatch([]MarginEvent{g.event(MarginEventPairCloseFailed, seq, target.PairID, "đóng khẩn cấp HỎNG — chuyển sang cặp kế tiếp", err)})
		} else {
			closed = append(closed, target.PairID)
			g.dispatch([]MarginEvent{g.event(MarginEventPairClosed, seq, target.PairID, "đã đóng phẳng cả hai chân", nil)})
		}

		g.readAll(ctx)
		if g.emergencyShouldStop(seq) {
			return closed, failed
		}
	}
	return closed, failed
}

// emergencyShouldStop reports whether closing must stop now: every venue reads
// fresh and below release, or no venue has FRESH evidence at or above release —
// in which case the only reason left to close is a venue nobody can read.
func (g *MarginGuard) emergencyShouldStop(seq uint64) bool {
	if g.belowReleaseEverywhere() {
		g.noteBelowRelease(seq)
		return true
	}
	g.mu.Lock()
	nowMs := g.cfg.Now().UnixMilli()
	above := false
	var states []string
	for _, v := range g.venues {
		ratio, _, ok := g.freshRatioLocked(v, nowMs)
		switch {
		case !ok:
			states = append(states, v.name+" KHÔNG BIẾT")
		case ratio >= g.cfg.Thresholds.ReleaseFrac:
			above = true
		case v.err != nil && v.hasConflict:
			states = append(states, fmt.Sprintf("%s MÂU THUẪN (công bố %.2f%%, suy ra %.2f%% — số thấp dưới ngưỡng)",
				v.name, v.conflict.PublishedRatioFrac*100, v.conflict.DerivedRatioFrac*100))
		default:
			states = append(states, fmt.Sprintf("%s %.2f%%", v.name, ratio*100))
		}
	}
	g.mu.Unlock()
	if above {
		return false
	}
	g.dispatchOnce("unknown", seq, g.event(MarginEventStoppedOnUnknown, seq, "",
		fmt.Sprintf("DỪNG đóng: không sàn nào có bằng chứng mới mà số THẤP vẫn ≥ %.0f%% — %s — không trả phí vòng khứ hồi vì một số đọc không tồn tại hoặc tự mâu thuẫn; lệnh mở vẫn bị chặn",
			g.cfg.Thresholds.ReleaseFrac*100, strings.Join(states, "; ")), nil))
	return true
}

// rankForMargin orders pairs by their exposure on the venue with the HIGHEST
// ratio, then on the next venue, then by id so the order is deterministic, and
// returns the venues in that order. Exposure on the stressed venue is what frees
// its margin; the design's "highest leverage / largest drawdown" is not a
// per-pair figure any venue publishes here. A conflict is ranked on its HIGHER
// figure: choosing which venue to relieve first is a decision that errs high
// (review 4.5k, m-b).
func (g *MarginGuard) rankForMargin(pairs []ExposedPair) ([]ExposedPair, []string) {
	g.mu.Lock()
	type venueRatio struct {
		name  string
		ratio float64
	}
	nowMs := g.cfg.Now().UnixMilli()
	var order []venueRatio
	for _, v := range g.venues {
		ratio, ok := g.freshHighRatioLocked(v, nowMs)
		if !ok {
			ratio = -1 // no fresh evidence ranks last
		}
		order = append(order, venueRatio{v.name, ratio})
	}
	g.mu.Unlock()
	sort.SliceStable(order, func(i, j int) bool { return order[i].ratio > order[j].ratio })

	out := append([]ExposedPair(nil), pairs...)
	sort.SliceStable(out, func(i, j int) bool {
		for _, v := range order {
			a, b := out[i].ExposureQuoteByVenue[v.name], out[j].ExposureQuoteByVenue[v.name]
			if a != b {
				return a > b
			}
		}
		return out[i].PairID < out[j].PairID
	})
	names := make([]string, len(order))
	for i, v := range order {
		names[i] = v.name
	}
	return out, names
}

// stressedVenueOf is the most stressed venue among the pair's OWN legs. The
// guard's most stressed venue overall may hold no leg of this pair — with three
// venues, a close told to start on a venue that is not one of its legs is refused
// (review 4.5k, m-c). "" when no leg's venue is known to the guard.
func stressedVenueOf(p ExposedPair, venueOrder []string) string {
	for _, name := range venueOrder {
		if _, leg := p.ExposureQuoteByVenue[name]; leg {
			return name
		}
	}
	return ""
}

func exposureVI(p ExposedPair) string {
	names := make([]string, 0, len(p.ExposureQuoteByVenue))
	for name := range p.ExposureQuoteByVenue {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s %.2f", name, p.ExposureQuoteByVenue[name]))
	}
	return strings.Join(parts, ", ") + " (" + p.ExposureBasisVI + ")"
}

// belowReleaseEverywhere reports whether EVERY venue has a fresh successful
// reading under ReleaseFrac. A venue that cannot be read is not below anything.
func (g *MarginGuard) belowReleaseEverywhere() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.belowReleaseEverywhereLocked(g.cfg.Now().UnixMilli())
}

// belowReleaseEverywhereLocked: every venue has FRESH evidence, and all of it —
// both figures of a conflict — is below ReleaseFrac. A conflict whose two
// figures agree on that is not a reason to keep a latch nobody can clear (review
// 4.5k, N6).
func (g *MarginGuard) belowReleaseEverywhereLocked(nowMs int64) bool {
	for _, v := range g.venues {
		high, ok := g.freshHighRatioLocked(v, nowMs)
		if !ok || !(high < g.cfg.Thresholds.ReleaseFrac) {
			return false
		}
	}
	return true
}

func (g *MarginGuard) noteBelowRelease(seq uint64) {
	g.mu.Lock()
	first := g.belowReleaseSeen != seq
	g.belowReleaseSeen = seq
	g.mu.Unlock()
	if first {
		g.dispatch([]MarginEvent{g.event(MarginEventBelowRelease, seq, "",
			fmt.Sprintf("mọi sàn đã dưới %.0f%% — ngừng đóng; lệnh mở vẫn bị chặn tới khi người vận hành xác nhận #%d",
				g.cfg.Thresholds.ReleaseFrac*100, seq), nil)})
	}
}

func (g *MarginGuard) currentSeq() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.emergencySeq
}

// Acknowledge clears the red latch the operator was shown, and only that one,
// and only once every venue reads below ReleaseFrac.
func (g *MarginGuard) Acknowledge(seenSeq uint64) error {
	g.mu.Lock()
	nowMs := g.cfg.Now().UnixMilli()
	switch {
	case !g.emergency:
		g.mu.Unlock()
		return ErrNothingToAcknowledge
	case seenSeq != g.emergencySeq:
		seq := g.emergencySeq
		g.mu.Unlock()
		return fmt.Errorf("risk: xác nhận van đỏ #%d nhưng van đang chốt là #%d — tải lại trang rồi xác nhận đúng cái đang thấy", seenSeq, seq)
	case !g.belowReleaseEverywhereLocked(nowMs):
		var parts []string
		for _, v := range g.venues {
			tier, why := g.tierLocked(v, nowMs)
			if high, ok := g.freshHighRatioLocked(v, nowMs); ok && v.err != nil && v.hasConflict {
				why = fmt.Sprintf("(mâu thuẫn, số cao %.2f%%)", high*100)
			}
			parts = append(parts, fmt.Sprintf("%s %s %s", v.name, tier, why))
		}
		g.mu.Unlock()
		return fmt.Errorf("risk: chưa xác nhận được van đỏ #%d — chưa phải mọi sàn đều đọc được và dưới %.0f%%: %s",
			seenSeq, g.cfg.Thresholds.ReleaseFrac*100, strings.Join(parts, "; "))
	}
	g.emergency = false
	g.mu.Unlock()
	g.dispatch([]MarginEvent{g.event(MarginEventAcknowledged, seenSeq, "", "người vận hành đã xác nhận van đỏ — lệnh mở được phép trở lại theo từng mức", nil)})
	return nil
}

// MarginVenueView is one venue as Snapshot shows it.
type MarginVenueView struct {
	Venue      string
	Tier       MarginTier
	RatioFrac  float64
	HasReading bool
	ReadAtMs   int64
	AgeMs      int64
	SourceVI   string
	ProblemVI  string
}

// MarginSnapshot is the guard's state for a page or a log line.
type MarginSnapshot struct {
	AtMs         int64
	Venues       []MarginVenueView
	Emergency    bool
	EmergencySeq uint64
	RecentEvents []MarginEvent
}

// Snapshot copies the guard's state.
func (g *MarginGuard) Snapshot() MarginSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	nowMs := g.cfg.Now().UnixMilli()
	out := MarginSnapshot{AtMs: nowMs, Emergency: g.emergency, EmergencySeq: g.emergencySeq,
		RecentEvents: append([]MarginEvent(nil), g.recent...)}
	for _, v := range g.venues {
		tier, why := g.tierLocked(v, nowMs)
		view := MarginVenueView{Venue: v.name, Tier: tier, HasReading: v.hasReading}
		if v.hasReading {
			view.RatioFrac, view.ReadAtMs, view.AgeMs, view.SourceVI =
				v.reading.MaintenanceMarginRatioFrac, v.reading.ReadAtMs, nowMs-v.reading.ReadAtMs, v.reading.SourceVI
		}
		if tier == MarginTierUnknown {
			view.ProblemVI = why
		}
		out.Venues = append(out.Venues, view)
	}
	return out
}

func (g *MarginGuard) event(kind MarginEventKind, seq uint64, pairID, messageVI string, err error) MarginEvent {
	return MarginEvent{AtMs: g.cfg.Now().UnixMilli(), Kind: kind, PairID: pairID, EmergencySeq: seq, MessageVI: messageVI, Err: err}
}

// dispatchOnce dispatches an event at most once per key per emergency sequence,
// so a latched emergency does not flood the recent-event list every tick and
// push out the event that started it (review 4.5k, m8).
func (g *MarginGuard) dispatchOnce(key string, seq uint64, ev MarginEvent) {
	g.mu.Lock()
	if last, seen := g.reported[key]; seen && last == seq {
		g.mu.Unlock()
		return
	}
	g.reported[key] = seq
	g.mu.Unlock()
	g.dispatch([]MarginEvent{ev})
}

// dispatch records events and hands them to OnEvent with no lock held.
func (g *MarginGuard) dispatch(events []MarginEvent) {
	if len(events) == 0 {
		return
	}
	g.mu.Lock()
	g.recent = append(g.recent, events...)
	if over := len(g.recent) - recentEventsKept; over > 0 {
		g.recent = append([]MarginEvent(nil), g.recent[over:]...)
	}
	g.mu.Unlock()
	if g.cfg.OnEvent != nil {
		for _, ev := range events {
			g.cfg.OnEvent(ev)
		}
	}
}
