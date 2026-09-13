package binance

import (
	"errors"
	"fmt"
	"testing"

	"futures-arbitrage-scanner/internal/broker"
)

// Every code in the table, with the message the venue documents for it.
//
// The mapping matters because two of these call for opposite actions and
// arrive in the same shape. -2013 says the venue positively has no such order,
// which is what makes a resend safe; -1021 says the venue did not act on the
// request at all and NOTHING is known about the order. A caller that treated
// the second as the first would place a leg twice.
func TestClassify_MapsTheDocumentedCodes(t *testing.T) {
	cases := []struct {
		code int
		msg  string
		want error
	}{
		{-2013, "Order does not exist.", broker.ErrOrderNotFound},
		{-2011, "Cancel request failure as open order not found in the orderbook: 'Unknown order sent'.", broker.ErrOrderNotFound},
		{-1021, "Timestamp for this request is outside of the recvWindow.", ErrTimestampOutOfWindow},
		{-1022, "Signature for this request is not valid.", ErrBadSignature},
		{-2015, "Invalid API-key, IP, or permissions for action.", ErrKeyRejected},
		{-1111, "Precision is over the maximum defined for this asset.", ErrBadPrecision},
		{-4116, "clientOrderId is duplicated", ErrDuplicateClientOrderID},
		{-2010, "NEW_ORDER_REJECTED", ErrOrderRejected},
		{-4164, "Order's notional must be no smaller than 5.0 (unless you choose reduce only)", broker.ErrBelowMinNotional},
		{-1013, "Filter failure: NOTIONAL", broker.ErrBelowMinNotional},
		{-1013, "Filter failure: MIN_NOTIONAL", broker.ErrBelowMinNotional},
		{-1013, "Filter failure: LOT_SIZE", ErrFilterRejected},
		{-1013, "Filter failure: PRICE_FILTER", ErrFilterRejected},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d %s", tc.code, tc.msg), func(t *testing.T) {
			err := classify(&broker.HTTPError{
				StatusCode: 400,
				URL:        "https://demo-fapi.binance.com/fapi/v1/order",
				Body:       fmt.Sprintf(`{"code":%d,"msg":%q}`, tc.code, tc.msg),
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("classify(%d) = %v, want errors.Is(..., %v)", tc.code, err, tc.want)
			}
			// The venue's own number survives, because that is the thing worth
			// pasting into a report.
			var ve *VenueError
			if !errors.As(err, &ve) {
				t.Fatalf("the error is not a *VenueError: %v", err)
			}
			if ve.Code != tc.code {
				t.Errorf("Code = %d, want %d", ve.Code, tc.code)
			}
		})
	}
}

// A timeout is NOT a statement about the order, and must not be reshaped into
// one. This is the single most dangerous mapping error possible here.
func TestClassify_LeavesNonVenueErrorsAlone(t *testing.T) {
	transport := errors.New("dial tcp: i/o timeout")
	got := classify(transport)
	if !errors.Is(got, transport) {
		t.Fatalf("a transport error was reshaped into %v", got)
	}
	if errors.Is(got, broker.ErrOrderNotFound) {
		t.Fatal("a transport failure satisfies ErrOrderNotFound; a caller would treat an unknown outcome as 'never arrived' and resend")
	}
}

// An HTTP error whose body is not the documented envelope keeps its HTTP
// identity rather than acquiring an invented code.
func TestClassify_ANonEnvelopeBodyIsNotGivenACode(t *testing.T) {
	httpErr := &broker.HTTPError{StatusCode: 502, URL: "https://demo-fapi.binance.com/fapi/v1/order", Body: "<html>bad gateway</html>"}
	got := classify(httpErr)
	var ve *VenueError
	if errors.As(got, &ve) {
		t.Fatalf("an HTML error page was decoded into venue code %d", ve.Code)
	}
	if !errors.Is(got, error(httpErr)) {
		t.Errorf("the HTTP error was lost: %v", got)
	}
}

// A code the table does not list keeps its number and gains no category: a
// wrong category is worse than none.
func TestClassify_AnUnknownCodeKeepsItsNumberAndNoCategory(t *testing.T) {
	err := classify(&broker.HTTPError{StatusCode: 400, Body: `{"code":-9999,"msg":"something new"}`})
	var ve *VenueError
	if !errors.As(err, &ve) {
		t.Fatalf("want a *VenueError, got %v", err)
	}
	if ve.Code != -9999 {
		t.Errorf("Code = %d, want -9999", ve.Code)
	}
	for _, sentinel := range []error{
		broker.ErrOrderNotFound, ErrTimestampOutOfWindow, ErrBadSignature,
		ErrBadPrecision, broker.ErrBelowMinNotional, ErrKeyRejected,
	} {
		if errors.Is(err, sentinel) {
			t.Errorf("an unmapped code was categorised as %v", sentinel)
		}
	}
}
