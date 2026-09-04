package store

import (
	"context"
	"fmt"
	"time"
)

// Retention is how long each series is kept, in days.
//
// The two numbers come from PLAN.md 2.6 and describe different things. Funding
// is the backtest corpus and a year of it is the point of collecting any;
// price samples are a diagnostic series with a row count linear in the sampling
// period, and three months of them at 5s is already ~56 million rows.
//
// Instrument snapshots are NOT pruned and take no field here. They are the
// record of what the rules were on a given day, which is exactly what a
// backtest of a year ago needs and what nothing can reconstruct once deleted —
// and at 36 markets a day the whole history is a rounding error next to one
// day of price samples.
type Retention struct {
	FundingDays int
	PriceDays   int

	// DepthDays keeps the step-2.7b snapshots. It gets its own number because
	// the series has a different shape from both of the others: one row per
	// market per sweep, so an hourly sweep of 36 markets is ~315k rows a year —
	// small enough to keep long, and unlike funding it can NEVER be re-fetched,
	// because a venue does not publish the book it had last Tuesday.
	DepthDays int
}

// PruneResult is what one pruning pass removed.
type PruneResult struct {
	FundingRows int64
	PriceRows   int64
	DepthRows   int64
}

// Prune deletes rows older than the policy allows.
//
// A zero or negative day count means KEEP EVERYTHING for that series, not
// "delete everything". This asymmetry is deliberate: an unset field in a config
// file must never be the instruction that empties a corpus, and the opposite
// default would make a typo in config.yaml destroy a year of collection with no
// way back.
func (s *Store) Prune(ctx context.Context, policy Retention) (PruneResult, error) {
	var result PruneResult
	now := s.now()

	if policy.FundingDays > 0 {
		cutoffMs := now.AddDate(0, 0, -policy.FundingDays).UnixMilli()
		removed, err := s.deleteOlderThan(ctx, "funding_history", "funding_at_ms", cutoffMs)
		if err != nil {
			return result, err
		}
		result.FundingRows = removed
	}
	if policy.PriceDays > 0 {
		cutoffMs := now.AddDate(0, 0, -policy.PriceDays).UnixMilli()
		removed, err := s.deleteOlderThan(ctx, "price_snapshots", "sampled_at_ms", cutoffMs)
		if err != nil {
			return result, err
		}
		result.PriceRows = removed
	}
	if policy.DepthDays > 0 {
		cutoffMs := now.AddDate(0, 0, -policy.DepthDays).UnixMilli()
		removed, err := s.deleteOlderThan(ctx, "depth_snapshots", "sampled_at_ms", cutoffMs)
		if err != nil {
			return result, err
		}
		result.DepthRows = removed
	}
	return result, nil
}

// deleteOlderThan is the one place a DELETE is issued. The table and column are
// not user input — they are the two literals above — but they are still
// interpolated rather than bound, because SQL cannot bind an identifier.
func (s *Store) deleteOlderThan(ctx context.Context, table, column string, cutoffMs int64) (int64, error) {
	result, err := s.db.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM %s WHERE %s < ?", table, column), cutoffMs)
	if err != nil {
		return 0, fmt.Errorf("store: prune %s: %w", table, err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, nil // the delete succeeded; only the count is unavailable
	}
	return removed, nil
}

// SetClock replaces the store's clock. Only tests call it: pruning is defined
// in days and waiting a year for one is not a test.
func (s *Store) SetClock(now func() time.Time) { s.now = now }
