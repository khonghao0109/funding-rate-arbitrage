package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Operational instants written by hand or by the relaunch script — the stamp in
// `.paper/started_at`, the `-from` / `-to` edges of the gate's comparison
// window, the cut-off a new run seeds its paper book from — are parsed HERE, so
// the commands cannot drift on what "2026-09-11 14:15:50 +0700" means.
//
// This package already owns the one mapping from operator-written text to typed
// values, and a run's launch record is that same kind of text. Three private
// copies of the layout list is how cmd/scanner would one day seed from a stamp
// cmd/backtest could not read back, on a gate whose whole point is that the two
// sides agree.

// runStampLayouts are tried in order. The last one is what `.paper/started_at`
// holds, so the run's own record can be pasted into any of these flags.
var runStampLayouts = []string{"2006-01-02", "2006-01-02T15:04:05", time.RFC3339, "2006-01-02 15:04:05 -0700"}

// ParseRunStamp reads one operational instant: a date (midnight UTC), a UTC
// date-time, or a date-time carrying its own zone. The result is UTC whatever
// was written.
func ParseRunStamp(s string) (time.Time, error) {
	for _, layout := range runStampLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("want YYYY-MM-DD, YYYY-MM-DDTHH:MM:SS (UTC), RFC3339, or 'YYYY-MM-DD HH:MM:SS -0700'")
}

// ReadRunStamp reads a launch record such as `.paper/started_at`.
//
// A missing file comes back as os.ErrNotExist for the CALLER to decide about:
// to the paper ledger it means "no window was given, read the whole journal",
// and to a scanner told to seed from that file it is a misconfiguration that
// must stop the launch. Choosing between those here would make one of the two
// wrong.
func ReadRunStamp(path string) (time.Time, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, err
	}
	t, err := ParseRunStamp(strings.TrimSpace(string(raw)))
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}
