package store

import (
	"context"
	"fmt"
)

// CrossSpreadEvent is one row of cross_spread_events (PLAN 4.5i). Every column
// carries its unit, because phase 8 reads this table from Python.
type CrossSpreadEvent struct {
	Writer          string // scanner:<port>; see schema.sql
	Symbol          string
	ThresholdAPRPct float64
	StartedAtMs     int64

	ShortSource string
	LongSource  string

	PeakGrossAPRPct float64
	PeakAtMs        int64
	LastSeenAboveMs int64

	EndedAtMs   int64 // 0 while open
	DurationSec float64
	EndReason   string // "" while open

	SampleEverySec int64
	RateModel      string
	RecordedAtMs   int64 // set by the store on write
}

// PutCrossSpreadEvents writes episodes, replacing an earlier row of the same
// episode (an open one rewritten as it lasts, then closed).
func (s *Store) PutCrossSpreadEvents(ctx context.Context, events []CrossSpreadEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin cross event tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a committed tx rolls back to a no-op
	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO cross_spread_events (
			writer, symbol, threshold_apr_pct, started_at_ms, short_source, long_source,
			peak_gross_apr_pct, peak_at_ms, last_seen_above_ms,
			ended_at_ms, duration_sec, end_reason,
			sample_every_sec, rate_model, recorded_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("store: prepare cross event insert: %w", err)
	}
	defer stmt.Close()
	recordedAtMs := s.now().UnixMilli()
	for _, e := range events {
		if e.Writer == "" || e.Symbol == "" || e.StartedAtMs <= 0 || e.ThresholdAPRPct <= 0 || e.RateModel == "" {
			return fmt.Errorf("store: refusing an unidentified cross event: %+v", e)
		}
		if (e.EndedAtMs == 0) != (e.EndReason == "") {
			return fmt.Errorf("store: cross event %s@%d has ended_at_ms %d with reason %q — an end needs both",
				e.Symbol, e.StartedAtMs, e.EndedAtMs, e.EndReason)
		}
		if _, err := stmt.ExecContext(ctx,
			e.Writer, e.Symbol, e.ThresholdAPRPct, e.StartedAtMs, e.ShortSource, e.LongSource,
			e.PeakGrossAPRPct, e.PeakAtMs, e.LastSeenAboveMs,
			e.EndedAtMs, e.DurationSec, e.EndReason,
			e.SampleEverySec, e.RateModel, recordedAtMs,
		); err != nil {
			return fmt.Errorf("store: insert cross event %s: %w", e.Symbol, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit cross event tx: %w", err)
	}
	return nil
}

// CloseOpenCrossSpreadEvents ends every episode an earlier run of THIS writer
// left open, at the last instant it was seen above its threshold, with reason
// 'restart'. Another writer's rows are never touched: they may be live. It
// returns how many it closed.
func (s *Store) CloseOpenCrossSpreadEvents(ctx context.Context, writer string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE cross_spread_events
		SET ended_at_ms = last_seen_above_ms,
		    duration_sec = (last_seen_above_ms - started_at_ms) / 1000.0,
		    end_reason = 'restart',
		    recorded_at_ms = ?
		WHERE ended_at_ms = 0 AND writer = ?`, s.now().UnixMilli(), writer)
	if err != nil {
		return 0, fmt.Errorf("store: close open cross events: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CrossSpreadEvents reads one writer's episodes that started at or after fromMs,
// newest first.
func (s *Store) CrossSpreadEvents(ctx context.Context, writer string, fromMs int64, limit int) ([]CrossSpreadEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT writer, symbol, threshold_apr_pct, started_at_ms, short_source, long_source,
		       peak_gross_apr_pct, peak_at_ms, last_seen_above_ms,
		       ended_at_ms, duration_sec, end_reason,
		       sample_every_sec, rate_model, recorded_at_ms
		FROM cross_spread_events
		WHERE writer = ? AND started_at_ms >= ?
		ORDER BY started_at_ms DESC, symbol, threshold_apr_pct
		LIMIT ?`, writer, fromMs, limit)
	if err != nil {
		return nil, fmt.Errorf("store: query cross events: %w", err)
	}
	defer rows.Close()
	var out []CrossSpreadEvent
	for rows.Next() {
		var e CrossSpreadEvent
		if err := rows.Scan(&e.Writer, &e.Symbol, &e.ThresholdAPRPct, &e.StartedAtMs, &e.ShortSource, &e.LongSource,
			&e.PeakGrossAPRPct, &e.PeakAtMs, &e.LastSeenAboveMs,
			&e.EndedAtMs, &e.DurationSec, &e.EndReason,
			&e.SampleEverySec, &e.RateModel, &e.RecordedAtMs); err != nil {
			return nil, fmt.Errorf("store: scan cross event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
