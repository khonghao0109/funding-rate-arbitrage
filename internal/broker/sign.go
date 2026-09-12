package broker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
)

// Binance signs the string it RECEIVES, so this file's one rule is that the
// bytes signed and the bytes sent are the same bytes.
//
// From the spot reference: "The signature payload is the query string
// concatenated without separator to the HTTP body", the HMAC-SHA256 key is the
// secretKey, and the result goes into the request as a `signature` parameter;
// the API key travels in the `X-MBX-APIKEY` header.
// https://developers.binance.com/docs/binance-spot-api-docs/rest-api/endpoint-security-type
//
// The USDⓈ-M futures reference says the same thing in its own words — "Use your
// secretKey as the key and totalParams as the value", totalParams being "the
// query string concatenated with the request body".
// https://developers.binance.com/docs/derivatives/usds-margined-futures/general-info
//
// That is why parameters are an ORDERED SLICE and not a map or url.Values:
// url.Values.Encode sorts alphabetically, and while a sorted string would still
// verify (the venue signs whatever arrived), building the string twice — once
// to sign and once to send — is how the two drift apart. Here it is built once.

// Param is one query parameter, in the order it will be sent.
type Param struct {
	Key   string
	Value string
}

// QueryString renders parameters into the exact bytes that will be sent.
//
// Values are percent-encoded because the documentation requires it for
// non-ASCII, and because a value carrying `&` or `=` would otherwise be read by
// the venue as two parameters — a request that signs correctly and means
// something else.
func QueryString(params []Param) string {
	var b strings.Builder
	for i, p := range params {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(p.Key))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(p.Value))
	}
	return b.String()
}

// Sign is HMAC-SHA256 of totalParams under the secret, lowercase hex.
//
// totalParams must be the finished string — QueryString's output, plus the body
// if there is one. Passing the parameters instead and letting this function
// render them would put the rendering in two places again.
func Sign(secret Secret, totalParams string) string {
	mac := hmac.New(sha256.New, []byte(secret.Expose()))
	mac.Write([]byte(totalParams))
	return hex.EncodeToString(mac.Sum(nil))
}

// SignedQuery renders the parameters and appends the signature, returning the
// query string to send. The signature is always LAST: it is computed over
// everything before it, so anything appended after it would not be covered.
func SignedQuery(secret Secret, params []Param) string {
	totalParams := QueryString(params)
	return totalParams + "&signature=" + Sign(secret, totalParams)
}

// redactURL is the second door on a leaked signature, and it is not optional.
//
// The signature rides on the query string. Go's http client wraps transport
// failures in *url.Error, whose Error() prints the WHOLE URL — so a DNS blip on
// a signed request would write a valid signature into the log, and a
// non-2xx branch that reports "GET <url> returned 400" does the same thing
// deliberately. Every error this package produces names scheme, host and path
// only. A signature is single-use against one timestamp, but a leaked one still
// proves the account and pairs with anything else in the same log.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		// Unparseable: say nothing rather than echo a string that may contain
		// the query.
		return "<url>"
	}
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return u.String()
}
