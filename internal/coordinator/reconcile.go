package coordinator

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"

	"futures-arbitrage-scanner/internal/broker"
)

// PositionShape is what a symbol's perp positions across the venues look like.
type PositionShape string

const (
	// ShapeFlat: every venue reads exactly zero.
	ShapeFlat PositionShape = "flat"
	// ShapeShortOnly: every non-flat venue is SHORT — Engine 1's perp leg,
	// which is always short.
	ShapeShortOnly PositionShape = "short_only"
	// ShapeCrossPair: exactly one venue long and exactly one venue short, of the
	// same size within Config.MaxCrossImbalanceFrac — Engine 2's shape.
	ShapeCrossPair PositionShape = "cross_long_short"
	// ShapeCrossUnbalanced: one long and one short further apart than that. It is
	// still ENGINE 2's shape — Engine 1 never holds a long perp — so it is
	// attributed to Engine 2 with the imbalance written into the lock, and whether
	// it is a hedge is Engine 2's judgement on the venues' own grids (Engine.Adopt
	// records a pair that is not as unresolved, with both legs' real sizes, where
	// the margin guard can close it). A conflict here made a hedge the executor
	// keeps — 0.049 against 0.05 on a 0.001 grid — unclosable after a restart
	// (review 4.5k, N4).
	ShapeCrossUnbalanced PositionShape = "cross_unbalanced"
	// ShapeUnattributable: a long with no short beside it, two longs, or more
	// than one of either. No engine's position looks like this.
	ShapeUnattributable PositionShape = "unattributable"
)

// classifyShape reads a shape off fully readable evidence.
func classifyShape(ev []VenuePosition, maxCrossImbalanceFrac float64) (PositionShape, []string) {
	var longs, shorts, nonFlat []string
	longQty, shortQty := 0.0, 0.0
	for _, v := range ev {
		switch {
		case v.QtyCoin > 0:
			longs = append(longs, v.Venue)
			nonFlat = append(nonFlat, v.Venue)
			longQty = v.QtyCoin
		case v.QtyCoin < 0:
			shorts = append(shorts, v.Venue)
			nonFlat = append(nonFlat, v.Venue)
			shortQty = -v.QtyCoin
		}
	}
	switch {
	case len(nonFlat) == 0:
		return ShapeFlat, nil
	case len(longs) == 0:
		return ShapeShortOnly, nonFlat
	case len(longs) == 1 && len(shorts) == 1:
		if math.Abs(longQty-shortQty) > maxCrossImbalanceFrac*math.Max(longQty, shortQty) {
			return ShapeCrossUnbalanced, nonFlat
		}
		return ShapeCrossPair, nonFlat
	}
	return ShapeUnattributable, nonFlat
}

// ownerOfShape is the one engine whose positions have this shape, or "".
func ownerOfShape(shape PositionShape) EngineID {
	switch shape {
	case ShapeShortOnly:
		return EngineCashAndCarry
	case ShapeCrossPair, ShapeCrossUnbalanced:
		return EngineCrossPerp
	}
	return ""
}

// shapeNoteVI is what a lock's details say about a shape beyond its owner.
func shapeNoteVI(shape PositionShape) string {
	if shape == ShapeCrossUnbalanced {
		return " — hai chân LỆCH nhau quá ngưỡng báo cáo: có là cặp phòng hộ hay không do Động cơ 2 phán xử trên lưới của hai sàn khi nhận (Adopt)"
	}
	return ""
}

// shapeConsistent reports whether a live shape is one the lock's owner can hold.
// Flat is consistent with any owner: a closed position waiting for its engine's
// Release. A cross pair must also sit on the lock's own two venues.
func shapeConsistent(l SymbolLock, shape PositionShape, nonFlat []string) bool {
	if shape == ShapeFlat {
		return true
	}
	if ownerOfShape(shape) != l.OwnerEngine {
		return false
	}
	if l.OwnerEngine == EngineCrossPerp && len(l.Venues) > 0 {
		inLock := map[string]bool{}
		for _, v := range l.Venues {
			inLock[v] = true
		}
		for _, v := range nonFlat {
			if !inLock[v] {
				return false
			}
		}
	}
	return true
}

// SymbolReconcile is what one reconcile saw and did for one symbol.
type SymbolReconcile struct {
	Symbol   string
	Evidence []VenuePosition
	Shape    PositionShape
	Before   SymbolLock
	After    SymbolLock
	ActionVI string
}

// ReconcileReport is one reconcile's account, symbol by symbol.
type ReconcileReport struct {
	AtMs       int64
	Symbols    []SymbolReconcile
	Occupied   []string
	Conflicts  []string
	Unverified []string
}

