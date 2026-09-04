package store

import (
	"context"
	"fmt"

	"futures-arbitrage-scanner/exchanges"
)

// FundingRow is one stored funding_history row, read back.
//
// It mirrors exchanges.FundingHistoryEntry and adds RecordedAtMs, which is the
// store's own stamp — when this process wrote the row, not when anything was
// received. The two are kept apart deliberately: a rate that settled last March
// has no receive time, and giving it one would let a freshness check compare a
// historical row against a staleness threshold.
type FundingRow struct {
	exchanges.FundingHistoryEntry
	RecordedAtMs int64
}

// Coverage is what the corpus actually holds for one series.
//
// It is computed from the rows rather than recorded beside them, so it cannot
// drift from the data it describes. Phase 3 reads it before a backtest: three
// of the seven venues cannot deliver twelve months (OKX keeps ~3, Gate 180
// days, Paradex has no settlements at all), and a backtest that assumes uniform
// depth would silently weight the venues by their retention policy.
type Coverage struct {
	Source     string
	Symbol     string
	Model      string
	Rows       int
	OldestAtMs int64
	NewestAtMs int64
}

// PutFundingHistory inserts entries, ignoring settlements already stored, and
// returns how many rows were new.
//
// INSERT OR IGNORE against the natural key is what makes the whole design
// restartable: backfill and the hourly top-up may re-fetch overlapping windows
// as often as they like, and an overlap costs a no-op instead of a duplicate.
//
// IGNORE rather than REPLACE, deliberately: the FIRST reading of a settlement
// wins. A venue restating a rate it has already settled is not something any of
// the seven was seen to do, and quietly overwriting would erase what was
// actually collected at the time in favour of a later story about it. If a
// venue ever does restate one, that has to surface as a decision, not as a
// silent update inside a routine top-up.
func (s *Store) PutFundingHistory(ctx context.Context, entries []exchanges.FundingHistoryEntry) (int, error) {
	if len(entries) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin funding insert: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a committed tx rolls back to a no-op

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO funding_history (
			source, symbol, funding_at_ms, model,
			rate_per_interval_frac, interval_sec, gap_prev_sec,
			rate_per_8h_frac, apr_frac,
			raw_rate, raw_rate_field, rate_type, mark_price, recorded_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("store: prepare funding insert: %w", err)
	}
	defer stmt.Close()

	recordedAtMs := s.now().UnixMilli()
	inserted := 0
	for _, entry := range entries {
		if entry.Source == "" || entry.Symbol == "" || entry.SettledAtMs <= 0 {
			return 0, fmt.Errorf("store: refusing an unidentified funding row: %+v", entry)
		}
		if entry.IntervalSec <= 0 {
			// Everything downstream divides by it; a zero here would become an
			// infinite APR in the backtest rather than an error at the border.
			return 0, fmt.Errorf("store: %s/%s at %d has IntervalSec %d",
				entry.Source, entry.Symbol, entry.SettledAtMs, entry.IntervalSec)
		}
		result, err := stmt.ExecContext(ctx,
			entry.Source, entry.Symbol, entry.SettledAtMs, string(entry.Model),
			entry.RatePerIntervalFrac, entry.IntervalSec, entry.GapPrevSec,
			entry.RatePer8hFrac, entry.APRFrac,
			entry.RawRate, entry.RawRateField, entry.RateType, entry.MarkPrice, recordedAtMs)
		if err != nil {
			return 0, fmt.Errorf("store: insert funding %s/%s: %w", entry.Source, entry.Symbol, err)
		}
		if affected, err := result.RowsAffected(); err == nil {
			inserted += int(affected)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit funding insert: %w", err)
	}
	return inserted, nil
}

// FundingHistory reads one pair's settled rates across every venue in a window,
// oldest first. An empty symbol reads every pair.
func (s *Store) FundingHistory(ctx context.Context, symbol string, fromMs, toMs int64) ([]FundingRow, error) {
	// The symbol predicate is composed rather than expressed as
	// `(? = '' OR symbol = ?)`: SQLite will not use funding_history_by_symbol
	// through an OR on a bound parameter, so the convenient form turns a
	// 30-day lookup into a full scan of a corpus meant to hold a year.
	query := `
		SELECT source, symbol, funding_at_ms, model,
		       rate_per_interval_frac, interval_sec, gap_prev_sec,
		       rate_per_8h_frac, apr_frac,
		       raw_rate, raw_rate_field, rate_type, mark_price, recorded_at_ms
		FROM funding_history
		WHERE funding_at_ms >= ? AND funding_at_ms < ?`
	args := []any{fromMs, toMs}
	if symbol != "" {
		query += " AND symbol = ?"
		args = append(args, symbol)
	}
	query += " ORDER BY funding_at_ms, source"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query funding history: %w", err)
	}
	defer rows.Close()

	var out []FundingRow
	for rows.Next() {
		var row FundingRow
		var model string
		if err := rows.Scan(
			&row.Source, &row.Symbol, &row.SettledAtMs, &model,
			&row.RatePerIntervalFrac, &row.IntervalSec, &row.GapPrevSec,
			&row.RatePer8hFrac, &row.APRFrac,
			&row.RawRate, &row.RawRateField, &row.RateType, &row.MarkPrice, &row.RecordedAtMs,
		); err != nil {
			return nil, fmt.Errorf("store: scan funding history: %w", err)
		}
		row.Model = exchanges.FundingModel(model)
		out = append(out, row)
	}
	return out, rows.Err()
}

// LatestFundingAt is the newest settlement stored for one series, or 0 when
// nothing is. The incremental top-up starts from it, which is what lets the
// scanner resume after any outage without re-reading a year.
func (s *Store) LatestFundingAt(ctx context.Context, source, symbol string) (int64, error) {
	var newest *int64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(funding_at_ms) FROM funding_history WHERE source = ? AND symbol = ?`,
		source, symbol).Scan(&newest)
	if err != nil {
		return 0, fmt.Errorf("store: latest funding for %s/%s: %w", source, symbol, err)
	}
	if newest == nil {
		return 0, nil // MAX over no rows is SQL NULL, not 0
	}
	return *newest, nil
}

// FundingCoverage reports what the corpus holds, per series, ordered by symbol
// then source.
func (s *Store) FundingCoverage(ctx context.Context) ([]Coverage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source, symbol, model, COUNT(*), MIN(funding_at_ms), MAX(funding_at_ms)
		FROM funding_history
		GROUP BY source, symbol, model
		ORDER BY symbol, source`)
	if err != nil {
		return nil, fmt.Errorf("store: query funding coverage: %w", err)
	}
	defer rows.Close()

	var out []Coverage
	for rows.Next() {
		var c Coverage
		if err := rows.Scan(&c.Source, &c.Symbol, &c.Model, &c.Rows, &c.OldestAtMs, &c.NewestAtMs); err != nil {
			return nil, fmt.Errorf("store: scan funding coverage: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
