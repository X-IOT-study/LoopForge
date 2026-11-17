package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"agentflow/internal/config"

	"github.com/gin-gonic/gin"
)

// localModeDesignPrefixes is a hand copy of the "rejected in local mode" rows in
// tech-design §5 (artifacts/local-only-edition/02-design/tech-design.md, the
// table at §5 "平台能力" / rejection list). It is duplicated on purpose: adding
// or removing a cloud prefix in local_mode_gate.go must fail this test until
// the design and the gate agree again, which is the regression net the review
// asked for (no silent list drift).
var localModeDesignPrefixes = []string{
	"/api/notification-bots",
	"/api/notification-preview",
	"/api/notification-deliveries",
	"/api/bot-bindings",
	"/api/platform/",
	"/api/auth/gongfeng",
	"/api/auth/exchange",
	"/api/auth/tokens",
	"/api/mcp-market",
	"/api/skillhub/",
	"/api/knowledge/submissions",
	"/api/sync/",
	"/api/shared-schedule-tasks",
	"/runners/knowledge/",
}

func TestLocalModeGatePrefixesMatchDesign(t *testing.T) {
	gate := append([]string(nil), localModeBlockedPrefixes...)
	design := append([]string(nil), localModeDesignPrefixes...)
	sort.Strings(gate)
	sort.Strings(design)
	if strings.Join(gate, "\n") != strings.Join(design, "\n") {
		t.Fatalf("localModeBlockedPrefixes drifted from tech-design §5:\ngate:\n%s\ndesign:\n%s",
			strings.Join(gate, "\n"), strings.Join(design, "\n"))
	}
}

// TestLocalModeGateMethodSpecialCases covers the three §5 rows whose siblings
// remain local features: only listing accounts, posting a runner bind code and
// publishing a schedule job reach the platform.
func TestLocalModeGateMethodSpecialCases(t *testing.T) {
	cases := []struct {
		name      string
		method    string
		path      string
		wantBlock bool
	}{
		{"list users blocked", http.MethodGet, "/api/users", true},
		{"user credentials stay open", http.MethodPut, "/api/users/u1/credentials", false},
		{"create bind code blocked", http.MethodPost, "/api/runners/bind-codes", true},
		{"read bind codes stays open", http.MethodGet, "/api/runners/bind-codes", false},
		{"publish schedule job blocked", http.MethodPost, "/api/schedule-jobs/j1/publish", true},
		{"edit schedule job stays open", http.MethodPut, "/api/schedule-jobs/j1", false},
		{"runs stay open", http.MethodGet, "/api/runs", false},
		{"previews stay open", http.MethodGet, "/api/runs/r1/preview", false},
		{"sync prefix blocked", http.MethodGet, "/api/sync/status", true},
		{"runner knowledge queue blocked", http.MethodGet, "/runners/knowledge/tasks", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := localModeBlockedPath(tc.method, tc.path); got != tc.wantBlock {
				t.Fatalf("localModeBlockedPath(%s %s)=%v want %v", tc.method, tc.path, got, tc.wantBlock)
			}
		})
	}
}

// TestLocalModeGateResponseShape asserts the rejection the page switches a
// feature off on: 403, the stable code, and CORS headers so a same-origin page
// can read the body instead of seeing an opaque failure.
func TestLocalModeGateResponseShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	const origin = "http://local.test"
	router := gin.New()
	// Mirrors server.go: cors() runs before localModeGuard().
	router.Use(cors(), localModeGuard())
	router.Any("/*any", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/api/mcp-market", nil)
	req.Host = "local.test"
	req.Header.Set("Origin", origin)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403 body=%s", rec.Code, rec.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode rejection body: %v (%s)", err, rec.Body.String())
	}
	if payload["code"] != config.LocalModeErrorCode {
		t.Fatalf("code=%q want %q", payload["code"], config.LocalModeErrorCode)
	}
	if payload["error"] == "" {
		t.Fatal("rejection body has no error text")
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
		t.Fatalf("Access-Control-Allow-Origin=%q want %q", got, origin)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Fatal("rejection response is missing CORS method header")
	}

	// Platform mode passes the same request through untouched.
	t.Setenv("AGENTFLOW_LOCAL_MODE", "0")
	pass := httptest.NewRequest(http.MethodGet, "/api/mcp-market", nil)
	pass.Host = "local.test"
	passRec := httptest.NewRecorder()
	router.ServeHTTP(passRec, pass)
	if passRec.Code != http.StatusOK {
		t.Fatalf("platform mode status=%d want 200 body=%s", passRec.Code, passRec.Body.String())
	}
}
