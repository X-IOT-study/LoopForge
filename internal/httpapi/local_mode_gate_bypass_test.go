package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"agentflow/internal/config"

	"github.com/gin-gonic/gin"
)

// localModeGateProbeHandlers are the representative cloud handlers a bypass
// attempt would want to reach. Each records how many times it ran so a variant
// that slips past the gate is visible even when it is not a 2xx.
var localModeGateProbeNames = []string{"mcp-market", "mcp-install", "users", "bind-codes", "open"}

// newLocalModeGateProbeRouter mirrors the routing settings server.go uses
// (gin.New, RedirectTrailingSlash=false, CORS and the local-mode gate ahead of
// every handler) and registers the real cloud paths from server.go plus one
// local-only control path. NoRoute/NoMethod stand in for the studio fallback:
// an unmatched path must never reach one of the cloud handlers.
func newLocalModeGateProbeRouter(t *testing.T) (*gin.Engine, map[string]*int) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	hits := make(map[string]*int, len(localModeGateProbeNames)+2)
	for _, name := range localModeGateProbeNames {
		hits[name] = new(int)
	}
	hits["no-route"] = new(int)
	hits["no-method"] = new(int)

	router := gin.New()
	router.HandleMethodNotAllowed = true
	router.RedirectTrailingSlash = false
	// server.go: cors() then localModeGuard() before the token middleware.
	router.Use(cors(), localModeGuard())

	mark := func(name string) gin.HandlerFunc {
		return func(c *gin.Context) {
			*hits[name]++
			c.JSON(http.StatusOK, gin.H{"handler": name})
		}
	}
	router.GET("/api/mcp-market", mark("mcp-market"))
	router.POST("/api/mcp-market/install", mark("mcp-install"))
	router.GET("/api/users", mark("users"))
	router.POST("/api/runners/bind-codes", mark("bind-codes"))
	router.GET("/api/open", mark("open"))
	router.NoRoute(func(c *gin.Context) {
		*hits["no-route"]++
		c.JSON(http.StatusNotFound, gin.H{"handler": "no-route"})
	})
	router.NoMethod(func(c *gin.Context) {
		*hits["no-method"]++
		c.JSON(http.StatusMethodNotAllowed, gin.H{"handler": "no-method"})
	})
	return router, hits
}

func resetLocalModeGateHits(hits map[string]*int) {
	for _, n := range hits {
		*n = 0
	}
}

// TestLocalModeGatePathVariants is the empirical half of audit AUD-04: the
// gate matches on URL.Path with strings.HasPrefix, so the question is whether
// a differently spelled path reaches a cloud handler without matching the
// prefix. Every variant below must fail (non-2xx) and must not run a cloud
// handler. The actual status is logged for each variant.
func TestLocalModeGatePathVariants(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	router, hits := newLocalModeGateProbeRouter(t)

	cases := []struct {
		name     string
		method   string
		target   string
		blocked  bool
		wantCode string
	}{
		{"PLAIN blocked prefix", http.MethodGet, "/api/mcp-market/x", true, config.LocalModeErrorCode},
		{"DOUBLE leading slash", http.MethodGet, "//api/mcp-market/x", false, ""},
		{"DOT dot-segment", http.MethodGet, "/api/mcp-market/../x", true, config.LocalModeErrorCode},
		{"ENCODED slash", http.MethodGet, "/api%2fmcp-market/x", true, config.LocalModeErrorCode},
		{"TRAILING slash", http.MethodGet, "/api/mcp-market/", true, config.LocalModeErrorCode},
		{"UPPER case prefix", http.MethodGet, "/API/mcp-market/x", false, ""},
		{"DOUBLE inner slash", http.MethodGet, "/api/mcp-market//x", true, config.LocalModeErrorCode},
		{"NUL suffix", http.MethodGet, "/api/mcp-market/x%00", true, config.LocalModeErrorCode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetLocalModeGateHits(hits)
			req := httptest.NewRequest(tc.method, tc.target, nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			t.Logf("%s %q -> status=%d", tc.method, tc.target, rec.Code)
			if rec.Code >= 200 && rec.Code < 300 {
				t.Fatalf("%s %q returned 2xx (%d); body=%s", tc.method, tc.target, rec.Code, rec.Body.String())
			}
			for _, name := range localModeGateProbeNames {
				if *hits[name] != 0 {
					t.Fatalf("%s %q reached the %s handler (status=%d)", tc.method, tc.target, name, rec.Code)
				}
			}
			if tc.blocked {
				if rec.Code != http.StatusForbidden {
					t.Fatalf("%s %q status=%d want 403", tc.method, tc.target, rec.Code)
				}
				var payload map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
					t.Fatalf("decode rejection body: %v (%s)", err, rec.Body.String())
				}
				if payload["code"] != tc.wantCode {
					t.Fatalf("%s %q code=%q want %q", tc.method, tc.target, payload["code"], tc.wantCode)
				}
			} else {
				// Not a gate match: it must fall through to NoRoute, never to a cloud handler.
				if *hits["no-route"] == 0 {
					t.Fatalf("%s %q returned %d without running the fallback; a handler may have matched", tc.method, tc.target, rec.Code)
				}
			}
		})
	}
}

// TestLocalModeGateProbeRouterControl proves the probe handlers are reachable,
// so the "handler did not run" assertions above are not vacuous. It also proves
// a local-only path still passes the gate in local mode.
func TestLocalModeGateProbeRouterControl(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "0")
	router, hits := newLocalModeGateProbeRouter(t)
	resetLocalModeGateHits(hits)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mcp-market", nil))
	if rec.Code != http.StatusOK || *hits["mcp-market"] != 1 {
		t.Fatalf("platform mode mcp-market status=%d hits=%d want 200/1", rec.Code, *hits["mcp-market"])
	}

	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	resetLocalModeGateHits(hits)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/open", nil))
	if rec.Code != http.StatusOK || *hits["open"] != 1 {
		t.Fatalf("local mode open path status=%d hits=%d want 200/1", rec.Code, *hits["open"])
	}
}
