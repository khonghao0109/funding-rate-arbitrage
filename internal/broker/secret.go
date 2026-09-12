package broker

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Secret is a credential value that cannot be printed.
//
// Every leak of an API secret this project could plausibly suffer goes through
// one of four doors, and each is closed here:
//
//   - fmt's verbs — %v, %s, %+v, %#v, %q — all route through Format, which
//     ignores the verb and writes Redacted.
//   - log.Printf, which is fmt.
//   - encoding/json, whose MarshalJSON REFUSES rather than emitting a
//     placeholder: a config dump that silently turned a secret into
//     "[redacted]" would look like it round-tripped, and the next reader would
//     restore an account whose secret is the literal string "[redacted]".
//   - errors, which are fmt again — but see redactURL: the signature rides on
//     the QUERY STRING, so a wrapped *url.Error printing the whole URL is a
//     second door that this type alone cannot close.
//
// The hole Go leaves open, stated rather than papered over: fmt can only call
// Format on a field it can take an interface from, so a Secret stored in an
// UNEXPORTED field of some other struct is printed by reflection and leaks.
// Keep Secret in exported fields, which is why Credentials' are.
type Secret struct {
	// v is unexported so no caller outside this package can read it without
	// going through Expose, and so a reflective printer has to give up.
	v string
}

// Redacted is what every rendering of a Secret produces. It is deliberately not
// a plausible secret: it has no length, no prefix and no suffix of the real
// value, because a "first four characters" convention is how a key ends up
// reconstructable from a handful of log lines.
const Redacted = "[redacted]"

// NewSecret wraps a value. Trimmed, because a secret pasted into .env with a
// trailing newline signs a different string than the one the venue stored and
// fails with an authentication error that names nothing.
func NewSecret(v string) Secret { return Secret{v: strings.TrimSpace(v)} }

// Format is the one rendering. It takes precedence over String and GoString for
// every fmt verb, so there is no verb that prints the value.
func (s Secret) Format(f fmt.State, verb rune) { _, _ = f.Write([]byte(Redacted)) }

// String and GoString exist so that a caller reaching for them directly — and
// any interface satisfied by them — gets the same answer as fmt.
func (s Secret) String() string   { return Redacted }
func (s Secret) GoString() string { return Redacted }

// MarshalJSON refuses. See the type comment: emitting a placeholder would make
// a round trip look successful.
func (s Secret) MarshalJSON() ([]byte, error) {
	return nil, errors.New("broker: refusing to serialise a Secret to JSON — a credential must not reach a file, a wire or a log")
}

// Expose returns the value. It is named to be conspicuous at the call site and
// in review: every use of it is a place where a secret enters a string, and
// there should be exactly one (Sign) plus the API-key header.
func (s Secret) Expose() string { return s.v }

// Empty reports whether nothing was loaded. It is the only question about a
// secret that can be answered without exposing it.
func (s Secret) Empty() bool { return s.v == "" }

// Credentials is one venue account's key pair and where it came from.
//
// The API KEY is a Secret too, not only the API secret. It cannot sign anything
// on its own, but it identifies an account to anyone who reads it, and a key
// published beside a leaked secret is the pair — there is no reason to let the
// weaker half be printable.
type Credentials struct {
	APIKey    Secret
	APISecret Secret

	// SourceVI names where the pair was read from, for the launch log. It is
	// the variable NAMES, never a value.
	SourceVI string
}

// ErrNoCredentials means the environment did not carry the pair. It is a
// distinct error because "no key configured" and "key rejected by the venue"
// are different operational facts and only one of them is a mistake.
var ErrNoCredentials = errors.New("broker: no credentials in the environment")

// CredentialsFromEnv reads a key pair from two environment variables.
//
// It reports the variable NAMES in every error and never their contents — an
// error saying "the secret 'abc123' is empty" is the leak this whole file
// exists to prevent, and it is the shape such a bug usually takes.
func CredentialsFromEnv(keyVar, secretVar string) (Credentials, error) {
	key, secret := NewSecret(os.Getenv(keyVar)), NewSecret(os.Getenv(secretVar))
	switch {
	case key.Empty() && secret.Empty():
		return Credentials{}, fmt.Errorf("%w: %s and %s are unset", ErrNoCredentials, keyVar, secretVar)
	case key.Empty():
		return Credentials{}, fmt.Errorf("%w: %s is set but %s is not — a pair is needed", ErrNoCredentials, secretVar, keyVar)
	case secret.Empty():
		return Credentials{}, fmt.Errorf("%w: %s is set but %s is not — a pair is needed", ErrNoCredentials, keyVar, secretVar)
	}
	return Credentials{APIKey: key, APISecret: secret, SourceVI: "biến môi trường " + keyVar + " / " + secretVar}, nil
}
