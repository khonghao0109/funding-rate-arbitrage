package store

import (
	"context"
	"fmt"
)

// SignalRecord is one live decision as the journal keeps it (step 3.5).
//
// Flat and unit-suffixed on purpose: this table is read back by the gate
// comparison and, later, from Python. The reasoning travels as JSON because the
// list of checks belongs to internal/strategy and changes with it.
type SignalRecord struct {
	EvaluatedAtMs int64
	Symbol        string
	PerpSource    string
	SpotSource    string

	Action       string
	NetAPRFrac   float64
	NetAPROK     bool
	CostTotalPct float64

	ChecksJSON string
	ParamsJSON string

	RecordedAtMs int64 // set by the store on write; ignored on input
}

// PutSignalDecisions writes decisions, replacing any earlier row for the same
// instant and market, and returns how many were written.
func (s *Store) PutSignalDecisions(ctx context.Context, records []SignalRecord) (int, error) {
	if len(records) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin signal tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a committed tx rolls back to a no-op

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO signal_journal (
			evaluated_at_ms, symbol, perp_source, spot_source,
			action, net_apr_frac, net_apr_ok, cost_total_pct,
			checks_json, params_json, recorded_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("store: prepare signal insert: %w", err)
	}
	defer stmt.Close()

	recordedAtMs := s.now().UnixMilli()
	written := 0
	for _, r := range records {
		if r.EvaluatedAtMs <= 0 || r.Symbol == "" || r.PerpSource == "" || r.Action == "" {
			return 0, fmt.Errorf("store: refusing an unidentified signal row: %+v", r)
		}
		if _, err := stmt.ExecContext(ctx,
			r.EvaluatedAtMs, r.Symbol, r.PerpSource, r.SpotSource,
			r.Action, r.NetAPRFrac, r.NetAPROK, r.CostTotalPct,
			r.ChecksJSON, r.ParamsJSON, recordedAtMs,
		); err != nil {
			return 0, fmt.Errorf("store: insert signal %s/%s: %w", r.Symbol, r.PerpSource, err)
		}
		written++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit signal tx: %w", err)
	}
	return written, nil
}

// SignalDecisions reads the journal in a closed-open window, oldest first.
func (s *Store) SignalDecisions(ctx context.Context, fromMs, toMs int64) ([]SignalRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT evaluated_at_ms, symbol, perp_source, spot_source,
		       action, net_apr_frac, net_apr_ok, cost_total_pct,
		       checks_json, params_json, recorded_at_ms
		FROM signal_journal
		WHERE evaluated_at_ms >= ? AND evaluated_at_ms < ?
		ORDER BY evaluated_at_ms, symbol, perp_source`, fromMs, toMs)
	if err != nil {
		return nil, fmt.Errorf("store: query signal journal: %w", err)
	}
	defer rows.Close()

	var out []SignalRecord
	for rows.Next() {
		var r SignalRecord
		if err := rows.Scan(&r.EvaluatedAtMs, &r.Symbol, &r.PerpSource, &r.SpotSource,
			&r.Action, &r.NetAPRFrac, &r.NetAPROK, &r.CostTotalPct,
			&r.ChecksJSON, &r.ParamsJSON, &r.RecordedAtMs); err != nil {
			return nil, fmt.Errorf("store: scan signal row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SignalActionTally counts the journal's rows per action in a closed-open
// window without loading them: the paper ledger (step 4.3) tallies every
// rebuild, and a window of a fortnight holds ~170k rows whose checks_json
// alone is several kilobytes each.
func (s *Store) SignalActionTally(ctx context.Context, fromMs, toMs int64) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT action, COUNT(*) FROM signal_journal
		WHERE evaluated_at_ms >= ? AND evaluated_at_ms < ?
		GROUP BY action`, fromMs, toMs)
	if err != nil {
		return nil, fmt.Errorf("store: tally signal journal: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var action string
		var n int
		if err := rows.Scan(&action, &n); err != nil {
			return nil, fmt.Errorf("store: scan signal tally: %w", err)
		}
		out[action] = n
	}
	return out, rows.Err()
}

// SignalPositionRows reads the rows that OPEN or CLOSE a paper position —
// action enter or exit — in a closed-open window, oldest first, WITHOUT
// their checks_json: the ledger prices a decision and never re-reads its
// reasoning, and leaving the column out is what keeps a rebuild cheap on
// the machine the live scanner runs on. ChecksJSON is therefore empty on
// every record returned here; a caller that needs it reads SignalDecisions.
func (s *Store) SignalPositionRows(ctx context.Context, fromMs, toMs int64) ([]SignalRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT evaluated_at_ms, symbol, perp_source, spot_source,
		       action, net_apr_frac, net_apr_ok, cost_total_pct,
		       params_json, recorded_at_ms
		FROM signal_journal
		WHERE evaluated_at_ms >= ? AND evaluated_at_ms < ? AND action IN ('enter', 'exit')
		ORDER BY evaluated_at_ms, symbol, perp_source`, fromMs, toMs)
	if err != nil {
		return nil, fmt.Errorf("store: query signal position rows: %w", err)
	}
	defer rows.Close()

	var out []SignalRecord
	for rows.Next() {
		var r SignalRecord
		if err := rows.Scan(&r.EvaluatedAtMs, &r.Symbol, &r.PerpSource, &r.SpotSource,
			&r.Action, &r.NetAPRFrac, &r.NetAPROK, &r.CostTotalPct,
			&r.ParamsJSON, &r.RecordedAtMs); err != nil {
			return nil, fmt.Errorf("store: scan signal position row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InstrumentQuoteAssets is source|symbol → the venue-declared quote asset
// from each market's OWN newest snapshot day — not one global newest day, so
// a venue whose fetch failed on the latest day still answers with the day
// it last succeeded. A market never snapshotted is absent, and the caller
// must read absence as NOT KNOWN.
func (s *Store) InstrumentQuoteAssets(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.source, i.symbol, i.quote_asset FROM instrument_snapshots i
		WHERE i.snapshot_day = (SELECT MAX(j.snapshot_day) FROM instrument_snapshots j
		                        WHERE j.source = i.source AND j.symbol = i.symbol)`)
	if err != nil {
		return nil, fmt.Errorf("store: query instrument quote assets: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var source, symbol, quote string
		if err := rows.Scan(&source, &symbol, &quote); err != nil {
			return nil, fmt.Errorf("store: scan instrument quote asset: %w", err)
		}
		out[source+"|"+symbol] = quote
	}
	return out, rows.Err()
}
