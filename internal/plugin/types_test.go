package plugin

import (
	"errors"
	"strings"
	"testing"
)

func TestRedactHidesCredentialShapedText(t *testing.T) {
	cases := map[string]string{
		"key is sk-abcdefghijklmnop":                    "sk-abcdefghijklmnop",
		"Authorization: Bearer abcdefghijklmnop":        "abcdefghijklmnop",
		`{"apiKey": "supersecretvalue"}`:                "supersecretvalue",
		"OPENAI_API_KEY=supersecretvalue":               "supersecretvalue",
		"token=abcdefghijkl":                            "abcdefghijkl",
		"client_secret: hunter2hunter2":                 "hunter2hunter2",
		"request failed with status 500 for /v1/models": "",
	}
	for input, secret := range cases {
		out := Redact(input)
		if secret != "" && strings.Contains(out, secret) {
			t.Errorf("Redact(%q) = %q, still contains %q", input, out, secret)
		}
		if secret == "" && out != input {
			t.Errorf("Redact(%q) = %q, should be unchanged", input, out)
		}
	}
}

func TestPluginErrorCarriesIdentityAndStage(t *testing.T) {
	cause := errors.New("boom")
	err := NewError("demo", "/tmp/demo/plugin.json", "run", StageInvoke, CategoryException, cause)
	message := err.Error()
	for _, want := range []string{"demo", "/tmp/demo/plugin.json", "invoke", "exception", "run", "boom"} {
		if !strings.Contains(message, want) {
			t.Errorf("Error() = %q, missing %q", message, want)
		}
	}
	if !errors.Is(err, cause) {
		t.Error("PluginError must unwrap to its cause")
	}
	if category, ok := CategoryOf(err); !ok || category != CategoryException {
		t.Errorf("CategoryOf = %q, %v", category, ok)
	}
	if !IsPluginError(err) {
		t.Error("IsPluginError must find a wrapped PluginError")
	}
}

func TestPluginErrorRedactsCause(t *testing.T) {
	err := NewError("demo", "", "", StageInvoke, CategoryException, errors.New("apiKey=supersecretvalue rejected"))
	if strings.Contains(err.Error(), "supersecretvalue") {
		t.Errorf("PluginError leaked a secret: %q", err.Error())
	}
}

func TestManifestErrorFormat(t *testing.T) {
	err := &ManifestError{Plugin: "demo", Path: "/p/plugin.json", Field: "tools[0].handler", Message: "must not be empty"}
	want := `plugin "demo" (/p/plugin.json): field tools[0].handler: must not be empty`
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	unknown := &ManifestError{Path: "/p/plugin.json", Message: "malformed JSON"}
	if !strings.Contains(unknown.Error(), "<unknown>") {
		t.Errorf("Error() = %q, want <unknown>", unknown.Error())
	}
}

func TestBuiltinNameReservations(t *testing.T) {
	if !IsBuiltinToolName("execute_command") || IsBuiltinToolName("my_tool") {
		t.Error("built-in tool reservation is wrong")
	}
	if !IsBuiltinCommandName("sandbox") || IsBuiltinCommandName("review") {
		t.Error("built-in command reservation is wrong")
	}
	// sortedContains relies on sorted input.
	for _, list := range [][]string{BuiltinToolNames(), BuiltinCommandNames()} {
		for i := 1; i < len(list); i++ {
			if list[i-1] >= list[i] {
				t.Fatalf("reserved name list is not sorted: %q then %q", list[i-1], list[i])
			}
		}
	}
}

func TestParsePermission(t *testing.T) {
	if permission, err := ParsePermission("fs.write"); err != nil || permission != PermissionFilesystemWrite {
		t.Errorf("ParsePermission = %q, %v", permission, err)
	}
	if _, err := ParsePermission("root"); err == nil {
		t.Error("unknown permission must be rejected")
	}
}
