package crossperp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"futures-arbitrage-scanner/internal/coordinator"
	"futures-arbitrage-scanner/internal/execution"
	"futures-arbitrage-scanner/internal/risk"
)

// Engine is Engine 2 as the coordinator and the margin guard see it. It takes
// the symbol's lock before opening, releases it only when the VENUES read flat,
// keeps the pairs it holds — including the ones it could not resolve — and
// closes them for the margin guard. It decides nothing: no signal reaches it.
//
// One operation per symbol at a time: an emergency close waits for an open in
// flight on that symbol to finish, rather than sending a second set of orders
// beside it.
type Engine struct {
	exec  *Executor
	coord *coordinator.Coordinator

	mu    sync.Mutex
	pairs map[string]*Pair         // by symbol
	busy  map[string]chan struct{} // by symbol, closed when the operation ends

	// held are symbols whose lock this engine keeps although the venues were
	// PROVEN flat and quiet, every opening order proven finished and every
	// reduce-only order proven finished or unseen past the quiet period: only the
	// release itself failed (a venue unreadable at that moment, a lock file that
	// would not write). RetryReleases retries them. Anything that may still hold
	// or fill something is a Pair instead, where the margin guard sees it (review
	// 4.5k, N3).
	held map[string]heldLock
}

type heldLock struct {
	intentID string
	err      error

	// withdraw: nothing was ever sent under the lock, and giving it back failed;
	// it is retried as a Withdraw, not as a Release that needs flat venues
	// (review 4.5k round 3, minor 8).
	withdraw bool
}

// Pair is one Engine-2 position as this process knows it — a CACHE of what the
// venues hold (rule 7); Close reads the venues again.
type Pair struct {
	IntentID string
	Symbol   string
	Long     LegSpec
	Short    LegSpec

	// The two legs' sizes, as magnitudes, from the venues when they were read.
	// Unequal only on an unresolved pair.
	LongQtyCoin  float64
	ShortQtyCoin float64

	LongAvgFillPriceQuote  float64
	ShortAvgFillPriceQuote float64
	OpenedAtMs             int64

	// Adopted: rebuilt after a restart from the venues, with no fill prices.
	Adopted bool

	// Unresolved: the open or a close ended loud; the venues may hold legs that
	// are not a hedge. The guard may still close it, and sees it at the sizes the
	// venues hold when it asks (OpenPairs).
	Unresolved bool

	// OpeningOrdersUnproven: an opening order of this intent was never proven
	// finished, so it may still fill — a send whose answer was lost, a cancel
	// never read back, or a lock recovered from a file under which orders had
	// been sent (review 4.5k, N2/N3). Close cancels both by their derived ids,
	// reads them back and closes what they filled, and stays loud until the venue
	// proves them finished or a person says so (ConfirmOrdersFinished).
	OpeningOrdersUnproven bool

	// PendingOrders are reduce-only orders not proven finished (PendingOrder):
	// they reduce whatever the venue holds when they execute, so the lock stays
	// until a Close proves them finished or absent, or a person says so (review
	// 4.5k round 3, M1).
	PendingOrders []PendingOrder

	// CloseBlockedVI is set when the evidence about this pair CONFLICTS: the
	// orders and the venues' positions disagree, and a close sized from a
	// position that may be wrong can itself open a naked leg. Nothing closes it
	// automatically; UnblockClose is a person's decision.
	CloseBlockedVI string
}

// The engine's refusals.
var (
	ErrUnknownPair  = errors.New("crossperp: Động cơ 2 không giữ cặp này")
	ErrCloseBlocked = errors.New("crossperp: cặp đang bị CHẶN ĐÓNG vì bằng chứng mâu thuẫn — người vận hành xử lý rồi UnblockClose")
	ErrPairBusy     = errors.New("crossperp: symbol đã có cặp được ghi nhận")
)

