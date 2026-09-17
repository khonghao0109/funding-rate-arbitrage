package scanner

import (
	"sort"
	"strconv"
	"time"
)

// Episodes of a wide cross-venue spread, for the event log (PLAN 4.5i).
//
// The tracker is pure: it turns successive radar snapshots into episode
// transitions and knows nothing of storage, so cmd/scanner owns the writes
// (internal/scanner must not learn about internal/store).
//
// # What an episode measures
//
// A FORMING-rate episode, sampled every few seconds: from the first reading at
// or above a threshold to the first reading below it that stayed below for the
// grace period. That is a live-market counterpart of the three-year corpus's
// "consecutive days ≥ 15% of SETTLED funding", not the same quantity — a
// forming spread can open and close inside one settlement period and pay
// nothing. The resolution is the sampling period, and the log says which.
//
// An episode is keyed by symbol and threshold, and it has one direction: a
// spread that flips sign while above the threshold ends one episode and starts
// another, because the two are opposite trades.
//
// An episode with no live funding on either venue is ENDED as "stale" at the
// last instant both were live — a silence is not an episode continuing.

// Episode end reasons.
const (
	CrossEndBelow   = "below"   // stayed under the threshold for the grace period
	CrossEndFlip    = "flip"    // the spread changed sign while above the threshold
	CrossEndStale   = "stale"   // funding stopped being live on a venue
	CrossEndRestart = "restart" // the process stopped while the episode was open
)

// CrossEpisode is one episode as the tracker knows it.
type CrossEpisode struct {
	Symbol          string
	ThresholdAPRPct float64
	ShortSource     string
	LongSource      string
	StartedAtMs     int64
	PeakGrossAPRPct float64
	PeakAtMs        int64
	LastSeenAboveMs int64
	FirstBelowAtMs  int64 // 0 while above
	FirstNotLiveMs  int64 // 0 while both venues' funding is live
	EndedAtMs       int64 // 0 while open
	EndReason       string
	SampleEverySec  int64
}

// DurationSec is ended − started; 0 while open.
func (e CrossEpisode) DurationSec() float64 {
	if e.EndedAtMs == 0 {
		return 0
	}
	return float64(e.EndedAtMs-e.StartedAtMs) / 1000
}

// CrossTransitions is what one observation changed. Every episode in Opened,
// Updated and Closed must be written; Updated carries the new peak or
// last-seen stamp of an episode still open.
type CrossTransitions struct {
	Opened  []CrossEpisode
	Updated []CrossEpisode
	Closed  []CrossEpisode
}

// CrossEventTracker follows episodes across observations. Not safe for
// concurrent use: one job owns it.
type CrossEventTracker struct {
	thresholds     []float64
	endBelow       time.Duration
	sampleEverySec int64
	open           map[string]*CrossEpisode // symbol|threshold
}

// NewCrossEventTracker builds a tracker for the configured thresholds.
func NewCrossEventTracker(thresholdsAPRPct []float64, endBelow time.Duration, sampleEverySec int64) *CrossEventTracker {
	th := append([]float64(nil), thresholdsAPRPct...)
	sort.Float64s(th)
	return &CrossEventTracker{thresholds: th, endBelow: endBelow, sampleEverySec: sampleEverySec,
		open: map[string]*CrossEpisode{}}
}

func episodeKey(symbol string, threshold float64) string {
	return symbol + "|" + strconv.FormatFloat(threshold, 'g', -1, 64)
}

