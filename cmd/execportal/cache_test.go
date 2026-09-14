package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A file cmd/execcheck wrote during the 4.4b/4.5 acceptance on 2026-09-13,
// verbatim (xbtcusdt-20260913-075852.json — the lifecycle held across a
// settlement). It carries order ids and prices, no credential.
const execcheckFile = `{
  "intent_id": "xbtcusdt-20260913-075852",
  "symbol": "BTCUSDT",
  "opened_at_ms": 1789286332825,
  "leg_order": "sequential_spot_first",
  "notional_quote": 2000,
  "target_qty_coin": 0.0259,
  "spot_client_order_id": "fa1s5c9dd4857e74e0917a33c298",
  "perp_client_order_id": "fa1pf66a679bfc332201f98202f5",
  "spot_filled_qty_coin": 0.0259,
  "perp_filled_qty_coin": 0.0259,
  "spot_avg_fill_price_quote": 77123.06,
  "perp_avg_fill_price_quote": 77123.3,
  "spot_ref_mid_quote": 77123.055,
  "perp_ref_mid_quote": 77129.4,
  "spot_best_ask_quote": 77123.06,
  "perp_best_bid_quote": 77123.3,
  "book_sampled_at_ms": 1789286331710,
  "unhedged_window_ms": 631,
  "reduced_to_match": false,
  "outcome": "both_open",
  "next_funding_time_ms": 1789286400000,
  "closed_at_ms": 1789286479458,
  "closed_qty_coin": 0.0259,
  "realized_quote": -1.5564851699998943,
  "funding_received_quote": 0.19975549,
  "commission_quote": 1.5981211599999998,
  "slippage_quote": 0.15811949999989447,
  "settlements_counted": 1,
  "pair_price_drift_quote": 0,
  "note_vi": ""
}
`

// A real execcheck file decodes with NO unknown field, and re-encodes to the
// same bytes — so the portal re-saving a file execcheck wrote changes nothing.
func TestIntentState_ReadsAndRewritesARealExeccheckFileByteForByte(t *testing.T) {
	var s intentState
	dec := json.NewDecoder(strings.NewReader(execcheckFile))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		t.Fatalf("execcheck's own file does not decode strictly: %v", err)
	}
	if s.IntentID != "xbtcusdt-20260913-075852" || s.FundingQuote != 0.19975549 || s.SettlementsCounted != 1 {
		t.Errorf("decoded %+v", s)
	}
	dir := t.TempDir()
	if err := saveState(dir, s); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(dir, s.IntentID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(blob, []byte(execcheckFile)) {
		t.Errorf("re-saved file differs from execcheck's:\n%s", blob)
	}
	info, err := os.Stat(filepath.Join(dir, s.IntentID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode %v, want 0644 like execcheck's", info.Mode().Perm())
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".*.tmp"))
	if len(leftovers) > 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

func TestValidIntentID(t *testing.T) {
	for _, ok := range []string{"xbtcusdt-20260913-074359", "pbtcusdt-20260914-101500-123", "a"} {
		if err := validIntentID(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "../x", "..", "a/b", `a\b`, "a.json", "X", "-lead", " a", "a b", strings.Repeat("a", 65), "a\x00"} {
		if err := validIntentID(bad); !errors.Is(err, errBadIntentID) {
			t.Errorf("%q accepted", bad)
		}
		if _, err := loadState(t.TempDir(), bad); err == nil {
			t.Errorf("loadState(%q) did not refuse", bad)
		}
	}
}

func TestNewIntentID_IsValidDistinctAndMarkedAsThePortals(t *testing.T) {
	at := time.Date(2026, 9, 14, 10, 15, 0, 123_456_789, time.FixedZone("ICT", 7*3600))
	id := newIntentID("BTCUSDT", at)
	if id != "pbtcusdt-20260914-031500-123" {
		t.Errorf("id = %q", id)
	}
	if err := validIntentID(id); err != nil {
		t.Error(err)
	}
	if newIntentID("BTCUSDT", at.Add(time.Millisecond)) == id {
		t.Error("two intents one millisecond apart share an id, and so would share ClientOrderIDs")
	}
}

func TestMintIntentID_SkipsAnIDAFileAlreadyUses(t *testing.T) {
	p := testPortal(t, false)
	at := time.Date(2026, 9, 14, 3, 15, 0, 0, time.UTC)
	p.now = func() time.Time { return at }
	taken := newIntentID("BTCUSDT", at)
	if err := saveState(p.stateDir, intentState{IntentID: taken, Symbol: "BTCUSDT"}); err != nil {
		t.Fatal(err)
	}
	got, err := p.mintIntentID("BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	if got == taken {
		t.Errorf("minted %q, which a file already uses", got)
	}
}

func TestListStates_NewestFirstFilteredAndLoudAboutBadFiles(t *testing.T) {
	dir := t.TempDir()
	for _, s := range []intentState{
		{IntentID: "xbtcusdt-1", Symbol: "BTCUSDT", OpenedAtMs: 100},
		{IntentID: "pbtcusdt-2", Symbol: "BTCUSDT", OpenedAtMs: 300},
		{IntentID: "pethusdt-3", Symbol: "ETHUSDT", OpenedAtMs: 200},
	} {
		if err := saveState(dir, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".pbtcusdt-2.123.tmp"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	all, unreadable, err := listStates(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].IntentID != "pbtcusdt-2" || all[2].IntentID != "xbtcusdt-1" {
		t.Errorf("order = %v", ids(all))
	}
	if len(unreadable) != 1 || !strings.Contains(unreadable[0], "broken.json") {
		t.Errorf("unreadable = %v, want the broken file named", unreadable)
	}
	btc, _, _ := listStates(dir, "BTCUSDT")
	if len(btc) != 2 {
		t.Errorf("BTCUSDT filter = %v", ids(btc))
	}
	none, _, err := listStates(filepath.Join(dir, "missing"), "")
	if err != nil || len(none) != 0 {
		t.Errorf("a missing directory is an empty cache, got %v %v", none, err)
	}
}

func ids(list []intentState) []string {
	var out []string
	for _, s := range list {
		out = append(out, s.IntentID)
	}
	return out
}

func TestIntentState_Tracked(t *testing.T) {
	cases := []struct {
		name string
		s    intentState
		want bool
	}{
		{"open and filled", intentState{Outcome: "both_open", SpotFilledQtyCoin: 0.0008, PerpFilledQtyCoin: 0.0008}, true},
		{"closed cleanly", intentState{Outcome: "both_open", SpotFilledQtyCoin: 0.0008, ClosedAtMs: 1}, false},
		{"closed with an error", intentState{Outcome: "both_open", SpotFilledQtyCoin: 0.0008, ClosedAtMs: 1, NoteVI: "ĐÓNG CHƯA XONG"}, true},
		{"refused before placing", intentState{Outcome: "both_flat"}, false},
		{"unwound cleanly", intentState{Outcome: "both_flat", SpotFilledQtyCoin: 0.0008}, false},
		{"unwind incomplete (portal writes the note)", intentState{Outcome: "both_flat", SpotFilledQtyCoin: 0.0008, NoteVI: "UNWIND INCOMPLETE"}, true},
		{"reconciled", intentState{Outcome: "both_open", SpotFilledQtyCoin: 0.0008, NoteVI: "đã cân lại"}, true},
	}
	for _, tc := range cases {
		if got := tc.s.tracked(); got != tc.want {
			t.Errorf("%s: tracked = %v, want %v", tc.name, got, tc.want)
		}
	}
}
