package redact

import (
	"strings"
	"testing"
)

func TestTextHidesCredentialShapedSubstrings(t *testing.T) {
	cases := map[string]string{
		"key is sk-abcdefghijklmnop":             "sk-abcdefghijklmnop",
		"Authorization: Bearer abcdefghijklmnop": "abcdefghijklmnop",
		`{"apiKey": "supersecretvalue"}`:         "supersecretvalue",
		"OPENAI_API_KEY=supersecretvalue":        "supersecretvalue",
		"token=abcdefghijkl":                     "abcdefghijkl",
		"client_secret: hunter2hunter2":          "hunter2hunter2",
	}
	for input, secret := range cases {
		out := Text(input)
		if strings.Contains(out, secret) {
			t.Errorf("Text(%q) = %q, still contains %q", input, out, secret)
		}
	}
}

func TestTextKeepsOrdinaryTextAndNamesTheField(t *testing.T) {
	const plain = "request failed with status 500 for /v1/models"
	if got := Text(plain); got != plain {
		t.Errorf("Text(%q) = %q, want it unchanged", plain, got)
	}
	// The key name survives so the diagnostic still says what was hidden.
	got := Text(`{"error":{"message":"invalid api key: sk-live-abcdefghijklmnop"}}`)
	if !strings.Contains(got, "invalid api key") {
		t.Errorf("Text dropped the field name: %q", got)
	}
	if strings.Contains(got, "sk-live-abcdefghijklmnop") {
		t.Errorf("Text leaked the key: %q", got)
	}
}

func TestTextHandlesEmptyInput(t *testing.T) {
	if got := Text(""); got != "" {
		t.Errorf("Text(\"\") = %q, want empty", got)
	}
}
