package binance

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"futures-arbitrage-scanner/internal/broker"
)

// Venue error codes, mapped to errors a caller can branch on.
//
// Every code below is quoted from Binance's own error pages, read 2026-09-13:
//
//	https://developers.binance.com/docs/derivatives/usds-margined-futures/error-code
//	https://developers.binance.com/docs/binance-spot-api-docs/errors
//
// The mapping exists because a caller unwinding two legs cannot branch on prose.
// "Order does not exist" and "your clock is wrong" arrive in the same shape —
// an HTTP 400 with a JSON body — and call for opposite actions: the first means
// the order is not there and it is safe to proceed, the second means nothing is
// known about the order at all and proceeding is how a leg gets placed twice.
var (
	// ErrTimestampOutOfWindow is -1021 INVALID_TIMESTAMP: "Timestamp for this
	// request is outside of the recvWindow."
	//
	// broker.Client refuses to sign when it knows the clock is off by more
	// than recvWindow, so seeing this means the skew moved AFTER the last
	// measurement — the machine slept, typically. It says nothing about
	// whether the order was placed.
	ErrTimestampOutOfWindow = errors.New("binance: -1021, the timestamp was outside recvWindow; the venue did not act on this request")

	// ErrBadSignature is -1022 INVALID_SIGNATURE: "Signature for this request
	// is not valid."
	ErrBadSignature = errors.New("binance: -1022, the signature was rejected")

	// ErrKeyRejected is -2015 REJECTED_MBX_KEY: "Invalid API-key, IP, or
	// permissions for action."
	//
	// Measured 2026-09-13: this is also what the SPOT testnet answers a
	// FUTURES key, because the two testnets are separate registrations.
	ErrKeyRejected = errors.New("binance: -2015, the key, the IP or the key's permissions were rejected")

	// ErrBadPrecision is -1111 BAD_PRECISION: "Precision is over the maximum
	// defined for this asset" (futures) / "Parameter '%s' has too much
	// precision" (spot).
	//
	// Reaching this means broker.RoundOrder was not applied, or was applied
	// with another market's rules.
	ErrBadPrecision = errors.New("binance: -1111, a parameter carried more precision than the market allows")

	// ErrDuplicateClientOrderID is -4116 DUPLICATED_CLIENT_ORDER_ID:
	// "clientOrderId is duplicated".
	//
	// This is a SAFE error and an informative one: it means the venue already
	// holds an order under that id, so a retry after an ambiguous timeout has
	// been correctly refused instead of doubling the position.
	ErrDuplicateClientOrderID = errors.New("binance: -4116, that clientOrderId is already in use at the venue")

	// ErrOrderRejected is -2010 NEW_ORDER_REJECTED (spot).
	ErrOrderRejected = errors.New("binance: -2010, the order was rejected")

	// ErrFilterRejected is a symbol filter the order did not satisfy: spot
	// -1013, whose message names the filter (PRICE_FILTER, LOT_SIZE,
	// MIN_NOTIONAL, NOTIONAL, MARKET_LOT_SIZE, …).
	ErrFilterRejected = errors.New("binance: the order failed one of the symbol's filters")
)

// VenueError carries the venue's own code and message beside the mapped error,
// because the code is the thing worth pasting into a report and the mapped
// error is the thing worth branching on.
type VenueError struct {
	Code       int
	MsgVI      string // the venue's text, already scrubbed by broker.Client
	StatusCode int
	mapped     error
}

func (e *VenueError) Error() string {
	return fmt.Sprintf("binance: HTTP %d, mã %d: %s", e.StatusCode, e.Code, e.MsgVI)
}

// Unwrap exposes the mapped sentinel so errors.Is works on the meaning rather
// than on the number.
func (e *VenueError) Unwrap() error { return e.mapped }

// classify turns a broker.HTTPError into a typed error. Anything that is not a
// venue error body passes through untouched: a transport failure must NOT be
// reshaped into a statement about the order.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var httpErr *broker.HTTPError
	if !errors.As(err, &httpErr) {
		// A timeout, a DNS failure, a refused connection. The venue said
		// nothing, so nothing may be concluded about the order.
		return err
	}
	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	// The body is venue-controlled text; if it is not the documented envelope,
	// keep the HTTP error rather than inventing a code.
	if jsonErr := json.Unmarshal([]byte(httpErr.Body), &payload); jsonErr != nil || payload.Code == 0 {
		return err
	}

	ve := &VenueError{Code: payload.Code, MsgVI: payload.Msg, StatusCode: httpErr.StatusCode}
	ve.mapped = mapCode(payload.Code, payload.Msg)
	return ve
}

// mapCode is the table. Codes not listed keep no sentinel: a caller then sees
// the VenueError with its number, which is better than a wrong category.
func mapCode(code int, msg string) error {
	switch code {
	// -2013 NO_SUCH_ORDER "Order does not exist." and -2011 CANCEL_REJECTED
	// "Cancel request failure as open order not found in the orderbook:
	// 'Unknown order sent'." Both mean the venue positively has no such open
	// order, which is exactly broker.ErrOrderNotFound's contract — and it is
	// the answer that resolves an ambiguous timeout into "safe to resend".
	case -2013, -2011:
		return broker.ErrOrderNotFound
	case -1021:
		return ErrTimestampOutOfWindow
	case -1022:
		return ErrBadSignature
	case -2015:
		return ErrKeyRejected
	case -1111:
		return ErrBadPrecision
	case -4116:
		return ErrDuplicateClientOrderID
	case -2010:
		return ErrOrderRejected
	// -4164 MIN_NOTIONAL (futures): "Order's notional must be no smaller than
	// %s (unless you choose reduce only)".
	case -4164:
		return broker.ErrBelowMinNotional
	// -1013 INVALID_MESSAGE (spot): "The request is rejected by the API",
	// used for filter failures whose NAME is in the message. The notional
	// filters are singled out because the caller has a specific remedy for
	// them — trade more — while the rest mean the request was malformed.
	case -1013:
		upper := strings.ToUpper(msg)
		if strings.Contains(upper, "NOTIONAL") {
			return broker.ErrBelowMinNotional
		}
		if strings.Contains(upper, "LOT_SIZE") || strings.Contains(upper, "PRICE_FILTER") {
			return ErrFilterRejected
		}
		return ErrFilterRejected
	}
	return nil
}
