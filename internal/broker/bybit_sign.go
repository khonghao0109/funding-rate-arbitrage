package broker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
)

// Bybit V5's signature, which differs from Binance's in WHERE it travels and
// WHAT it covers (PLAN 4.5i).
//
// From the V5 integration guide, "Authentication for API":
// https://bybit-exchange.github.io/docs/v5/guide#authentication
//
//   - The request carries X-BAPI-API-KEY, X-BAPI-TIMESTAMP (ms),
//     X-BAPI-RECV-WINDOW and X-BAPI-SIGN as HEADERS.
//   - The string signed is timestamp + api_key + recv_window + payload, where
//     payload is the query string for a GET and the raw JSON body for a POST.
//   - HMAC-SHA256 under the secret, lowercase hex.
//
// Nothing about the signature is on the URL, so the query-string leak door that
// redactURL guards for Binance is not the door here; the header is, and Go's
// transport logs headers only under GODEBUG=http2debug, which NewClient refuses.

// SignBybitV5 is the V5 HMAC-SHA256 over timestamp + api key + recv_window +
// payload, lowercase hex. payload must be the finished query string or the exact
// body bytes sent.
func SignBybitV5(secret Secret, timestampMs int64, apiKey Secret, recvWindowMs int64, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret.Expose()))
	mac.Write([]byte(strconv.FormatInt(timestampMs, 10)))
	mac.Write([]byte(apiKey.Expose()))
	mac.Write([]byte(strconv.FormatInt(recvWindowMs, 10)))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// The header names, spelled as the guide spells them. http.Header canonicalises
// them on Set; the venue matches case-insensitively as HTTP requires.
const (
	bybitHeaderAPIKey     = "X-BAPI-API-KEY"
	bybitHeaderTimestamp  = "X-BAPI-TIMESTAMP"
	bybitHeaderRecvWindow = "X-BAPI-RECV-WINDOW"
	bybitHeaderSign       = "X-BAPI-SIGN"
)

func setBybitV5Headers(h http.Header, creds Credentials, timestampMs, recvWindowMs int64, payload string) {
	h.Set(bybitHeaderAPIKey, creds.APIKey.Expose())
	h.Set(bybitHeaderTimestamp, strconv.FormatInt(timestampMs, 10))
	h.Set(bybitHeaderRecvWindow, strconv.FormatInt(recvWindowMs, 10))
	h.Set(bybitHeaderSign, SignBybitV5(creds.APISecret, timestampMs, creds.APIKey, recvWindowMs, payload))
}

// bybitServerTimeResponse is GET /v5/market/time:
//
//	{"retCode":0,"retMsg":"OK","result":{"timeSecond":"1688639403",
//	 "timeNano":"1688639403423213947"},"retExtInfo":{},"time":1688639403423}
//
// https://bybit-exchange.github.io/docs/v5/market/time
//
// result.timeNano is the server's clock; the top-level `time` is when the
// response was stamped. They are the same instant to within the handler, and
// timeNano is read because it is the documented answer to "what time is it".
type bybitServerTimeResponse struct {
	RetCode int    `json:"retCode"`
	RetMsg  string `json:"retMsg"`
	Result  struct {
		TimeSecond string `json:"timeSecond"`
		TimeNano   string `json:"timeNano"`
	} `json:"result"`
}

func (r bybitServerTimeResponse) serverTimeMs() (int64, error) {
	if r.RetCode != 0 {
		return 0, errors.New("the venue answered a non-zero retCode for its own clock")
	}
	nano, err := strconv.ParseInt(r.Result.TimeNano, 10, 64)
	if err != nil || nano <= 0 {
		return 0, errors.New("the venue answered no usable result.timeNano")
	}
	return nano / 1_000_000, nil
}
