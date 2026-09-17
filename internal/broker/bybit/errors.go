package bybit

import (
	"errors"
	"fmt"
	"net/http"
	"unicode/utf8"

	"futures-arbitrage-scanner/internal/broker"
)

// Venue codes mapped to errors a caller can branch on. Every code is quoted
// from https://bybit-exchange.github.io/docs/v5/error (UTA section), read
// 2026-09-17 from the docs repository's master branch.
var (
	// 10002 "The request time exceeds the time window range." broker.Client
	// refuses to sign on a skew it knows is too wide, so this means the clock
	// moved after the last measurement. The venue did not act on the request.
	ErrTimestampOutOfWindow = errors.New("bybit: 10002, the timestamp was outside recv_window; the venue did not act on this request")

	// 10003 "API key is invalid. Check whether the key and domain are matched,
	// there are 4 env: mainnet, testnet, mainnet-demo, testnet-demo".
	ErrKeyInvalid = errors.New("bybit: 10003, the API key is invalid for this host — a testnet key on the demo host, or the reverse, answers this")

	// 10004 "Error sign, please check your signature generation algorithm."
	ErrBadSignature = errors.New("bybit: 10004, the signature was rejected")

	// 10005 "Permission denied, please check your API key permissions."
	ErrPermissionDenied = errors.New("bybit: 10005, the key lacks the permission this call needs")

	// 10006 "Too many visits. Exceeded the API Rate Limit." — the per-UID,
	// per-endpoint bucket.
	ErrRateLimited = errors.New("bybit: 10006, the per-UID rate limit was exceeded")

	// 10010 "Unmatched IP, please check your API key's bound IP addresses."
	ErrIPNotBound = errors.New("bybit: 10010, this IP is not bound to the key")

	// 10028 "The API can only be accessed by unified account users."
	ErrNotUnified = errors.New("bybit: 10028, the account is not a unified trading account")

	// 110072 "OrderLinkedID is duplicate". A SAFE error: the venue already
	// holds an order under that id, so a resend after an ambiguous timeout was
	// refused instead of doubling the position.
	ErrDuplicateClientOrderID = errors.New("bybit: 110072, that orderLinkId is already in use at the venue")

	// 110079 "The order is processing and can not be operated, please try
	// again later". AMBIGUOUS by construction: the order exists and its state
	// is not settled.
	ErrOrderProcessing = errors.New("bybit: 110079, the order is still processing at the venue")

	// ErrOrderNotVisible: the venue lists no order under the id asked for, on
	// either /v5/order/realtime or /v5/order/history. On Bybit that is NOT
	// broker.ErrOrderNotFound: create is asynchronous, history "may delay", and
	// realtime loses closed orders after a venue restart, so an order that
	// arrived — and filled — can be invisible for a while. It is AMBIGUOUS:
	// neither "nothing filled" nor "safe to resend".
	ErrOrderNotVisible = errors.New("bybit: no order under that id is visible yet — ambiguous, not a statement that it does not exist")

	// ErrCancelNotConfirmed: a cancel was acknowledged (or answered "gone")
	// but the order did not read back in a terminal state within the
	// confirmation window. The returned Order, if any, is NOT final.
	ErrCancelNotConfirmed = errors.New("bybit: the cancel is not confirmed — the order has not been read back in a final state")

	// HTTP 403 — "IP rate limit breached", a GET with an empty JSON body, or a
	// U.S. IP; for the rate limit, "terminate all HTTP sessions and wait for at
	// least 10 minutes". Treated like Binance's 418: stop, do not retry.
	ErrForbidden = errors.New("bybit: HTTP 403 — IP rate limit breached or a restricted region; stop and wait at least 10 minutes, do not retry")

	// HTTP 401 — "Need to use the correct key to access; Need to put
	// authentication params in the request header".
	ErrUnauthorized = errors.New("bybit: HTTP 401 — the key or the authentication headers were refused")
)

// VenueError carries the venue's retCode and message beside the mapped error.
type VenueError struct {
	Code   int
	MsgVI  string // the venue's retMsg, scrubbed of the key and secret by broker.Client
	mapped error
}

func (e *VenueError) Error() string {
	return fmt.Sprintf("bybit: retCode %d: %s", e.Code, e.MsgVI)
}

func (e *VenueError) Unwrap() error { return e.mapped }

// venueError maps a retCode. Codes with no entry keep no sentinel: the number
// is better than a wrong category.
func venueError(code int, msg string, kind callKind) error {
	return &VenueError{Code: code, MsgVI: printable(msg), mapped: mapCode(code, kind)}
}

func mapCode(code int, kind callKind) error {
	switch code {
	case 10002:
		return ErrTimestampOutOfWindow
	case 10003:
		return ErrKeyInvalid
	case 10004:
		return ErrBadSignature
	case 10005:
		return ErrPermissionDenied
	case 10006:
		return ErrRateLimited
	case 10010:
		return ErrIPNotBound
	case 10028:
		return ErrNotUnified
	case 110072:
		return ErrDuplicateClientOrderID
	case 110079:
		return ErrOrderProcessing
	// 110094 "Order notional value below the lower limit", 170139 / 170213 spot notional below limit.
	case 110094, 170139, 170213:
		return broker.ErrBelowMinNotional
	// 110017 "orderQty will be truncated to zero", 170140 spot qty below lower limit.
	case 110017, 170140:
		return broker.ErrBelowMinQty
	// 110001 "Order does not exist" is mapped only on a CANCEL, and even there
	// CancelOrder reads the order back before passing it on — on an asynchronous
	// venue a cancel can meet it before the create is processed. On a read it is
	// not mapped: that would bring back the "never arrived, safe to resend"
	// reading GetOrder deliberately does not give (ErrOrderNotVisible).
	//
	// 110008 "The order has been completed or cancelled." and 110010 "The
	// order has been cancelled" mean GONE only to a cancel, whose contract is
	// "cancelling an order that is already gone returns ErrOrderNotFound". To
	// any other call they are not a statement that the order never existed,
	// and mapping them there would make a resend look safe.
	case 110001, 110008, 110010:
		if kind == callCancel {
			return broker.ErrOrderNotFound
		}
	}
	if kind == callPlace {
		// On order placement, any 170xxx (spot trading) or 110xxx (linear trading)
		// code from Bybit (except 110072 duplicate and 110079 processing, handled
		// above) or 10001 (params error) is the matching engine saying NO: the order
		// was definitively rejected and will never execute. Mapping to ErrInvalidOrder
		// lets execution's definiteRejection surface the venue's reason immediately
		// rather than wasting 10 seconds polling for an order that was never created.
		if (code >= 170000 && code < 180000) || (code >= 110000 && code < 120000) || code == 10001 {
			return broker.ErrInvalidOrder
		}
	}
	return nil
}

// classifyTransport keeps a transport failure as it is — a timeout says
// nothing about the order — and names the two HTTP statuses the error page
// gives a meaning to.
func classifyTransport(err error) error {
	var httpErr *broker.HTTPError
	if errors.Is(err, broker.ErrRedirectAttempted) || !errors.As(err, &httpErr) {
		return err
	}
	switch httpErr.StatusCode {
	case http.StatusForbidden:
		return fmt.Errorf("%w: %w", ErrForbidden, err)
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: %w", ErrUnauthorized, err)
	}
	return err
}

// printable bounds and cleans venue text before it lands in an error.
func printable(s string) string {
	const maxBytes = 512
	if len(s) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	out := []rune{}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			r = '?'
		}
		out = append(out, r)
	}
	return string(out)
}
