package broker

import (
	"encoding/json"
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
