package api_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/api"
)

// schemaRoute is a route as a migration's comment names one: a method, then
// a path under /v1/; pathParam is one of its path's parameters.
var (
	schemaRoute = regexp.MustCompile(`\b(GET|POST|PUT|PATCH|DELETE) (/v1/[A-Za-z0-9_{}/.-]*[A-Za-z0-9_}])`)
	pathParam   = regexp.MustCompile(`\{[^}]*\}`)
)

// A migration's comments are frozen once it is released, and the route one
// names as what writes a column is where an audit of that column starts: it
// must be a route this API serves.
func TestTheSchemaNamesOnlyRoutesTheAPIServes(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "store", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migration read: %v", err)
	}
	mux := http.NewServeMux()
	(&api.Handler{}).Mount(mux)
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range schemaRoute.FindAllStringSubmatch(string(raw), -1) {
			path := pathParam.ReplaceAllString(m[2], "x")
			if _, pattern := mux.Handler(httptest.NewRequest(m[1], path, nil)); pattern == "" ||
				!strings.HasPrefix(pattern, m[1]+" ") {
				t.Errorf("%s names %s %s, which this API does not serve", filepath.Base(file), m[1], m[2])
			}
		}
	}
}
