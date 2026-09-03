package scanner

import (
	"encoding/json"
	"math"
	"testing"

	"futures-arbitrage-scanner/internal/fees"
)

// The registry must not carry its own copy of the fee numbers. Two tables of the
// same fact drift, and the one nobody looks at is the one that goes stale.
func TestSourceRegistry_FeesComeFromTheFeeTable(t *testing.T) {
	for _, meta := range sourceRegistry {
		schedule := fees.For(meta.Source)
		if meta.MakerFeeBps != schedule.MakerFeeBps || meta.TakerFeeBps != schedule.TakerFeeBps {
			t.Errorf("%s registry fees %g/%g differ from the fee table's %g/%g",
				meta.Source, meta.MakerFeeBps, meta.TakerFeeBps,
				schedule.MakerFeeBps, schedule.TakerFeeBps)
		}
		if meta.FeeVerified != schedule.Verified {
			t.Errorf("%s fee_verified = %v, fee table says %v",
				meta.Source, meta.FeeVerified, schedule.Verified)
		}
	}
}

// Every source the dashboard knows about needs an entry, even if that entry only
// records that the fee was never verified. A source missing from the table would
// silently fall through to the unverified default and nobody would notice which.
func TestSourceRegistry_EverySourceIsNamedInTheFeeTable(t *testing.T) {
	named := map[string]bool{}
	for _, schedule := range fees.All() {
		named[schedule.Source] = true
	}
	for _, meta := range sourceRegistry {
		if !named[meta.Source] {
			t.Errorf("%s has no entry in the fee table", meta.Source)
		}
	}
}