// ReconcileActivePositions reads every venue's perp position for every symbol
// in the universe, and every venue's resting perp orders, and brings the lock
// table into line with what the VENUES hold (rule 7). It is what makes a lost or
// corrupt lock file safe: until it has run once, nothing is granted.
//
// Per symbol:
//
//   - a venue that cannot be read leaves the symbol UNVERIFIED — never granted
//     until a later reconcile reads it — and its lock, if any, untouched;
//   - flat venues with an order still resting and no lock: UNVERIFIED too —
//     somebody is about to hold a position there;
//   - flat venues, nothing resting, no lock: idle;
//   - flat venues under a lock an engine ACQUIRED: the lock stays; its owner
//     releases it, and Release reads the venues again — that engine may have
//     crashed between taking the lock and sending, and only it can ask the
//     venue about its derived order ids;
//   - flat venues, nothing resting, under a lock that was only ever INFERRED
//     from a position and never adopted: released — the position that was its
//     only basis is gone and nobody holds the intent;
//   - a live position with no lock: occupied by the one engine whose shape it
//     is (SourceReconciledInferred, no intent id), or a conflict when no engine's
//     shape explains it;
//   - a live position under a lock whose owner cannot hold that shape: a
//     conflict, with both pieces of evidence written into it — unless the owner
//     is live in this process and may be mid-operation (doc.go), in which case
//     the report says so and nothing changes.
func (c *Coordinator) ReconcileActivePositions(ctx context.Context) (ReconcileReport, error) {
	c.mu.Lock()
	symbols := make([]string, 0, len(c.universe))
	for s := range c.universe {
		symbols = append(symbols, s)
	}
	versions := map[string]uint64{}
	for s, e := range c.locks {
		versions[s] = e.version
	}
	c.mu.Unlock()
	sort.Strings(symbols)

	evidence, err := c.readUniverse(ctx, symbols)
	if err != nil {
		return ReconcileReport{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.cfg.Now().UnixMilli()
	report := ReconcileReport{AtMs: now}
	changed := false
	for _, s := range symbols {
		ev := evidence[s]
		evVI := evidenceVI(ev)
		row := SymbolReconcile{Symbol: s, Evidence: ev, Before: SymbolLock{Symbol: s, State: StateIdle}}
		cur := c.locks[s]
		if cur != nil {
			row.Before = cur.lock.clone()
		}
		finish := func(action string) {
			row.ActionVI = action
			if e := c.locks[s]; e != nil {
				row.After = e.lock.clone()
				if e.lock.State == StateConflict {
					report.Conflicts = append(report.Conflicts, s)
				} else {
					report.Occupied = append(report.Occupied, s)
				}
			} else {
				row.After = SymbolLock{Symbol: s, State: StateIdle}
			}
			report.Symbols = append(report.Symbols, row)
		}

		before, had := versions[s]
		if (cur != nil) != had || cur != nil && cur.version != before {
			finish("khóa đổi trong lúc đọc sàn — để lần đối soát sau phán xử")
			continue
		}
		unreadable, resting := false, 0
		for _, v := range ev {
			unreadable = unreadable || !v.Read
			resting += v.OpenOrders
		}
		if unreadable {
			c.unverified[s] = "lần đối soát gần nhất không đọc được mọi sàn: " + evVI
			report.Unverified = append(report.Unverified, s)
			finish("CHƯA XÁC MINH — không cấp khóa cho tới khi đọc được mọi sàn: " + evVI)
			continue
		}

		shape, nonFlat := classifyShape(ev, c.cfg.MaxCrossImbalanceFrac)
		row.Shape = shape
		if cur == nil && shape == ShapeFlat && resting > 0 {
			c.unverified[s] = "vị thế phẳng nhưng còn lệnh đang treo — chưa rõ của ai: " + evVI
			report.Unverified = append(report.Unverified, s)
			finish("CHƯA XÁC MINH — " + c.unverified[s])
			continue
		}
		delete(c.unverified, s)

		switch {
		case cur == nil && shape == ShapeFlat:
			finish("mọi sàn phẳng, không lệnh treo — rảnh")

		case cur == nil:
			c.versions++
			e := &entry{version: c.versions, lock: SymbolLock{
				Symbol: s, LockedAtMs: now, Venues: nonFlat, Source: SourceReconciledInferred,
				EvidenceVI: evVI, EvidenceAtMs: now,
			}}
			if owner := ownerOfShape(shape); owner != "" {
				e.lock.State, e.lock.OwnerEngine = StateOccupied, owner
				e.lock.Details = fmt.Sprintf("không có bản ghi khóa; SUY RA từ hình dạng vị thế sàn (%s) — động cơ sở hữu phải nhận (Adopt) trước khi thao tác%s", shape, shapeNoteVI(shape))
			} else {
				e.lock.State = StateConflict
				e.lock.Details = fmt.Sprintf("không có bản ghi khóa và không động cơ nào giữ vị thế hình dạng %s — người vận hành xử lý", shape)
			}
			c.locks[s] = e
			changed = true
			finish(e.lock.Details)

		case cur.lock.State == StateConflict:
			cur.lock.EvidenceVI, cur.lock.EvidenceAtMs = evVI, now
			changed = true
			finish("vẫn XUNG ĐỘT — bằng chứng mới: " + evVI)

		case shape == ShapeFlat && resting == 0 && cur.lock.Source == SourceReconciledInferred && cur.lock.IntentID == "" && !cur.liveOwner:
			delete(c.locks, s)
			changed = true
			finish(fmt.Sprintf("vị thế đã SUY RA cho %s đã hết và không lệnh nào treo — nhả khóa suy ra", cur.lock.OwnerEngine))

		case shapeConsistent(cur.lock, shape, nonFlat):
			cur.lock.EvidenceVI, cur.lock.EvidenceAtMs = evVI, now
			changed = true
			if shape == ShapeFlat {
				finish(fmt.Sprintf("mọi sàn phẳng dưới khóa của %s — khóa giữ tới khi chủ khóa Release (Release đọc lại sàn)", cur.lock.OwnerEngine))
			} else {
				finish(fmt.Sprintf("khớp chủ khóa %s (%s)%s", cur.lock.OwnerEngine, shape, shapeNoteVI(shape)))
			}

		case cur.liveOwner:
			cur.lock.EvidenceVI, cur.lock.EvidenceAtMs = evVI, now
			changed = true
			finish(fmt.Sprintf("KHÔNG KHỚP: %s không giữ hình dạng %s — nhưng chủ khóa đang sống trong tiến trình này và có thể đang giữa thao tác; báo cáo, không đổi", cur.lock.OwnerEngine, shape))

		default:
			details := fmt.Sprintf("bản ghi khóa (%s) nói %s giữ %s (ý định %q, sàn %v) nhưng sàn nói %s: %s",
				cur.lock.Source, cur.lock.OwnerEngine, s, cur.lock.IntentID, cur.lock.Venues, shape, evVI)
			cur.lock.State, cur.lock.Details = StateConflict, details
			cur.lock.EvidenceVI, cur.lock.EvidenceAtMs = evVI, now
			c.versions++
			cur.version = c.versions
			changed = true
			finish("→ XUNG ĐỘT: " + details)
		}
	}
	c.reconciledAtMs = now
	if changed {
		if err := c.persistLocked(); err != nil {
			// Memory holds at least every lock the disk does; the next write,
			// or the next reconcile after a restart, catches the file up.
			return report, fmt.Errorf("%w: %v", ErrPersist, err)
		}
	}
	return report, nil
}

// readUniverse reads, per venue, every resting perp order ONCE (an empty symbol
// lists them all — one request instead of one per symbol) and then every
// symbol's position. No lock is held.
func (c *Coordinator) readUniverse(ctx context.Context, symbols []string) (map[string][]VenuePosition, error) {
	type venueOrders struct {
		bySymbol map[string]int
		errVI    string
	}
	orders := make([]venueOrders, len(c.cfg.Venues))
	var wg sync.WaitGroup
	for i, v := range c.cfg.Venues {
		wg.Add(1)
		go func(i int, v Venue) {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, c.cfg.ReadTimeout)
			defer cancel()
			list, err := v.Reader.OpenOrders(rctx, broker.MarketFuturesUSDM, "")
			if err != nil {
				orders[i] = venueOrders{errVI: "lệnh đang treo: " + err.Error()}
				return
			}
			counts := map[string]int{}
			for _, o := range list {
				if !o.Status.Done() {
					counts[o.Symbol]++
				}
			}
			orders[i] = venueOrders{bySymbol: counts}
		}(i, v)
	}
	wg.Wait()

	out := make(map[string][]VenuePosition, len(symbols))
	for _, s := range symbols {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row := make([]VenuePosition, len(c.cfg.Venues))
		for i, v := range c.cfg.Venues {
			wg.Add(1)
			go func(i int, v Venue) {
				defer wg.Done()
				rctx, cancel := context.WithTimeout(ctx, c.cfg.ReadTimeout)
				defer cancel()
				row[i] = readPosition(rctx, v, s)
				if !row[i].Read {
					return
				}
				if orders[i].errVI != "" {
					row[i].Read, row[i].ErrVI = false, orders[i].errVI
					return
				}
				row[i].OpenOrders = orders[i].bySymbol[s]
			}(i, v)
		}
		wg.Wait()
		out[s] = row
	}
	return out, nil
}
