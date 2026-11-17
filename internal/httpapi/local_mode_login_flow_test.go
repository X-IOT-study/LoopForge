package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentflow/internal/auth"
	"agentflow/internal/engine"
	"agentflow/internal/repo"

	"github.com/gin-gonic/gin"
)

const (
	localModeTestHost     = "local.test"
	localModeTestPassword = "local-mode-test-password"
)

// newLocalModeAuthFlowServer wires the same deps and middleware order the real
// server uses (localModeGuard before userAccessAuth), but only the auth routes
// and one protected API route, so the login journey runs against the real
// handlers without starting the background services.
func newLocalModeAuthFlowServer(t *testing.T) (*gin.Engine, *repo.Store) {
	t.Helper()
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")

	s, err := repo.OpenTest()
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	d := &deps{
		repo:             s,
		engine:           engine.New(s, nil),
		authClient:       auth.NewClient(auth.Config{}),
		passwordAttempts: newPasswordLimiter(),
	}
	srv := &Server{deps: d}
	srv.auth = &authHandlers{deps: d}
	srv.tokens = &tokenHandlers{deps: d}

	g := gin.New()
	g.Use(cors(), localModeGuard(), srv.tokens.userAccessAuth())
	g.GET("/api/auth/setup", srv.auth.authSetup)
	g.POST("/api/auth/register", srv.auth.authRegister)
	g.POST("/api/auth/password", srv.auth.authPassword)
	g.GET("/api/auth/current", srv.auth.authCurrent)
	// A protected, non-cloud route: only the cookie decides 200 vs 401.
	g.GET("/api/runs", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	return g, s
}

func localModeJSONRequest(t *testing.T, g *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Host = localModeTestHost
	req.Header.Set("Origin", "http://"+localModeTestHost)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	return rec
}

// TestLocalModeSetupRegisterLoginFlow covers the single-account journey the
// local edition ships: first-run setup advertises no signup, registration takes
// a password only and creates the fixed "local" administrator, a second
// registration is refused as a closed signup (403, not 409), password login
// ignores the submitted username, and the cookie is what unlocks protected
// routes.
func TestLocalModeSetupRegisterLoginFlow(t *testing.T) {
	g, store := newLocalModeAuthFlowServer(t)

	// 1. First run: setup form only, no self-service signup.
	rec := localModeJSONRequest(t, g, http.MethodGet, "/api/auth/setup", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup status=%d body=%s", rec.Code, rec.Body.String())
	}
	var setup struct {
		NeedsSetup    bool `json:"needsSetup"`
		SignupEnabled bool `json:"signupEnabled"`
		LocalMode     bool `json:"localMode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &setup); err != nil {
		t.Fatalf("decode setup: %v (%s)", err, rec.Body.String())
	}
	if !setup.NeedsSetup || setup.SignupEnabled || !setup.LocalMode {
		t.Fatalf("first-run setup=%+v want {needsSetup:true signupEnabled:false localMode:true}", setup)
	}

	// 2. Registration carries only a password; the login name is server-owned.
	rec = localModeJSONRequest(t, g, http.MethodPost, "/api/auth/register", map[string]string{"password": localModeTestPassword})
	if rec.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", rec.Code, rec.Body.String())
	}
	var registered struct {
		User  auth.User `json:"user"`
		Admin bool      `json:"admin"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &registered); err != nil {
		t.Fatalf("decode register: %v (%s)", err, rec.Body.String())
	}
	if registered.User.LoginName != "local" {
		t.Fatalf("registered loginName=%q want local", registered.User.LoginName)
	}
	if !registered.Admin || !registered.User.IsPlatformAdmin {
		t.Fatalf("first local account must be an administrator: admin=%v isPlatformAdmin=%v", registered.Admin, registered.User.IsPlatformAdmin)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("register response set no session cookie")
	}
	if !store.IsPlatformAdmin("local") {
		t.Fatal("store does not report the local account as platform admin")
	}
	admin, ok := store.LocalAdmin()
	if !ok || admin.LoginName != "local" {
		t.Fatalf("LocalAdmin()=%q,%v want local,true", admin.LoginName, ok)
	}

	// 3. After setup the page must still advertise no signup.
	rec = localModeJSONRequest(t, g, http.MethodGet, "/api/auth/setup", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &setup); err != nil {
		t.Fatalf("decode second setup: %v (%s)", err, rec.Body.String())
	}
	if setup.NeedsSetup || setup.SignupEnabled || !setup.LocalMode {
		t.Fatalf("post-setup setup=%+v want {needsSetup:false signupEnabled:false localMode:true}", setup)
	}

	// 4. A second registration is a closed signup: 403, never 409 (the account
	// name is server-owned, so "login already taken" cannot be the answer).
	rec = localModeJSONRequest(t, g, http.MethodPost, "/api/auth/register", map[string]string{"password": localModeTestPassword})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("second register status=%d want 403 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "自助注册已关闭") {
		t.Fatalf("second register body=%s want the closed-signup message", rec.Body.String())
	}

	// 5. Login decides on the password alone: an unrelated login name still
	// signs in the local administrator.
	rec = localModeJSONRequest(t, g, http.MethodPost, "/api/auth/password", map[string]string{
		"login":    "someone-else",
		"password": localModeTestPassword,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("password login status=%d body=%s", rec.Code, rec.Body.String())
	}
	// A wrong password is rejected regardless of the login name.
	rec = localModeJSONRequest(t, g, http.MethodPost, "/api/auth/password", map[string]string{
		"login":    "local",
		"password": "definitely-wrong",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status=%d want 401 body=%s", rec.Code, rec.Body.String())
	}

	// 6. Cookie gates a protected route; no cookie is 401.
	req := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	req.Host = localModeTestHost
	req.AddCookie(cookies[0])
	withCookie := httptest.NewRecorder()
	g.ServeHTTP(withCookie, req)
	if withCookie.Code != http.StatusOK {
		t.Fatalf("protected route with cookie status=%d body=%s", withCookie.Code, withCookie.Body.String())
	}

	anon := httptest.NewRecorder()
	anonReq := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	anonReq.Host = localModeTestHost
	g.ServeHTTP(anon, anonReq)
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("protected route without cookie status=%d want 401 body=%s", anon.Code, anon.Body.String())
	}
}
