package exchangestest

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
)

// Golden files have to come from the venues, not from anyone's memory of what a
// venue sends. This is the tool that fetches them.
//
// Capture is skipped unless CAPTURE_TESTDATA=1, because it opens real sockets -
// `go test ./...` must stay offline and fast. Each venue package owns a capture
// test that builds its REAL StreamConfig - the same URL and the same
// subscription production uses - and hands it here; only Handle is swapped. A
// capture tool with its own copy of the subscribe messages would drift from the
// connector and record payloads nobody actually receives, which is the failure
// mode golden tests exist to prevent.

const (
	CaptureEnv = "CAPTURE_TESTDATA"
	// CaptureDuration is how long each venue is listened to. It has to be long
	// enough for the SLOWEST channel a connector subscribes to: OKX pushed 76
	// trades before its first books5 frame.
	CaptureDuration = 30 * time.Second
	// framesPerKind bounds how many of each message shape are kept. Keeping the
	// first N frames overall does not work - one chatty channel starves every
	// other.
	framesPerKind = 8

	// framesPerSequenceKind is the cap for a message shape whose MEANING is a
	// sequence rather than a sample. Kraken's book deltas are the only such
	// shape: the connector's top of book is the result of every frame since the
	// snapshot, so a recording has to hold enough of them to actually reach the
	// best level.
	framesPerSequenceKind = 40
)

// sequenceKindPrefix marks the frame kinds capped by framesPerSequenceKind.
// frameKind renders Kraken book updates as "feed=book|<product>".
const sequenceKindPrefix = "feed=book|"

// SkipUnlessCapture skips the calling test unless re-recording was requested.
func SkipUnlessCapture(t *testing.T) {
	t.Helper()
	if os.Getenv(CaptureEnv) != "1" {
		t.Skipf("set %s=1 to re-record golden payloads from the live venues", CaptureEnv)
	}
}

// CaptureFrames records raw frames from one venue for CaptureDuration, keeping
// at most framesPerKind of each message shape. Arrival order is preserved,
// which matters: Kraken's book_snapshot has to come before the deltas that
// build on it, or the golden file describes a sequence that could not have
// happened.
func CaptureFrames(t *testing.T, source string, cfg exchanges.StreamConfig) [][]byte {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), CaptureDuration)
	defer cancel()

	var frames [][]byte
	kept := map[string]int{}
	done := make(chan struct{})

	recording := cfg
	// The capture keeps frames; it publishes nothing, so it answers false to
	// StreamConfig.Handle's "did this produce a message". That is honest and
	// harmless here: Feeds carries no DataSilenceTimeout in this harness, so
	// the data deadline is off and a capture session is never torn down for
	// producing nothing.
	recording.Handle = func(raw []byte, _ time.Time) bool {
		kind := frameKind(raw, Symbols(source))
		limit := framesPerKind
		if strings.HasPrefix(kind, sequenceKindPrefix) {
			limit = framesPerSequenceKind
		}
		if kept[kind] >= limit {
			return false
		}
		kept[kind]++
		frames = append(frames, bytes.Clone(raw))
		return false
	}

	go func() {
		defer close(done)
		exchanges.RunStream(exchanges.Feeds{Ctx: ctx}, recording)
	}()
	<-done

	t.Logf("%s: %d shapes", source, len(kept))
	return frames
}

// frameKind fingerprints a message so the recording keeps a spread of shapes
// rather than a spread of timestamps.
//
// The fingerprint is the venue's own discriminator fields plus which subscribed
// market the frame mentions. The market half is what forces a usable recording
// out of Paradex: it streams a summary for EVERY market it lists, hundreds of
// them including options, and without it a recording is a random slice of
// instruments this scanner does not follow.
func frameKind(raw []byte, symbols []exchanges.Symbol) string {
	market := ""
	for _, symbol := range symbols {
		if bytes.Contains(raw, []byte(`"`+symbol.Venue+`"`)) {
			market = symbol.Venue
			break
		}
	}

	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) != nil {
		return "non-json|" + market
	}

	var parts []string
	for _, key := range []string{"stream", "topic", "channel", "feed", "method", "event", "type"} {
		value, ok := probe[key]
		if !ok {
			continue
		}
		parts = append(parts, key+"="+strings.Trim(string(value), `"`))
	}
	// OKX names the channel inside arg, and Paradex inside params.
	for _, nested := range []string{"arg", "params"} {
		value, ok := probe[nested]
		if !ok {
			continue
		}
		var inner struct {
			Channel string `json:"channel"`
		}
		if json.Unmarshal(value, &inner) == nil && inner.Channel != "" {
			parts = append(parts, nested+".channel="+inner.Channel)
		}
	}
	if len(parts) == 0 {
		for key := range probe {
			parts = append(parts, key)
		}
		sort.Strings(parts)
	}

	return strings.Join(parts, "|") + "|" + market
}

// WriteFrames stores one frame per line in the calling package's testdata, so a
// golden file stays greppable and diffs one message at a time.
//
// JSON frames are compacted first. Paradex pretty-prints its payloads across
// several lines, which the format cannot hold. Compaction removes only
// insignificant whitespace - every key, every value and every type survives
// exactly - and these files exist to test PARSING, for which whitespace is by
// definition not part of the input. A non-JSON frame (OKX answers a keepalive
// with the bare word "pong") is stored as it arrived.
func WriteFrames(t *testing.T, source string, frames [][]byte) {
	t.Helper()

	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatalf("create testdata: %v", err)
	}

	var out bytes.Buffer
	for _, frame := range frames {
		var compact bytes.Buffer
		if err := json.Compact(&compact, frame); err == nil {
			frame = compact.Bytes()
		}
		if bytes.ContainsAny(frame, "\r\n") {
			t.Errorf("%s sent a non-JSON frame containing a newline; the format cannot hold it", source)
			continue
		}
		out.Write(frame)
		out.WriteByte('\n')
	}

	path := filepath.Join("testdata", source+".jsonl")
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TrimInstrumentList re-encodes a { "<arrayKey>": [ {symbol: ...}, ... ] }
// response keeping only the requested symbols. Used by the capture test for
// the two venues whose only endpoint is an unfiltered ~1MB list (Binance
// futures, Kraken): kept entries are byte-verbatim, the array is filtered.
func TrimInstrumentList(raw []byte, arrayKey string, natives map[string]bool) ([]byte, error) {
	var full map[string]json.RawMessage
	if err := json.Unmarshal(raw, &full); err != nil {
		return nil, err
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(full[arrayKey], &entries); err != nil {
		return nil, err
	}
	kept := make([]json.RawMessage, 0, len(natives))
	for _, entry := range entries {
		var head struct {
			Symbol string `json:"symbol"`
		}
		if err := json.Unmarshal(entry, &head); err != nil {
			return nil, err
		}
		if natives[head.Symbol] {
			kept = append(kept, entry)
		}
	}
	keptRaw, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]json.RawMessage{arrayKey: keptRaw})
}
