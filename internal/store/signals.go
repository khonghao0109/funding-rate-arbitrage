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
