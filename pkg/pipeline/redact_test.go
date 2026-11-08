package pipeline

import (
	"reflect"
	"testing"
)

func TestCredentialPathsRedact(t *testing.T) {
	values := map[string]any{
		"auth": map[string]any{
			"postgresPassword": "hunter2",
			"existingSecret":   "db-credentials",
			"secretName":       "db",
			"enabled":          true,
		},
		"image":   map[string]any{"tag": "1.2"},
		"tokens":  []any{map[string]any{"apiKey": "k"}},
		"jwtRef":  "secret-ref",
		"replica": 2,
	}
	got := credentialPaths(values, "", true)
	want := []string{"auth.postgresPassword", "tokens[0].apiKey"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths %v, want %v", got, want)
	}
	auth := values["auth"].(map[string]any)
	if auth["postgresPassword"] != RedactedValue || auth["existingSecret"] != "db-credentials" {
		t.Errorf("redacted: %v", auth)
	}
	if again := credentialPaths(values, "", false); len(again) != 0 {
		t.Errorf("redacted values still reported: %v", again)
	}
}