// NewEngine wires Engine 2 to its executor and the coordinator.
func NewEngine(exec *Executor, coord *coordinator.Coordinator) (*Engine, error) {
	if exec == nil || coord == nil {
		return nil, errors.New("crossperp: an engine needs both an executor and a coordinator")
	}
	return &Engine{exec: exec, coord: coord, pairs: map[string]*Pair{}, busy: map[string]chan struct{}{},
		held: map[string]heldLock{}}, nil
}

var _ risk.EmergencyCloser = (*Engine)(nil)

// OpenRequest is one intent with the figure it competes for the symbol with.
type OpenRequest struct {
	Intent Intent

	// PriorityAPROnCapitalFrac and PriorityAPRBasisVI go to the coordinator's
	// contest unchanged (coordinator.AcquireRequest).
	PriorityAPROnCapitalFrac float64
	PriorityAPRBasisVI       string
	DetailsVI                string
}

// Open takes the lock, opens, and gives the lock back only when nothing of the
// intent can be on the venues.
func (g *Engine) Open(ctx context.Context, req OpenRequest) (Result, error) {
	intent := req.Intent
	base := Result{IntentID: intent.ID, Symbol: intent.Symbol, Outcome: OutcomeBothFlat}
	dec, err := g.coord.TryAcquire(ctx, coordinator.AcquireRequest{
		Symbol: intent.Symbol, Engine: coordinator.EngineCrossPerp, IntentID: intent.ID, Details: req.DetailsVI,
		Venues:                   []string{intent.Long.Venue.Name, intent.Short.Venue.Name},
		PriorityAPROnCapitalFrac: req.PriorityAPROnCapitalFrac, PriorityAPRBasisVI: req.PriorityAPRBasisVI,
	})
	if err != nil {
		// Refused at the gate: no intent formed, nothing sent.
		base.ReasonVI = dec.ReasonVI
		return base, err
	}
	if err := g.begin(ctx, intent.Symbol); err != nil {
		return base, errors.Join(err, g.coord.Withdraw(intent.Symbol, coordinator.EngineCrossPerp, intent.ID))
	}
	defer g.end(intent.Symbol)

	res, openErr := g.exec.Open(ctx, intent)
	switch {
	case errors.Is(openErr, ErrRefusedBeforePlacing):
		// NOTHING was sent under this lock, so what the venues hold is not this
		// intent's: the lock goes back without a venue proof. Keeping it because
		// Engine 1 holds the symbol would lock both engines out of it for good
		// (review 4.5k, M6); the next reconcile attributes what is there. The
		// coordinator itself refuses once the executor marked orders as sent.
		if wErr := g.coord.Withdraw(intent.Symbol, coordinator.EngineCrossPerp, intent.ID); wErr != nil {
			g.holdWithdraw(intent.Symbol, intent.ID, wErr)
			return res, errors.Join(openErr, wErr)
		}
		return res, openErr

	case res.Outcome == OutcomeBothOpen:
		g.recordPair(intent, res, isLoud(openErr), "")
		return res, openErr

	case res.Outcome == OutcomeUnresolved || isLoud(openErr):
		// Nothing opens beside a state nobody can name: the lock stays, and the
		// pair is recorded — flat or not — so the guard sees what the venues hold
		// and a close can still cancel an opening order that may fill (review
		// 4.5k, M4 and N3). A pair whose evidence itself conflicts is not closed
		// automatically.
		blocked := ""
		if errors.Is(openErr, ErrFillEvidenceConflict) {
			blocked = openErr.Error()
		}
		g.recordPair(intent, res, true, blocked)
		return res, openErr
	}
	if rep, relErr := g.coord.Release(context.WithoutCancel(ctx), intent.Symbol, coordinator.EngineCrossPerp, intent.ID); relErr != nil {
		g.hold(intent.Symbol, intent.ID, relErr)
		return res, errors.Join(openErr, fmt.Errorf("khóa %s được GIỮ, sẽ nhả lại ở RetryReleases: %w (%s)", intent.Symbol, relErr, rep.EvidenceVI))
	}
	return res, openErr
}