// Observe folds one radar snapshot into the open episodes.
func (t *CrossEventTracker) Observe(snap CrossRadarSnapshot) CrossTransitions {
	var tr CrossTransitions
	nowMs := snap.UpdatedAtMs
	seen := map[string]bool{}
	for _, p := range snap.Pairs {
		live := p.A.FundingStatus == statusLive && p.B.FundingStatus == statusLive
		for _, th := range t.thresholds {
			key := episodeKey(p.Symbol, th)
			seen[key] = true
			ep := t.open[key]
			above := live && p.ShortSource != "" && p.GrossAPRPct >= th

			if ep != nil && !live {
				// A blip is not an end: a failed poll or a settlement boundary
				// would otherwise cut the longest episodes — the ones this log
				// exists to measure. Only a silence as long as the grace ends
				// it, stamped at the last instant it was known to be above.
				if ep.FirstNotLiveMs == 0 {
					ep.FirstNotLiveMs = nowMs
				}
				// A spread that had already stayed below for the grace ENDED; the
				// data gap after it does not turn a measurement into a lower bound.
				if ep.FirstBelowAtMs > 0 && time.Duration(nowMs-ep.FirstBelowAtMs)*time.Millisecond >= t.endBelow {
					tr.Closed = append(tr.Closed, t.close(key, ep, ep.FirstBelowAtMs, CrossEndBelow))
					continue
				}
				if time.Duration(nowMs-ep.FirstNotLiveMs)*time.Millisecond >= t.endBelow {
					tr.Closed = append(tr.Closed, t.close(key, ep, ep.LastSeenAboveMs, CrossEndStale))
				}
				continue
			}
			if ep != nil {
				ep.FirstNotLiveMs = 0
			}
			if ep != nil && above && p.ShortSource != ep.ShortSource {
				end := nowMs
				if ep.FirstBelowAtMs > 0 {
					end = ep.FirstBelowAtMs
				}
				tr.Closed = append(tr.Closed, t.close(key, ep, end, CrossEndFlip))
				ep = nil
			}
			switch {
			case ep == nil && above:
				ep = &CrossEpisode{Symbol: p.Symbol, ThresholdAPRPct: th,
					ShortSource: p.ShortSource, LongSource: p.LongSource,
					StartedAtMs: nowMs, PeakGrossAPRPct: p.GrossAPRPct, PeakAtMs: nowMs,
					LastSeenAboveMs: nowMs, SampleEverySec: t.sampleEverySec}
				t.open[key] = ep
				tr.Opened = append(tr.Opened, *ep)
			case ep != nil && above:
				ep.LastSeenAboveMs, ep.FirstBelowAtMs = nowMs, 0
				if p.GrossAPRPct > ep.PeakGrossAPRPct {
					ep.PeakGrossAPRPct, ep.PeakAtMs = p.GrossAPRPct, nowMs
				}
				tr.Updated = append(tr.Updated, *ep)
			case ep != nil: // live, below
				if ep.FirstBelowAtMs == 0 {
					ep.FirstBelowAtMs = nowMs
				}
				if time.Duration(nowMs-ep.FirstBelowAtMs)*time.Millisecond >= t.endBelow {
					tr.Closed = append(tr.Closed, t.close(key, ep, ep.FirstBelowAtMs, CrossEndBelow))
				}
			}
		}
	}
	// A symbol that dropped out of the radar entirely has no live funding.
	for key, ep := range t.open {
		if !seen[key] {
			tr.Closed = append(tr.Closed, t.close(key, ep, ep.LastSeenAboveMs, CrossEndStale))
		}
	}
	sortEpisodes(tr.Opened)
	sortEpisodes(tr.Updated)
	sortEpisodes(tr.Closed)
	return tr
}

func (t *CrossEventTracker) close(key string, ep *CrossEpisode, endedAtMs int64, reason string) CrossEpisode {
	ep.EndedAtMs, ep.EndReason = endedAtMs, reason
	delete(t.open, key)
	return *ep
}

// OpenCount is how many episodes are open.
func (t *CrossEventTracker) OpenCount() int { return len(t.open) }

func sortEpisodes(eps []CrossEpisode) {
	sort.Slice(eps, func(i, j int) bool {
		if eps[i].Symbol != eps[j].Symbol {
			return eps[i].Symbol < eps[j].Symbol
		}
		return eps[i].ThresholdAPRPct < eps[j].ThresholdAPRPct
	})
}