// A fee of 0 is real on some venues, so the dashboard cannot read 0 as "free".
// The flag is what separates the two.
func TestNewWireMeta_UnverifiedFeeIsFlaggedNotZeroed(t *testing.T) {
	meta := newWireMeta([]string{"BTCUSDT"}, 1)

	var verified, unverified int
	for _, source := range meta.Sources {
		if source.FeeVerified {
			verified++
			if source.TakerFeeBps <= 0 {
				t.Errorf("%s claims a verified fee of %g bps", source.Source, source.TakerFeeBps)
			}
			continue
		}
		unverified++
		if source.TakerFeeBps != 0 || source.MakerFeeBps != 0 {
			t.Errorf("%s is unverified but carries fees %g/%g",
				source.Source, source.MakerFeeBps, source.TakerFeeBps)
		}
	}
	if verified == 0 || unverified == 0 {
		t.Fatalf("expected both verified and unverified sources, got %d/%d", verified, unverified)
	}

	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Sources []map[string]any `json:"sources"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded.Sources[0]["fee_verified"]; !ok {
		t.Error("meta.sources[] is missing fee_verified")
	}
}

// The disclosure has to name the exit as well as the entry, because the cost
// model charges both. A cost_basis that says only "taker fee" would understate
// what has been deducted just as much as omitting it.
func TestNewWireMeta_CostBasisNamesEntryAndExit(t *testing.T) {
	meta := newWireMeta([]string{"BTCUSDT"}, 1)

	if meta.CostBasis.Model == "none" {
		t.Error("cost_basis.model is still none after the fee model exists")
	}
	applied := map[string]bool{}
	for _, cost := range meta.CostBasis.Applied {
		applied[cost] = true
	}
	for _, want := range []string{"taker_fee_entry", "taker_fee_exit"} {
		if !applied[want] {
			t.Errorf("cost_basis.applied missing %q, got %v", want, meta.CostBasis.Applied)
		}
	}

	// Slippage and funding are still not deducted, and the contract requires the
	// dashboard to say so. Claiming them would be the "net profit" lie.
	excluded := map[string]bool{}
	for _, cost := range meta.CostBasis.Excluded {
		excluded[cost] = true
	}
	for _, want := range []string{"slippage", "funding"} {
		if !excluded[want] {
			t.Errorf("cost_basis.excluded missing %q, got %v", want, meta.CostBasis.Excluded)
		}
	}
	if applied["slippage"] {
		t.Error("slippage cannot be applied: it needs book depth, which arrives in phase 2")
	}
}

// The whole point of the step: a cell says what is left after the fees of a
// complete round trip.
func TestNewWireSpreads_AfterFeesIsGrossMinusRoundTrip(t *testing.T) {
	// hyperliquid and kraken are both verified: taker 4.5 and 5.0 bps.
	// Entry and exit on both legs = 19 bps = 0.19%.
	msg := newWireSpreads("BTCUSDT", map[string]float64{
		"hyperliquid_futures": 100,
		"kraken_futures":      101,
	}, nil, 1)

	cell, ok := matrixCell(msg, "perp_usd", "hyperliquid_futures", "kraken_futures")
	if !ok {
		t.Fatalf("no hyperliquid -> kraken cell in %v", groupIDs(msg))
	}

	if cell.SpreadAfterFeesPct == nil {
		t.Fatal("both venues are verified; the cell must carry an after-fee figure")
	}
	if math.Abs(cell.SpreadGrossPct-1.0) > 1e-9 {
		t.Errorf("gross = %g, want 1.0", cell.SpreadGrossPct)
	}
	if want := 1.0 - 0.19; math.Abs(*cell.SpreadAfterFeesPct-want) > 1e-9 {
		t.Errorf("after fees = %g, want %g", *cell.SpreadAfterFeesPct, want)
	}
	if *cell.SpreadAfterFeesPct >= cell.SpreadGrossPct {
		t.Error("the after-fee figure must be strictly smaller than the gross one")
	}
}

// A tight spread goes NEGATIVE after a round trip, and that is the number the
// operator needs to see. Clamping it at zero, or falling back to the gross
// figure, would turn a losing trade into a flat one.
func TestNewWireSpreads_AfterFeesGoesNegativeOnATightSpread(t *testing.T) {
	msg := newWireSpreads("BTCUSDT", map[string]float64{
		"hyperliquid_futures": 100,
		"kraken_futures":      100.02, // 0.02% gross, far below the 0.19% round trip
	}, nil, 1)

	cell, ok := matrixCell(msg, "perp_usd", "hyperliquid_futures", "kraken_futures")
	if !ok {
		t.Fatalf("no hyperliquid -> kraken cell in %v", groupIDs(msg))
	}
	if cell.SpreadAfterFeesPct == nil {
		t.Fatal("expected an after-fee figure")
	}
	if *cell.SpreadAfterFeesPct >= 0 {
		t.Errorf("after fees = %g, want negative: 0.02%% gross cannot survive a 0.19%% round trip",
			*cell.SpreadAfterFeesPct)
	}
}

// An unverified fee must produce no figure at all. Treating the missing number
// as zero would report the full gross spread as if it were free to capture.
func TestNewWireSpreads_NoAfterFeesWhenAVenueIsUnverified(t *testing.T) {
	msg := newWireSpreads("BTCUSDT", map[string]float64{
		"binance_futures": 100, // verified
		"bybit_futures":   101, // not verified
		"okx_futures":     102, // not verified
	}, nil, 1)

	for _, g := range msg.CrossVenueGroups {
		for buySource, row := range g.Matrix {
			for sellSource, cell := range row {
				if cell.SpreadAfterFeesPct != nil {
					t.Errorf("%s -> %s produced %g with an unverified venue",
						buySource, sellSource, *cell.SpreadAfterFeesPct)
				}
				if cell.SpreadGrossPct == 0 {
					t.Errorf("%s -> %s lost its gross figure too", buySource, sellSource)
				}
			}
		}
	}
}

// An alert carries the same disclosure as a cell: the gross number that
// triggered it, and what survives the fees.
func TestNewWireOpportunity_CarriesTheAfterFeeFigure(t *testing.T) {
	verified := newWireOpportunity("BTCUSDT", "perp_usd",
		"hyperliquid_futures", "kraken_futures", 100, 101, 1)
	if verified.SpreadAfterFeesPct == nil {
		t.Fatal("a verified pair must carry an after-fee figure")
	}
	if want := 1.0 - 0.19; math.Abs(*verified.SpreadAfterFeesPct-want) > 1e-9 {
		t.Errorf("after fees = %g, want %g", *verified.SpreadAfterFeesPct, want)
	}

	unverified := newWireOpportunity("BTCUSDT", "perp_usdt",
		"binance_futures", "bybit_futures", 100, 101, 1)
	if unverified.SpreadAfterFeesPct != nil {
		t.Errorf("an unverified pair must carry null, got %g", *unverified.SpreadAfterFeesPct)
	}
	if unverified.SpreadGrossPct == 0 {
		t.Error("the gross figure must survive an unverified fee")
	}
}

// The wire says "after fees", and no field anywhere may call it profit or net.
// This is CLAUDE.md rule 2 enforced on the contract itself.
func TestWire_NeverNamesAnAfterFeeFigureAsProfit(t *testing.T) {
	msg := newWireSpreads("BTCUSDT", map[string]float64{
		"hyperliquid_futures": 100,
		"kraken_futures":      101,
	}, nil, 1)
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for _, forbidden := range []string{"profit", "net_pct", "\"net\"", "loi_nhuan"} {
		if containsFold(string(raw), forbidden) {
			t.Errorf("spreads payload contains %q; an after-fee spread is not profit", forbidden)
		}
	}
}

func containsFold(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if equalFold(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// matrixCell returns one cell from one named group. Reaching into every group
// with a hardcoded pair passes only while the fixture happens to yield a single
// group, and starts failing for the wrong reason once it yields two.
func matrixCell(msg wireSpreads, groupID, buySource, sellSource string) (wireSpreadCell, bool) {
	for _, group := range msg.CrossVenueGroups {
		if group.GroupID != groupID {
			continue
		}
		cell, ok := group.Matrix[buySource][sellSource]
		return cell, ok
	}
	return wireSpreadCell{}, false
}