// recordPair keeps what the venues hold after an open. Sizes come from the
// venues' positions when they were read, and from the orders otherwise — as
// MAGNITUDES, so a leg on the wrong side is still counted as exposure (review
// 4.5k, m-f).
func (g *Engine) recordPair(intent Intent, res Result, unresolved bool, blockedVI string) {
	longQty := math.Max(0, res.Long.FilledQtyCoin-res.Long.ClosedQtyCoin)
	shortQty := math.Max(0, res.Short.FilledQtyCoin-res.Short.ClosedQtyCoin)
	if res.Long.VenuePositionRead {
		longQty = math.Abs(res.Long.VenuePositionQtyCoin)
	}
	if res.Short.VenuePositionRead {
		shortQty = math.Abs(res.Short.VenuePositionQtyCoin)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pairs[intent.Symbol] = &Pair{
		IntentID: intent.ID, Symbol: intent.Symbol, Long: intent.Long, Short: intent.Short,
		LongQtyCoin: longQty, ShortQtyCoin: shortQty,
		LongAvgFillPriceQuote: res.Long.AvgFillPriceQuote, ShortAvgFillPriceQuote: res.Short.AvgFillPriceQuote,
		OpenedAtMs: g.exec.cfg.Now().UnixMilli(), Unresolved: unresolved, CloseBlockedVI: blockedVI,
		OpeningOrdersUnproven: !res.Long.OrderConfirmed || !res.Short.OrderConfirmed,
		PendingOrders:         append([]PendingOrder(nil), res.PendingOrders...),
	}
}

// Close closes the pair on symbol and releases its lock once the venues read
// flat and quiet. firstVenue names the venue whose leg closes first ("" for the
// larger).
func (g *Engine) Close(ctx context.Context, symbol, firstVenue, reasonVI string) (CloseResult, error) {
	g.mu.Lock()
	p := g.pairs[symbol]
	var snapshot Pair
	if p != nil {
		snapshot = *p
	}
	g.mu.Unlock()
	if p == nil {
		return CloseResult{Symbol: symbol, Outcome: OutcomeUnresolved}, fmt.Errorf("%w: %s", ErrUnknownPair, symbol)
	}
	if snapshot.CloseBlockedVI != "" {
		return CloseResult{IntentID: snapshot.IntentID, Symbol: symbol, Outcome: OutcomeUnresolved},
			fmt.Errorf("%w: %s — %s", ErrCloseBlocked, symbol, snapshot.CloseBlockedVI)
	}
	if err := g.begin(ctx, symbol); err != nil {
		return CloseResult{IntentID: snapshot.IntentID, Symbol: symbol, Outcome: OutcomeBothOpen}, err
	}
	defer g.end(symbol)
	// The begin above may have waited for an operation that changed the pair.
	g.mu.Lock()
	cur := g.pairs[symbol]
	same := cur != nil && cur.IntentID == snapshot.IntentID
	if same {
		snapshot = *cur
	}
	g.mu.Unlock()
	switch {
	case !same:
		return CloseResult{IntentID: snapshot.IntentID, Symbol: symbol, Outcome: OutcomeUnresolved}, fmt.Errorf("%w: %s đã đổi trong lúc chờ", ErrUnknownPair, symbol)
	case snapshot.CloseBlockedVI != "":
		return CloseResult{IntentID: snapshot.IntentID, Symbol: symbol, Outcome: OutcomeUnresolved},
			fmt.Errorf("%w: %s — %s", ErrCloseBlocked, symbol, snapshot.CloseBlockedVI)
	}

	res, err := g.exec.Close(ctx, CloseRequest{IntentID: snapshot.IntentID, Symbol: symbol,
		Long: snapshot.Long, Short: snapshot.Short, FirstVenue: firstVenue, ReasonVI: reasonVI,
		OpeningOrdersUnproven: snapshot.OpeningOrdersUnproven, PendingOrders: snapshot.PendingOrders})

	if res.Outcome == OutcomeBothFlat && err == nil {
		g.mu.Lock()
		delete(g.pairs, symbol)
		g.mu.Unlock()
		if _, relErr := g.coord.Release(context.WithoutCancel(ctx), symbol, coordinator.EngineCrossPerp, snapshot.IntentID); relErr != nil {
			g.hold(symbol, snapshot.IntentID, relErr)
			return res, fmt.Errorf("cặp %s đã đóng phẳng nhưng khóa được GIỮ, sẽ nhả lại ở RetryReleases: %w", symbol, relErr)
		}
		return res, nil
	}
	// Still something on the venues, or beside them: keep the record at what
	// they hold now, so the guard ranks the pair on its real size (review 4.5k,
	// m6), and say what is not known (review 4.5k, m-f).
	g.mu.Lock()
	if cur := g.pairs[symbol]; cur != nil && cur.IntentID == snapshot.IntentID && res.Outcome != "" {
		if res.OpeningOrdersProven {
			cur.OpeningOrdersUnproven = false
		}
		cur.PendingOrders = append([]PendingOrder(nil), res.PendingOrders...)
		if res.Long.VenuePositionRead {
			cur.LongQtyCoin = math.Abs(res.Long.VenuePositionQtyCoin)
		}
		if res.Short.VenuePositionRead {
			cur.ShortQtyCoin = math.Abs(res.Short.VenuePositionQtyCoin)
		}
		switch {
		case errors.Is(err, ErrPositionDisagrees):
			// A venue holds the wrong side for this pair: evidence that conflicts,
			// which no automatic close may act on.
			cur.Unresolved, cur.CloseBlockedVI = true, err.Error()
		case res.Outcome == OutcomeUnresolved:
			cur.Unresolved = true
		}
	}
	g.mu.Unlock()
	return res, err
}

// Adopt rebuilds the record of a pair the coordinator says Engine 2 holds —
// after a restart — from what the VENUES hold, and binds the lock to intentID.
// The venues must be the lock's own two, and neither may hold the wrong side.
//
// It records what it finds rather than refusing what is not a clean hedge: a
// refused pair is invisible to the margin guard and closable by nobody (review
// 4.5k, N4). So:
//
//   - two legs within hedgedWithin — the executor's own criterion — and nothing
//     resting: a pair;
//   - legs that are not a hedge, or an order still resting beside them:
//     Unresolved, at both legs' real sizes, where the guard sees and can close
//     them (review 4.5k, M7);
//   - both venues flat: a pair of size zero, whose Close releases the lock —
//     Unresolved with its opening orders unproven when orders were sent under the
//     lock, because the process that sent them may have died before they were
//     finished (review 4.5k, N7).
//
// It sends nothing.
func (g *Engine) Adopt(ctx context.Context, intentID string, long, short LegSpec) (Pair, error) {
	symbol := long.Rules.Symbol
	lock, held := g.coord.QueryLock(symbol)
	switch {
	case symbol == "" || short.Rules.Symbol != symbol:
		return Pair{}, fmt.Errorf("%w: hai chân không cùng symbol (%q, %q)", ErrAdoptRefused, symbol, short.Rules.Symbol)
	case long.Venue.Broker == nil || short.Venue.Broker == nil || long.Venue.Name == short.Venue.Name:
		return Pair{}, fmt.Errorf("%w: hai chân phải ở hai sàn có broker (%q, %q)", ErrAdoptRefused, long.Venue.Name, short.Venue.Name)
	case !held || lock.State != coordinator.StateOccupied || lock.OwnerEngine != coordinator.EngineCrossPerp:
		return Pair{}, fmt.Errorf("%w: %w: khóa của %s là %+v", ErrAdoptRefused, ErrUnknownPair, symbol, lock)
	case !sameVenues(lock.Venues, long.Venue.Name, short.Venue.Name):
		return Pair{}, fmt.Errorf("%w: khóa của %s nằm trên %v, yêu cầu nhận trên %s/%s", ErrAdoptRefused, symbol, lock.Venues, long.Venue.Name, short.Venue.Name)
	case !(long.Book.MidPriceQuote > 0) || !(short.Book.MidPriceQuote > 0):
		// With no reference price the pair's exposure is valued at zero — ranked
		// last in an emergency — and no residual can be shown to be below a
		// venue's minimum notional (review 4.5k round 3, minor 3).
		return Pair{}, fmt.Errorf("%w: một chân không có giá tham chiếu (sổ) — không định giá được phơi nhiễm của cặp", ErrAdoptRefused)
	}
	g.mu.Lock()
	_, known := g.pairs[symbol]
	_, busy := g.busy[symbol]
	g.mu.Unlock()
	if known || busy {
		return Pair{}, fmt.Errorf("%w: %w: %s", ErrAdoptRefused, ErrPairBusy, symbol)
	}

	pos := g.exec.readBoth(ctx, symbol, long.Venue, short.Venue, true)
	evidence := pos.evidenceVI(long.Venue.Name, short.Venue.Name)
	switch {
	case !pos.read() || pos.longOrdersErr != nil || pos.shortOrdersErr != nil:
		return Pair{}, fmt.Errorf("%w: không đọc được sàn — %s", ErrAdoptRefused, evidence)
	case pos.longQtyCoin < 0 || pos.shortQtyCoin > 0:
		return Pair{}, fmt.Errorf("%w: %s — sàn chân long giữ short hoặc sàn chân short giữ long; hai chân có bị đảo không?", ErrAdoptRefused, evidence)
	}
	resting := pos.longOrders + pos.shortOrders
	longQty, shortQty := pos.longQtyCoin, -pos.shortQtyCoin
	flat := longQty == 0 && shortQty == 0
	hedged := longQty > 0 && shortQty > 0 &&
		hedgedWithin(long, short, math.Max(long.Rules.StepSizeCoin, short.Rules.StepSizeCoin), longQty, shortQty)
	p := &Pair{IntentID: intentID, Symbol: symbol, Long: long, Short: short,
		LongQtyCoin: longQty, ShortQtyCoin: shortQty, OpenedAtMs: g.exec.cfg.Now().UnixMilli(), Adopted: true}
	switch {
	case flat && resting == 0 && lock.OrdersSentAtMs == 0:
		// Nothing was ever sent under this lock, and nothing is there.
	case hedged && resting == 0:
	default:
		p.Unresolved = !hedged
		p.OpeningOrdersUnproven = resting > 0 || lock.OrdersSentAtMs != 0
	}
	if _, err := g.coord.Adopt(symbol, coordinator.EngineCrossPerp, intentID); err != nil {
		return Pair{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, raced := g.pairs[symbol]; raced {
		return Pair{}, fmt.Errorf("%w: %w: %s", ErrAdoptRefused, ErrPairBusy, symbol)
	}
	g.pairs[symbol] = p
	return *p, nil
}

func sameVenues(lockVenues []string, a, b string) bool {
	if len(lockVenues) != 2 {
		return false
	}
	return lockVenues[0] == a && lockVenues[1] == b || lockVenues[0] == b && lockVenues[1] == a
}

// UnblockClose lifts the close block on a pair whose conflicting evidence a
// person has resolved. It sends nothing.
func (g *Engine) UnblockClose(symbol string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := g.pairs[symbol]
	if p == nil {
		return fmt.Errorf("%w: %s", ErrUnknownPair, symbol)
	}
	p.CloseBlockedVI = ""
	return nil
}

// ConfirmOrdersFinished is a PERSON saying that the pair's opening orders and its
// pending reduce-only orders are finished — read on the venues' own pages, say —
// when no venue answer can prove it: Binance answers "no such order" and Bybit
// "not visible" forever for an order that never arrived, which proves nothing
// either way (review 4.5k, N2), and an order answered "execution status unknown"
// is never taken as absent. It names the intent, so a newer pair on the symbol is
// not vouched for by an older look. It sends nothing; the next Close still reads
// the venues, the resting orders included.
func (g *Engine) ConfirmOrdersFinished(symbol, intentID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := g.pairs[symbol]
	switch {
	case p == nil:
		return fmt.Errorf("%w: %s", ErrUnknownPair, symbol)
	case p.IntentID != intentID:
		return fmt.Errorf("%w: %s giữ ý định %q, xác nhận nêu %q", ErrUnknownPair, symbol, p.IntentID, intentID)
	}
	p.OpeningOrdersUnproven, p.PendingOrders = false, nil
	return nil
}

// RetryReleases retries every lock this engine keeps only because its release
// failed after the venues were proven flat and quiet. Run it on the reconcile
// cadence. It returns what is still held and why.
func (g *Engine) RetryReleases(ctx context.Context) map[string]error {
	g.mu.Lock()
	type job struct {
		symbol, intentID string
		withdraw         bool
	}
	var jobs []job
	for s, h := range g.held {
		if _, busy := g.busy[s]; !busy && g.pairs[s] == nil {
			jobs = append(jobs, job{s, h.intentID, h.withdraw})
		}
	}
	g.mu.Unlock()
	for _, j := range jobs {
		var err error
		if j.withdraw {
			err = g.coord.Withdraw(j.symbol, coordinator.EngineCrossPerp, j.intentID)
		} else {
			_, err = g.coord.Release(ctx, j.symbol, coordinator.EngineCrossPerp, j.intentID)
		}
		switch {
		case err == nil, errors.Is(err, coordinator.ErrNotLocked):
			g.clearHold(j.symbol, j.intentID)
		default:
			g.updateHold(j.symbol, j.intentID, err)
		}
	}
	return g.Held()
}

// Held lists the symbols whose lock this engine keeps with no pair, and why.
func (g *Engine) Held() map[string]error {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]error, len(g.held))
	for s, h := range g.held {
		out[s] = h.err
	}
	return out
}

// Pairs lists the pairs Engine 2 holds, by symbol.
func (g *Engine) Pairs() []Pair {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Pair, 0, len(g.pairs))
	for _, p := range g.pairs {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

// OpenPairs implements risk.EmergencyCloser. Exposure is each leg's quantity at
// its entry fill price — or, with no fill price, at its book's mid — and
// ExposureBasisVI says which. GROSS. A pair that is Unresolved or whose opening
// orders are unproven is read from the VENUES first, because what it holds may
// have changed since it was recorded — an opening order filling late is exactly
// that (review 4.5k, N3); a venue that cannot be read leaves the recorded size
// and says so. A pair whose close is blocked is listed with the reason, so the
// guard can raise it instead of closing it.
func (g *Engine) OpenPairs(ctx context.Context) ([]risk.ExposedPair, error) {
	g.mu.Lock()
	snapshot := make([]Pair, 0, len(g.pairs))
	for _, p := range g.pairs {
		snapshot = append(snapshot, *p)
	}
	g.mu.Unlock()

	out := make([]risk.ExposedPair, len(snapshot))
	var wg sync.WaitGroup
	for i, p := range snapshot {
		if !p.Unresolved && !p.OpeningOrdersUnproven && len(p.PendingOrders) == 0 {
			out[i] = exposedPair(p, p.LongQtyCoin, p.ShortQtyCoin, "cỡ ghi nhận lần cuối đọc sàn")
			continue
		}
		wg.Add(1)
		go func(i int, p Pair) {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, g.exec.cfg.PositionSettleTimeout)
			defer cancel()
			pos := g.exec.readBoth(rctx, p.Symbol, p.Long.Venue, p.Short.Venue, false)
			longQty, shortQty, notes := p.LongQtyCoin, p.ShortQtyCoin, []string{}
			if pos.longErr == nil {
				longQty = math.Abs(pos.longQtyCoin)
			} else {
				notes = append(notes, fmt.Sprintf("%s không đọc được (%v) — dùng cỡ ghi nhận", p.Long.Venue.Name, pos.longErr))
			}
			if pos.shortErr == nil {
				shortQty = math.Abs(pos.shortQtyCoin)
			} else {
				notes = append(notes, fmt.Sprintf("%s không đọc được (%v) — dùng cỡ ghi nhận", p.Short.Venue.Name, pos.shortErr))
			}
			basis := "cỡ đọc sàn ngay lúc này (cặp CHƯA GIẢI QUYẾT)"
			if len(notes) > 0 {
				basis += "; " + strings.Join(notes, "; ")
			}
			out[i] = exposedPair(p, longQty, shortQty, basis)
		}(i, p)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].PairID < out[j].PairID })
	return out, nil
}

