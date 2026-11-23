package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/kuini-1/llm-gateway/assets"
	"github.com/kuini-1/llm-gateway/internal/config"
	"github.com/kuini-1/llm-gateway/internal/router"
)

// Every path and method in the OpenAPI spec must be served by the router, so
// the spec can't drift from the code. Unauthenticated calls are enough: a
// registered route answers 401 (or 200 for ops endpoints), an unregistered
// one gets the mux's plain-text 404 or 405.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(assets.OpenAPI(), &spec); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Routes: map[string]config.Route{}}
	g := &Gateway{Cfg: cfg, Router: router.New(cfg), OpenAPI: assets.OpenAPI(), Dashboard: assets.Dashboard(),
		AdminToken: "x", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := g.Handler()

	n := 0
	for path, methods := range spec.Paths {
		url := strings.NewReplacer("{id}", "some-id").Replace(path)
		for method := range methods {
			n++
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(strings.ToUpper(method), url, nil))
			if rec.Code == http.StatusNotFound && !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") ||
				rec.Code == http.StatusMethodNotAllowed {
				t.Errorf("%s %s is documented but not routed (%d)", strings.ToUpper(method), path, rec.Code)
			}
		}
	}
	if n < 12 {
		t.Errorf("only %d operations documented", n)
	}
}
