package bybit

import (
	"encoding/json"
	"strings"
	"testing"

	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// Recorded 2026-09-03 and again 2026-09-04: at depth 1, both Bybit ORDERBOOK
// streams sent nothing but snapshots across the whole window. The connector
// cannot currently tell the two apart, so a delta that removes the top level
// (size "0") would be taken at face value as a book priced at zero - the defect
// recorded in CLAUDE.md and docs/PLAN.md, still open.
//
// This test pins what the recording contains. It is what makes the defect
// falsifiable: if a re-recording ever captures an orderbook delta, this fails
// and the golden data for fixing it exists.
//
// Scoped to the orderbook TOPIC since step 2.5. The tickers channel added there
// is snapshot+delta too and its deltas are now recorded — but those are merged
// correctly (bybit_ticker.go, TestBybitTickerMerge_AbsentMeansUnchanged), and
// counting them here would fire this tripwire for a defect that is fixed,
// hiding the open one it exists to watch.
func TestBybit_TheOrderbookRecordingContainsOnlySnapshots(t *testing.T) {
	for _, source := range []string{"bybit_futures", "bybit_spot"} {
		t.Run(source, func(t *testing.T) {
			var deltas int
			for _, frame := range exchangestest.ReadFrames(t, source) {
				var message struct {
					Topic string `json:"topic"`
					Type  string `json:"type"`
				}
				if json.Unmarshal(frame, &message) != nil {
					continue
				}
				if message.Type == "delta" && strings.HasPrefix(message.Topic, "orderbook.") {
					deltas++
				}
			}
			if deltas > 0 {
				t.Errorf("the recording now holds %d orderbook delta frames; the snapshot/delta defect can and should be fixed and tested with them",
					deltas)
			}
		})
	}
}