func exposedPair(p Pair, longQtyCoin, shortQtyCoin float64, qtyBasisVI string) risk.ExposedPair {
	longPx, shortPx, priceVI := p.LongAvgFillPriceQuote, p.ShortAvgFillPriceQuote, "giá khớp lúc mở (không phải giá mark)"
	if longPx <= 0 || shortPx <= 0 {
		longPx, shortPx, priceVI = p.Long.Book.MidPriceQuote, p.Short.Book.MidPriceQuote, "giá giữa của sổ đã biết (không phải giá mark)"
	}
	return risk.ExposedPair{PairID: p.IntentID, Symbol: p.Symbol, ExposureBasisVI: qtyBasisVI + " × " + priceVI,
		ExposureQuoteByVenue: map[string]float64{p.Long.Venue.Name: longQtyCoin * longPx, p.Short.Venue.Name: shortQtyCoin * shortPx},
		CloseBlockedVI:       p.CloseBlockedVI}
}

// CloseForMargin implements risk.EmergencyCloser: the same Close, one leg at a
// time, the stressed venue's leg first. It returns nil exactly when the venues
// read the pair flat and quiet, every opening order is proven finished and every
// reduce-only order proven finished or unseen past its quiet period, and
// the lock was released; anything short of that is an error the guard reports
// (review 4.5k, N3).
func (g *Engine) CloseForMargin(ctx context.Context, pairID, stressedVenue string) error {
	g.mu.Lock()
	symbol := ""
	for s, p := range g.pairs {
		if p.IntentID == pairID {
			symbol = s
		}
	}
	g.mu.Unlock()
	if symbol == "" {
		return fmt.Errorf("%w: %q", ErrUnknownPair, pairID)
	}
	why := "VAN KÝ QUỸ ĐỎ — đóng khẩn cấp"
	if stressedVenue != "" {
		why += ", sàn " + stressedVenue + " trước"
	}
	res, err := g.Close(ctx, symbol, stressedVenue, why)
	if res.Outcome == OutcomeBothFlat && err == nil {
		return nil
	}
	if err == nil {
		err = fmt.Errorf("cặp %s kết thúc %s", symbol, res.Outcome)
	}
	return err
}

