package account

import (
	"testing"

	"github.com/thehappieco/mailie/internal/secrets"
)

func TestSubmissionsGreetWithThePublicHostNeverAnAddress(t *testing.T) {
	for publicURL, want := range map[string]string{
		"https://console.mailie.example": "console.mailie.example",
		"http://localhost:5174":          "localhost",
		"http://127.0.0.1:8080":          "",
		"":                               "",
	} {
		r := &Registry{publicURL: publicURL}
		if got := r.heloName(); got != want {
			t.Errorf("public URL %q greets as %q, want %q", publicURL, got, want)
		}
	}
}

func TestAnAccountIDIsARefEveryCredentialCanBeSealedFor(t *testing.T) {
	// A credential is sealed for its account id, which a key service's
	// encryption context carries: an id it would refuse could be linked and
	// never sealed in production.
	for range 256 {
		id := newAccountID()
		for _, field := range []string{"password", "oauth_token"} {
			if err := secrets.Credential(id, field).Validate(); err != nil {
				t.Fatalf("the %s of %s cannot be sealed: %v", field, id, err)
			}
		}
	}
}
