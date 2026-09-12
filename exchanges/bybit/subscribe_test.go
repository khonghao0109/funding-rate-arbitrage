package bybit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// The refusal is a REAL frame, captured from wss://stream.bybit.com/v5/public/spot
// on 2026-09-12 by sending the 26-arg subscribe the shipped 13-pair config
// produced. The session that received it went on to deliver 0 data frames,
// while the same probe with 8 args received {"success":true} and 99 frames in
// twelve seconds.
//
// It is kept as testdata rather than inline because it is evidence: this exact
// string is why bybit_spot contributed nothing to step-3.5 run 2.
func refusalFrame(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "bybit_spot_subscribe_refused.json"))
	if err != nil {
		t.Fatal(err)
	}
	return []byte(strings.TrimSpace(string(raw)))
}

// A refused subscription must not read as market data — and must not read as
// "nothing happened" either. Before 2026-09-12 this frame fell through every
// decode in the handler and was counted by the lifecycle as a frame off the
// socket, which is how a socket subscribed to NOTHING kept resetting the
// backoff and looking healthy for nineteen hours.
func TestSubscribeRefusal_IsNotDataAndIsReported(t *testing.T) {
	r := exchangestest.NewRecorder(t)
	frame := refusalFrame(t)

	if produced := goldenConfigs(r.Feeds)["bybit_spot"].Handle(frame, time.Now()); produced {
		t.Error("a refused subscribe reported itself as data; the data-silence deadline would never fire")
	}
	if books, trades, prices := r.Orderbooks(), r.Trades(), r.Prices(); len(books)+len(trades)+len(prices) != 0 {
		t.Errorf("a refusal produced %d books, %d trades, %d prices", len(books), len(trades), len(prices))
	}

	reply, ok := decodeBybitOpReply(frame)
	if !ok {
		t.Fatal("the refusal did not decode as an op reply, so nothing can log why the feed is empty")
	}
	if reply.Op != "subscribe" || reply.Success {
		t.Errorf("decoded %+v, want op=subscribe success=false", reply)
	}
	if !strings.Contains(reply.RetMsg, "args size") {
		t.Errorf("ret_msg %q lost the venue's reason", reply.RetMsg)
	}
}

