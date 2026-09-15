// Command refusalprobe is a binary that `go test` did NOT build, run by
// TestNewClient_RefusesATestTransportInABinaryGoTestDidNotBuild with `go run`.
// It hands NewClient a TestTransport and reports whether it was refused and
// whether the transport was ever reached. It opens no socket either way.
//
// It lives under testdata/ so that `./...` never builds it, and it is the one
// file outside internal/broker's own sources that the TestTransport source
// guard allows to name the hook.
package main

import (
	"errors"
	"fmt"
	"net/http"

	"futures-arbitrage-scanner/internal/broker"
)

type counting struct{ reached *int }

func (c counting) RoundTrip(*http.Request) (*http.Response, error) {
	*c.reached++
	return nil, errors.New("refusalprobe: the test transport was used")
}

func main() {
	reached := 0
	c, err := broker.NewClient(broker.Config{
		BaseURL:           broker.BinanceFuturesTestnetBaseURL,
		Credentials:       broker.Credentials{APIKey: broker.NewSecret("probe-key"), APISecret: broker.NewSecret("probe-secret")},
		WeightLimitPerMin: broker.BinanceFuturesWeightPerMin,
		TestTransport:     counting{&reached},
	})
	fmt.Printf("refused=%v client=%v reached=%d\n", errors.Is(err, broker.ErrTestTransportOutsideTest), c != nil, reached)
}
