package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// sentinel is a value no other string in the process can contain by accident,
// so "it does not appear" is a real answer rather than a coincidence. It is not
// a credential and is not valid anywhere.
const sentinel = "SENTINEL-c2f48a1d-NEVER-PRINT-THIS-SECRET"

// Every fmt verb, one table. The failure this guards is not exotic: it is
// log.Printf("%+v", cfg) written in a hurry while debugging an auth error,
// which is exactly when a secret is most likely to be printed.
func TestSecret_NoFmtVerbPrintsTheValue(t *testing.T) {
	s := NewSecret(sentinel)
	type holder struct {
		Name   string
		Key    Secret
		Nested struct{ Deeper Secret }
	}
	h := holder{Name: "binance_testnet", Key: s}
	h.Nested.Deeper = s

	renderings := map[string]string{
		"%v":                    fmt.Sprintf("%v", s),
		"%s":                    fmt.Sprintf("%s", s),
		"%q":                    fmt.Sprintf("%q", s),
		"%+v":                   fmt.Sprintf("%+v", s),
		"%#v":                   fmt.Sprintf("%#v", s),
		"%x":                    fmt.Sprintf("%x", s),
		"%d":                    fmt.Sprintf("%d", s),
		"pointer %v":            fmt.Sprintf("%v", &s),
		"Sprint":                fmt.Sprint(s),
		"Sprintln":              fmt.Sprintln(s),
		"String()":              s.String(),
		"GoString()":            s.GoString(),
		"in a struct %v":        fmt.Sprintf("%v", h),
		"in a struct %+v":       fmt.Sprintf("%+v", h),
		"in a nested struct %v": fmt.Sprintf("%v", h.Nested),
		"in a slice %v":         fmt.Sprintf("%v", []Secret{s}),
		"in a map %v":           fmt.Sprintf("%v", map[string]Secret{"k": s}),
		"error wrapping":        fmt.Errorf("auth failed for %v", s).Error(),
	}
	for name, got := range renderings {
		if strings.Contains(got, sentinel) {
			t.Errorf("%s printed the secret: %q", name, got)
		}
		if !strings.Contains(got, Redacted) {
			t.Errorf("%s = %q, want it to contain %q", name, got, Redacted)
		}
	}
}

// json.Marshal REFUSES rather than substituting a placeholder: a config dump
// that silently wrote "[redacted]" would look like it round-tripped, and the
// next reader would restore an account whose secret is that literal string.
func TestSecret_MarshalJSONRefusesInsteadOfSubstituting(t *testing.T) {
	s := NewSecret(sentinel)
	if _, err := json.Marshal(s); err == nil {
		t.Fatal("json.Marshal(Secret) succeeded; it must refuse")
	}
	type creds struct {
		APIKey Secret `json:"api_key"`
	}
	raw, err := json.Marshal(creds{APIKey: s})
	if err == nil {
		t.Fatalf("marshalling a struct CONTAINING a Secret succeeded and produced %s", scrubValue(string(raw), sentinel))
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("the marshal error itself carries the secret: %q", err)
	}
}

func TestCredentialsFromEnv_NamesTheVariablesAndNeverTheValues(t *testing.T) {
	const keyVar, secretVar = "BROKER_TEST_KEY", "BROKER_TEST_SECRET"

	if _, err := CredentialsFromEnv(keyVar, secretVar); err == nil {
		t.Fatal("both unset must be an error")
	} else if !strings.Contains(err.Error(), keyVar) || !strings.Contains(err.Error(), secretVar) {
		t.Errorf("the error must name both variables so the operator knows what to set: %q", err)
	}

	// Half a pair is a configuration mistake, not a usable credential — and the
	// error must still name variables rather than echo the value that IS set.
	t.Setenv(keyVar, "a-key")
	t.Setenv(secretVar, "")
	_, err := CredentialsFromEnv(keyVar, secretVar)
	if err == nil {
		t.Fatal("a key without a secret must be an error")
	}
	if strings.Contains(err.Error(), "a-key") {
		t.Errorf("the error echoed the value that was set: %q", err)
	}

	t.Setenv(secretVar, "  "+sentinel+"\n")
	creds, err := CredentialsFromEnv(keyVar, secretVar)
	if err != nil {
		t.Fatalf("a complete pair must load: %v", err)
	}
	// Trimmed: a secret pasted with a trailing newline signs a different string
	// than the venue stored, and fails with an error that names nothing.
	if creds.APISecret.Expose() != sentinel {
		t.Error("the secret was not trimmed of surrounding whitespace")
	}
	if strings.Contains(fmt.Sprintf("%+v", creds), sentinel) {
		t.Error("printing Credentials printed the secret")
	}
	if !strings.Contains(creds.SourceVI, keyVar) {
		t.Errorf("SourceVI must name where the pair came from, got %q", creds.SourceVI)
	}
}

// scrubValue is the test's own helper for the one place a test must print
// something that might contain the sentinel.
func scrubValue(s, secret string) string { return strings.ReplaceAll(s, secret, Redacted) }

