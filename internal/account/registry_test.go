package account

import "testing"

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
