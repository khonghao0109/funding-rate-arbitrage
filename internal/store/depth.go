package store

import (
	"context"
	"fmt"

	"futures-arbitrage-scanner/internal/depth"
)

// Order book depth snapshots (step 2.7b).
//
// Unlike funding history, this series can only ever be collected FORWARD: a
// venue publishes its book as it is now and nowhere keeps what it was an hour
// ago. Every sweep not written here is a gap phase 3 can never fill, which is
// why the table exists at all when the dashboard itself would be content with
// memory.

// PutDepthSnapshots writes one collection round.
//
// Summaries that carry an error are skipped rather than stored as zeros: a row
// of zeros in this table is indistinguishable from a market with an empty book,
// and phase 3 would read it as "no liquidity" instead of "not measured". The
// dashboard still shows those, because there the distinction is visible.
func (s *Store) PutDepthSnapshots(ctx context.Context, summaries []depth.Summary) (int, error) {
	if len(summaries) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin depth tx: %w", err)
	}
	defer tx.Rollback()

	// INSERT OR REPLACE, unlike funding history's INSERT OR IGNORE: a
	// re-measurement of the same market at the same instant is a corrected
	// reading of a moment, not a second settlement that must never overwrite
	// the first.
	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO depth_snapshots (
			source, symbol, sampled_at_ms, venue_time_ms,
			mid_price_quote, best_bid_quote, best_ask_quote,
			best_bid_qty_coin, best_ask_qty_coin, spread_pct,
			bid_depth_within_0_1pct_quote, ask_depth_within_0_1pct_quote,
			bid_depth_within_0_5pct_quote, ask_depth_within_0_5pct_quote,
			bid_levels, ask_levels, bid_span_pct, ask_span_pct, is_contract_book
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("store: prepare depth insert: %w", err)
	}
	defer stmt.Close()

	written := 0
	for _, summary := range summaries {
		if !summary.OK() {
			continue
		}
		if summary.Source == "" || summary.Symbol == "" || summary.SampledAtMs <= 0 {
			return 0, fmt.Errorf("store: depth snapshot %+v has no identity", summary)
		}
		if _, err := stmt.ExecContext(ctx,
			summary.Source, summary.Symbol, summary.SampledAtMs, summary.VenueTimeMs,
			summary.MidPriceQuote, summary.BestBidQuote, summary.BestAskQuote,
			summary.BestBidQtyCoin, summary.BestAskQtyCoin, summary.SpreadPct,
			summary.BidDepthWithinTightQuote, summary.AskDepthWithinTightQuote,
			summary.BidDepthWithinWideQuote, summary.AskDepthWithinWideQuote,
			summary.BidLevels, summary.AskLevels, summary.BidSpanPct, summary.AskSpanPct,
			summary.IsContractBook,
		); err != nil {
			return 0, fmt.Errorf("store: insert depth %s/%s: %w", summary.Source, summary.Symbol, err)
		}
		written++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit depth: %w", err)
	}
	return written, nil
}

// DepthSnapshots reads one pair's measurements in a window, oldest first. An
// empty symbol reads every pair.
func (s *Store) DepthSnapshots(ctx context.Context, symbol string, fromMs, toMs int64) ([]depth.Summary, error) {
	// The symbol predicate is composed rather than expressed as
	// `(? = '' OR symbol = ?)`, for the same reason as in FundingHistory:
	// SQLite will not use the index through an OR on a bound parameter.
	query := `
		SELECT source, symbol, sampled_at_ms, venue_time_ms,
		       mid_price_quote, best_bid_quote, best_ask_quote,
		       best_bid_qty_coin, best_ask_qty_coin, spread_pct,
		       bid_depth_within_0_1pct_quote, ask_depth_within_0_1pct_quote,
		       bid_depth_within_0_5pct_quote, ask_depth_within_0_5pct_quote,
		       bid_levels, ask_levels, bid_span_pct, ask_span_pct, is_contract_book
		FROM depth_snapshots
		WHERE sampled_at_ms >= ? AND sampled_at_ms < ?`
	args := []any{fromMs, toMs}
	if symbol != "" {
		query += " AND symbol = ?"
		args = append(args, symbol)
	}
	query += " ORDER BY sampled_at_ms, source"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query depth snapshots: %w", err)
	}
	defer rows.Close()

	var out []depth.Summary
	for rows.Next() {
		var summary depth.Summary
		if err := rows.Scan(
			&summary.Source, &summary.Symbol, &summary.SampledAtMs, &summary.VenueTimeMs,
			&summary.MidPriceQuote, &summary.BestBidQuote, &summary.BestAskQuote,
			&summary.BestBidQtyCoin, &summary.BestAskQtyCoin, &summary.SpreadPct,
			&summary.BidDepthWithinTightQuote, &summary.AskDepthWithinTightQuote,
			&summary.BidDepthWithinWideQuote, &summary.AskDepthWithinWideQuote,
			&summary.BidLevels, &summary.AskLevels, &summary.BidSpanPct, &summary.AskSpanPct,
			&summary.IsContractBook,
		); err != nil {
			return nil, fmt.Errorf("store: scan depth snapshot: %w", err)
		}
		out = append(out, summary)
	}
	return out, rows.Err()
}
