package exchanges

import (
	"context"
	"testing"
)

// SendFunding must obey the same contract as SendPrice: deliver, or give up
// when the context is cancelled so a connector can never block on shutdown.
func TestSendFunding_GivesUpOnCancelledContext(t *testing.T) {
	ch := make(chan FundingData, 1)
	ctx, cancel := context.WithCancel(context.Background())
	f := Feeds{Ctx: ctx, Funding: ch}

	if ok := f.SendFunding(FundingData{Symbol: "BTCUSDT"}); !ok {
		t.Fatal("SendFunding with buffer space should deliver")
	}
	// Buffer now full and nobody draining: only the cancellation can free it.
	cancel()
	if ok := f.SendFunding(FundingData{Symbol: "ETHUSDT"}); ok {
		t.Fatal("SendFunding after cancel with a full buffer should give up")
	}
}