// The keepalive reply and the successful acknowledgement share that shape and
// must also answer false: they are the frames that kept the old read deadline
// alive forever.
func TestOpReplies_AreNeverData(t *testing.T) {
	for name, frame := range map[string]string{
		"pong":             `{"success":true,"ret_msg":"pong","conn_id":"x","op":"ping"}`,
		"subscribe ack":    `{"success":true,"ret_msg":"subscribe","conn_id":"x","op":"subscribe"}`,
		"refused with ack": `{"success":false,"ret_msg":"args size >10","conn_id":"x","op":"subscribe"}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := exchangestest.NewRecorder(t)
			if goldenConfigs(r.Feeds)["bybit_spot"].Handle([]byte(frame), time.Now()) {
				t.Errorf("%s reported itself as data", name)
			}
		})
	}
}

// And the recorded book frames must answer true, or the deadline would tear
// down a healthy feed — the failure mode that is worse than the one being
// fixed. Replayed from the same recording the golden tests use.
func TestRecordedSpotFrames_ReportTheDataTheyProduce(t *testing.T) {
	r := exchangestest.NewRecorder(t)
	cfg := goldenConfigs(r.Feeds)["bybit_spot"]

	produced, total := 0, 0
	for _, frame := range exchangestest.ReadFrames(t, "bybit_spot") {
		total++
		if cfg.Handle(frame, time.Now()) {
			produced++
		}
	}
	if total == 0 {
		t.Fatal("no recorded frames")
	}
	if produced == 0 {
		t.Fatalf("none of %d recorded bybit_spot frames reported data; a live feed would be torn down every data_silence_sec", total)
	}
	// The recording opens with the subscribe acknowledgement, so it can never
	// be all of them either.
	if produced == total {
		t.Errorf("all %d frames reported data, acknowledgement included", total)
	}
	t.Logf("%d of %d recorded bybit_spot frames carried data", produced, total)
}

// Bybit documents a 10-arg ceiling per subscribe request on SPOT and none on
// futures. Splitting is what keeps a 13-pair config working; dropping topics to
// fit would be the same silent gap in a quieter form.
// https://bybit-exchange.github.io/docs/v5/ws/connect
func TestBybitArgBatches_RespectTheVenuesDocumentedCeiling(t *testing.T) {
	args := make([]string, 26)
	for i := range args {
		args[i] = string(rune('a' + i))
	}

	cases := []struct {
		name    string
		maxArgs int
		batches int
	}{
		{"spot, the 13-pair shipped config", bybitSpotMaxArgsPerRequest, 3},
		{"spot, exactly at the ceiling", 26, 1},
		{"futures, no documented ceiling", 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			batches := bybitArgBatches(args, tc.maxArgs)
			if len(batches) != tc.batches {
				t.Errorf("%d batches, want %d", len(batches), tc.batches)
			}
			var flat []string
			for _, batch := range batches {
				if tc.maxArgs > 0 && len(batch) > tc.maxArgs {
					t.Errorf("a batch carries %d args, over the %d ceiling", len(batch), tc.maxArgs)
				}
				if len(batch) == 0 {
					t.Error("an empty subscribe request would be sent")
				}
				flat = append(flat, batch...)
			}
			// Every topic must survive exactly once, in order: a dropped one is
			// a market missing from the dashboard with nothing to say so.
			if len(flat) != len(args) {
				t.Fatalf("%d args across the batches, want %d", len(flat), len(args))
			}
			for i := range args {
				if flat[i] != args[i] {
					t.Fatalf("arg %d is %q, want %q", i, flat[i], args[i])
				}
			}
		})
	}
}

// The ceiling applies to SPOT only, and the test must reach it the way
// production does. It goes through spotStream/futuresStream — the two
// constructors ConnectSpot and ConnectFutures call — so the line that carries
// the ceiling into production is the line under test. Building the stream with
// the constant supplied by the test would prove only that bybitArgBatches can
// count, which the review demonstrated by restoring the bug and watching the
// suite stay green.
func TestConnectors_ApplyTheCeilingWhereTheVenueDocumentsIt(t *testing.T) {
	thirteen := make([]exchanges.Symbol, 13)
	for i := range thirteen {
		thirteen[i] = exchanges.Symbol{Standard: "X", Venue: string(rune('A' + i))}
	}
	feeds := exchangestest.NewRecorder(t).Feeds

	for name, tc := range map[string]struct {
		stream   func(string, []exchanges.Symbol, exchanges.Feeds) exchanges.StreamConfig
		requests int
		topics   int
		maxArgs  int
	}{
		// 13 symbols x 2 topics = 26 args, ceiling 10 -> 3 requests.
		"spot": {spotStream, 3, 26, bybitSpotMaxArgsPerRequest},
		// 13 symbols x 3 topics = 39 args, no ceiling -> 1 request.
		"futures": {futuresStream, 1, 39, 0},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := tc.stream("bybit_"+name, thirteen, feeds)
			recorder := &recordingConn{}
			if err := cfg.Subscribe(recorder); err != nil {
				t.Fatal(err)
			}
			if len(recorder.requests) != tc.requests {
				t.Errorf("%d subscribe requests, want %d", len(recorder.requests), tc.requests)
			}
			seen := 0
			for _, batch := range recorder.requests {
				if tc.maxArgs > 0 && len(batch) > tc.maxArgs {
					t.Errorf("a request carries %d args, over the venue's %d", len(batch), tc.maxArgs)
				}
				seen += len(batch)
			}
			if seen != tc.topics {
				t.Errorf("%d topics subscribed in total, want %d — a topic short is a market missing from the dashboard", seen, tc.topics)
			}
		})
	}
}

// recordingConn stands in for the socket and keeps the args of every subscribe
// request, which is the thing the venue counts.
type recordingConn struct {
	requests [][]string
}

func (c *recordingConn) WriteJSON(v any) error {
	message, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("subscribe wrote %T, not a JSON object", v)
	}
	args, ok := message["args"].([]string)
	if !ok {
		return fmt.Errorf("subscribe wrote args of type %T", message["args"])
	}
	c.requests = append(c.requests, args)
	return nil
}