// Splitting the credential by venue (2026-09-13). The measurement that forced
// it: one key pair does not serve two venues — Binance's futures testnet and
// spot testnet are SEPARATE systems with separate registrations, and a futures
// key presented to testnet.binance.vision is refused with -2015 ("Invalid
// API-key, IP, or permissions for action"). So each venue reads its own pair,
// and the original names stay readable as the futures fallback so an existing
// .env keeps working.
func TestCredentialsFromEnvAny_PrefersTheVenuePairAndFallsBackToTheLegacyOne(t *testing.T) {
	const (
		venueKey, venueSecret = "BROKER_TEST_FUT_KEY", "BROKER_TEST_FUT_SECRET"
		oldKey, oldSecret     = "BROKER_TEST_OLD_KEY", "BROKER_TEST_OLD_SECRET"
	)
	candidates := []EnvPair{{venueKey, venueSecret}, {oldKey, oldSecret}}

	// (a) Nothing set at all: ErrNoCredentials, and the error names EVERY
	//     variable that would have worked, because the operator's next action
	//     is to set one of them.
	_, err := CredentialsFromEnvAny(candidates...)
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("nothing set: err = %v, want ErrNoCredentials", err)
	}
	for _, name := range []string{venueKey, venueSecret, oldKey, oldSecret} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name %s, so it does not say what to set: %q", name, err)
		}
	}

	// (b) Only the legacy pair: it loads, and SourceVI says WHICH pair was
	//     used. That matters operationally — with two accounts in play, "which
	//     key signed this" is the first question when the venue answers -2015.
	t.Setenv(oldKey, "legacy-key")
	t.Setenv(oldSecret, sentinel)
	creds, err := CredentialsFromEnvAny(candidates...)
	if err != nil {
		t.Fatalf("the legacy pair must still load: %v", err)
	}
	if creds.APIKey.Expose() != "legacy-key" {
		t.Error("the legacy pair was not the one loaded")
	}
	if !strings.Contains(creds.SourceVI, oldKey) {
		t.Errorf("SourceVI must name the pair actually used, got %q", creds.SourceVI)
	}

	// (c) Both set: the venue-specific pair wins, so adding the new names to a
	//     .env that already has the old ones changes which key is used.
	t.Setenv(venueKey, "venue-key")
	t.Setenv(venueSecret, sentinel)
	creds, err = CredentialsFromEnvAny(candidates...)
	if err != nil {
		t.Fatalf("the venue pair must load: %v", err)
	}
	if creds.APIKey.Expose() != "venue-key" {
		t.Error("the legacy pair won over the venue-specific one; the specific name must take precedence")
	}

	// (d) The one that matters. A HALF-configured venue pair must be a loud
	//     error, NOT a silent fall-through to the legacy pair: falling back
	//     would sign with a different account than the operator believes, and
	//     they would debug the venue instead of the typo.
	t.Setenv(venueSecret, "")
	_, err = CredentialsFromEnvAny(candidates...)
	if err == nil {
		t.Fatal("a half-set venue pair fell back to the legacy pair silently — the operator would be signing with the wrong account")
	}
	if !errors.Is(err, ErrIncompleteCredentials) {
		t.Errorf("a half pair must be ErrIncompleteCredentials, got %v", err)
	}
	// And specifically NOT ErrNoCredentials: brokercheck SKIPS a venue with no
	// key, so a typo reported as "no key" would be silently skipped and the
	// venue reported as untested while the operator believes it was configured.
	if errors.Is(err, ErrNoCredentials) {
		t.Error("a half pair reads as ErrNoCredentials — a caller that skips unconfigured venues would silently skip this typo")
	}
	if !strings.Contains(err.Error(), venueSecret) {
		t.Errorf("the error must name the missing half, got %q", err)
	}
	if strings.Contains(err.Error(), "venue-key") {
		t.Errorf("the error echoed the value that WAS set: %q", err)
	}

	// (e) No rendering of the result carries a value, on either pair.
	t.Setenv(venueSecret, sentinel)
	creds, err = CredentialsFromEnvAny(candidates...)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{
		"%v":  fmt.Sprintf("%v", creds),
		"%+v": fmt.Sprintf("%+v", creds),
		"%#v": fmt.Sprintf("%#v", creds),
	} {
		if strings.Contains(got, sentinel) {
			t.Errorf("%s printed the secret: %q", name, scrubValue(got, sentinel))
		}
	}
}

// An empty candidate list is a programming mistake and must not read as "no
// credentials configured" — that would report a missing .env to an operator
// whose .env is fine.
func TestCredentialsFromEnvAny_RefusesAnEmptyCandidateList(t *testing.T) {
	_, err := CredentialsFromEnvAny()
	if err == nil {
		t.Fatal("no candidates must be an error")
	}
	if errors.Is(err, ErrNoCredentials) {
		t.Error("an empty candidate list must not masquerade as ErrNoCredentials — it is a caller bug, not a configuration one")
	}
}