func (g *Engine) begin(ctx context.Context, symbol string) error {
	for {
		g.mu.Lock()
		ch, busy := g.busy[symbol]
		if !busy {
			g.busy[symbol] = make(chan struct{})
			g.mu.Unlock()
			return nil
		}
		g.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return fmt.Errorf("chờ thao tác đang chạy trên %s: %w", symbol, ctx.Err())
		}
	}
}

func (g *Engine) end(symbol string) {
	g.mu.Lock()
	ch := g.busy[symbol]
	delete(g.busy, symbol)
	g.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

func (g *Engine) hold(symbol, intentID string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held[symbol] = heldLock{intentID: intentID, err: err}
}

func (g *Engine) holdWithdraw(symbol, intentID string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held[symbol] = heldLock{intentID: intentID, err: err, withdraw: true}
}

// updateHold and clearHold touch a hold only while it is still the same
// intent's: RetryReleases works without the engine's mutex, and a newer intent on
// the symbol may have been held in the meantime (review 4.5k, m-h).
func (g *Engine) updateHold(symbol, intentID string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if h, ok := g.held[symbol]; ok && h.intentID == intentID {
		h.err = err
		g.held[symbol] = h
	}
}

func (g *Engine) clearHold(symbol, intentID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if h, ok := g.held[symbol]; ok && h.intentID == intentID {
		delete(g.held, symbol)
	}
}

func isLoud(err error) bool {
	return errors.Is(err, execution.ErrUnwindIncomplete) || errors.Is(err, execution.ErrFlatEvidenceConflict)
}
