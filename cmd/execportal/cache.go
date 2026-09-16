package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"futures-arbitrage-scanner/internal/execution"
)

// The intent cache — the SAME files cmd/execcheck reads and writes, in the SAME
// shape, in the SAME directory.
//
// Sharing the files is the point: a position opened in the browser can be
// inspected with `execcheck -status` and closed with `execcheck -close`, and the
// sixteen intents the 4.4b/4.5 acceptance left behind appear in the portal's
// history. Two formats would be two sources of truth about one position.
// guard_test.go holds the two struct shapes together by reading execcheck's
// source, because execcheck is a main package and cannot be imported.
//
// It is still a CACHE (CLAUDE.md rule 7). Every figure in it was true at an
// instant; what an intent holds NOW is read from the venue by the ClientOrderIDs
// derived from its id (hedge.go), and nothing here is evidence of a position.

// stateDir is execcheck's directory, under the gitignored .paper/.
const stateDir = ".paper/exec"

// intentState mirrors cmd/execcheck's `state` field for field and tag for tag.
// Do not add a field here alone: execcheck re-saves a file it closes, and a
// field it does not know would be silently dropped on that write.
type intentState struct {
	IntentID      string  `json:"intent_id"`
	Symbol        string  `json:"symbol"`
	OpenedAtMs    int64   `json:"opened_at_ms"`
	LegOrder      string  `json:"leg_order"`
	NotionalQuote float64 `json:"notional_quote"`
	TargetQtyCoin float64 `json:"target_qty_coin"`

	SpotClientOrderID   string  `json:"spot_client_order_id"`
	PerpClientOrderID   string  `json:"perp_client_order_id"`
	SpotFilledQtyCoin   float64 `json:"spot_filled_qty_coin"`
	PerpFilledQtyCoin   float64 `json:"perp_filled_qty_coin"`
	SpotAvgPriceQuote   float64 `json:"spot_avg_fill_price_quote"`
	PerpAvgPriceQuote   float64 `json:"perp_avg_fill_price_quote"`
	SpotRefMidQuote     float64 `json:"spot_ref_mid_quote"`
	PerpRefMidQuote     float64 `json:"perp_ref_mid_quote"`
	SpotBestAskQuote    float64 `json:"spot_best_ask_quote"`
	PerpBestBidQuote    float64 `json:"perp_best_bid_quote"`
	BookSampledAtMs     int64   `json:"book_sampled_at_ms"`
	UnhedgedWindowMs    int64   `json:"unhedged_window_ms"`
	ReducedToMatch      bool    `json:"reduced_to_match"`
	Outcome             string  `json:"outcome"`
	NextFundingTimeMs   int64   `json:"next_funding_time_ms"`
	ClosedAtMs          int64   `json:"closed_at_ms"`
	ClosedQtyCoin       float64 `json:"closed_qty_coin"`
	RealizedQuote       float64 `json:"realized_quote"`
	FundingQuote        float64 `json:"funding_received_quote"`
	CommissionQuote     float64 `json:"commission_quote"`
	SlippageQuote       float64 `json:"slippage_quote"`
	SettlementsCounted  int     `json:"settlements_counted"`
	PairPriceDriftQuote float64 `json:"pair_price_drift_quote"`
	CloseReasonVI       string  `json:"close_reason_vi,omitempty"`
	NoteVI              string  `json:"note_vi"`
}

// tracked reports whether the portal should read this intent's orders back from
// the venue on every refresh.
//
// An intent is left out only when the machine that last acted on it RETURNED
// HAVING PROVED IT FLAT and left no note: a close that finished, an open that
// unwound, an open that never filled. That is not trusting the cache about a
// position — execution.Close and the unwind both confirm flatness against the
// venue with two independent pieces of evidence before they return, and when
// either proof fails the portal writes the error into NoteVI, which keeps the
// intent tracked. Everything else is read from the venue on every refresh.
//
// The gap, stated: an intent cmd/execcheck opened whose unwind FAILED carries no
// note (execcheck prints the alarm and exits 1 rather than writing it), so the
// portal does not poll its spot leg. Its perp leg is still covered — the
// venue's position would disagree with the tracked intents and read as
// evidence_conflict — and /api/reconcile scans EVERY intent regardless.
func (s intentState) tracked() bool {
	if s.NoteVI != "" {
		return true
	}
	if s.ClosedAtMs > 0 || s.Outcome == string(execution.OutcomeBothFlat) {
		return false
	}
	return s.SpotFilledQtyCoin > 0 || s.PerpFilledQtyCoin > 0
}

// intentIDPattern is what an intent id may look like.
//
// It is enforced on every id that arrives over HTTP, because the id becomes a
// FILE PATH: "../../../.env" is a perfectly good string and a perfectly bad
// filename. Lower-case letters, digits and dashes cover every id either tool
// has minted (xbtcusdt-20260913-074359, pbtcusdt-20260914-101500-123).
var intentIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// errBadIntentID is an id that does not match intentIDPattern.
var errBadIntentID = errors.New("intent id không hợp lệ: chỉ chữ thường, số và dấu gạch ngang, tối đa 64 ký tự")

func validIntentID(id string) error {
	if !intentIDPattern.MatchString(id) {
		return errBadIntentID
	}
	return nil
}

func statePath(dir, id string) (string, error) {
	if err := validIntentID(id); err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".json"), nil
}

// saveState writes one intent atomically.
//
// execcheck writes in place; the portal writes a sibling and renames it over,
// because a portal is a long-running process that a SIGTERM can reach in the
// middle of a write, and a truncated intent file is an intent whose entry fills
// nobody can recover. The bytes on disk are identical to execcheck's.
func saveState(dir string, s intentState) error {
	path, err := statePath(dir, s.IntentID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+s.IntentID+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(blob, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func loadState(dir, id string) (intentState, error) {
	var s intentState
	path, err := statePath(dir, id)
	if err != nil {
		return s, err
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(blob, &s); err != nil {
		return s, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return s, nil
}

// listStates reads every intent file, newest first. A file that cannot be read
// is REPORTED by name rather than skipped: an unreadable intent is an intent
// whose orders nobody is looking at.
func listStates(dir, symbol string) ([]intentState, []string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var (
		out        []intentState
		unreadable []string
	)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		s, err := loadState(dir, id)
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if symbol != "" && s.Symbol != symbol {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OpenedAtMs != out[j].OpenedAtMs {
			return out[i].OpenedAtMs > out[j].OpenedAtMs
		}
		return out[i].IntentID > out[j].IntentID
	})
	return out, unreadable, nil
}

// The first letter of an intent id says which tool opened it: execcheck's start
// with "x", a button press on this page with "p", the auto-trader with "a"
// (PLAN Q18). It is how the bot recognises — and after a restart adopts — the
// one position that is its own, and never one a person opened.
const (
	intentPrefixPortal    = "p"
	intentPrefixAutotrade = "a"
)

// newIntentID mints a readable id for a button press. The milliseconds are there
// because a browser can submit twice inside one second where a person at a
// terminal cannot, and two intents with one id would derive the same
// ClientOrderIDs.
func newIntentID(symbol string, now time.Time) string {
	return newIntentIDWith(intentPrefixPortal, symbol, now)
}

func newIntentIDWith(prefix, symbol string, now time.Time) string {
	u := now.UTC()
	return fmt.Sprintf("%s%s-%s-%03d", prefix, strings.ToLower(symbol), u.Format("20060102-150405"), u.Nanosecond()/int(time.Millisecond))
}
