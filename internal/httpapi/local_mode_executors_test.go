package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/engine"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"agentflow/internal/tracker"

	"github.com/gin-gonic/gin"
)

// localModeKeptExecutorIDs and localModeHiddenExecutorIDs are the expected
// split of repo.BuiltInExecutors in local mode: the eight CLIs that run on the
// machine itself stay, the two whose local form is still incomplete go.
var (
	localModeKeptExecutorIDs   = []string{"agy", "claude", "codebuddy", "codex", "cursor", "dsh", "opencode", "pi"}
	localModeHiddenExecutorIDs = []string{"kimi", "zcode"}
)

// newLocalModeExecutorServer builds the real router (bootstrap and every
// catalog route included) on a throwaway store; the test flips the mode with
// t.Setenv before calling it.
func newLocalModeExecutorServer(t *testing.T) http.Handler {
	t.Helper()
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return New(s, engine.New(s, runtime.NewManager(filepath.Join(t.TempDir(), "workspaces"))), tracker.NewService(), "", "")
}

func localModeJSONCall(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// executorIDsFromItems decodes the executor list of a JSON payload (the
// /api/executors array or bootstrap's "executors" field) into sorted ids.
func executorIDsFromItems(t *testing.T, label string, items []any) []string {
	t.Helper()
	ids := make([]string, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s: executor entry %T is not an object", label, item)
		}
		id, _ := entry["id"].(string)
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// assertLocalModeExecutorSet checks one executor list against the split the
// mode promises.
func assertLocalModeExecutorSet(t *testing.T, label string, ids []string, local bool) {
	t.Helper()
	want := append([]string(nil), localModeKeptExecutorIDs...)
	if !local {
		want = append(want, localModeHiddenExecutorIDs...)
	}
	sort.Strings(want)
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("%s executor ids = %v, want %v", label, ids, want)
	}
}

// TestLocalModeExecutorCatalogFiltering covers all four dispatch points that
// hand the page the CLI list, in both modes. Local mode must hide kimi and
// zcode everywhere while keeping the other eight; platform mode must be byte
// for byte what the page saw before this change.
func TestLocalModeExecutorCatalogFiltering(t *testing.T) {
	modes := []struct {
		name      string
		localMode string
		local     bool
	}{
		{name: "local mode", localMode: "1", local: true},
		{name: "platform mode", localMode: "", local: false},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv("AGENTFLOW_LOCAL_MODE", mode.localMode)
			t.Setenv("AGENTFLOW_AUTH_REQUIRED", "")
			h := newLocalModeExecutorServer(t)

			// 1. GET /api/executors
			rec := localModeJSONCall(t, h, http.MethodGet, "/api/executors", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /api/executors: status %d body %s", rec.Code, rec.Body.String())
			}
			var executors []any
			if err := json.Unmarshal(rec.Body.Bytes(), &executors); err != nil {
				t.Fatalf("decode executors: %v (%s)", err, rec.Body.String())
			}
			assertLocalModeExecutorSet(t, "GET /api/executors", executorIDsFromItems(t, "GET /api/executors", executors), mode.local)

			// 2. bootstrap "executors"
			rec = localModeJSONCall(t, h, http.MethodGet, "/api/bootstrap", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /api/bootstrap: status %d body %s", rec.Code, rec.Body.String())
			}
			var boot map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &boot); err != nil {
				t.Fatalf("decode bootstrap: %v (%s)", err, rec.Body.String())
			}
			bootExecutors, ok := boot["executors"].([]any)
			if !ok {
				t.Fatalf("bootstrap has no executors array: %T", boot["executors"])
			}
			assertLocalModeExecutorSet(t, "bootstrap", executorIDsFromItems(t, "bootstrap", bootExecutors), mode.local)

			// 3. GET /api/executor-models
			rec = localModeJSONCall(t, h, http.MethodGet, "/api/executor-models", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /api/executor-models: status %d body %s", rec.Code, rec.Body.String())
			}
			var catalogs map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &catalogs); err != nil {
				t.Fatalf("decode executor models: %v (%s)", err, rec.Body.String())
			}
			for _, kind := range localModeKeptExecutorIDs {
				if _, found := catalogs[kind]; !found {
					// Catalog keys are executor ids here, which for the built-in
					// eight are the CLI kinds; Claude is the one id ("claude")
					// that differs from its kind ("claude-code").
					t.Fatalf("GET /api/executor-models: missing catalog %q (mode local=%v)", kind, mode.local)
				}
			}
			for _, kind := range localModeHiddenExecutorIDs {
				_, found := catalogs[kind]
				if found == mode.local {
					t.Fatalf("GET /api/executor-models: catalog %q present=%v, want %v", kind, found, !mode.local)
				}
			}

			// 4. Creating a role pinned to kimi is rejected in local mode only.
			agent := map[string]any{
				"id": "local-mode-kimi-role", "name": "Kimi 角色", "projectId": "platform",
				"prompt": "验证本地模式拒绝 kimi 执行器", "executorId": "kimi",
			}
			rec = localModeJSONCall(t, h, http.MethodPut, "/api/agents", agent)
			if !mode.local {
				if rec.Code != http.StatusOK {
					t.Fatalf("platform mode PUT /api/agents: status %d body %s", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("local mode PUT /api/agents: status %d body %s, want 403", rec.Code, rec.Body.String())
			}
			var rejection struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &rejection); err != nil {
				t.Fatalf("decode rejection: %v (%s)", err, rec.Body.String())
			}
			if rejection.Code != config.LocalModeErrorCode {
				t.Fatalf("rejection code = %q, want %q", rejection.Code, config.LocalModeErrorCode)
			}
			if !strings.Contains(rejection.Error, "kimi") {
				t.Fatalf("rejection error = %q, want it to name kimi", rejection.Error)
			}
		})
	}
}

// TestLocalModeExecutorSwitchMatchesCatalog keeps the config allow-list and
// the built-in catalog from drifting: every built-in kind is either allowed
// by the switch or listed by LocalModeUnsupportedExecutors, never both.
func TestLocalModeExecutorSwitchMatchesCatalog(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	unsupported := config.LocalModeUnsupportedExecutors()
	for _, executor := range repo.BuiltInExecutors() {
		allowed := config.LocalModeSupportsExecutor(executor.Kind)
		listed := false
		for _, kind := range unsupported {
			if kind == executor.Kind {
				listed = true
				break
			}
		}
		if allowed == listed {
			t.Fatalf("executor kind %q: LocalModeSupportsExecutor=%v, listed as unsupported=%v; exactly one must hold", executor.Kind, allowed, listed)
		}
	}
}

// TestLocalModeRefusesToInstallExcludedCLI covers the assistant install button:
// local mode must not download a CLI it refuses to run. Platform mode keeps the
// previous answer, which the status branch shows without starting a download.
func TestLocalModeRefusesToInstallExcludedCLI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("AGENTFLOW_RUNTIME_CLI_DIR", t.TempDir())
	t.Setenv("AGENTFLOW_MANAGED_CLI_ROOT", "")

	local := func(t *testing.T, start bool) *httptest.ResponseRecorder {
		t.Helper()
		t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/assistant/cli-install", strings.NewReader(`{"engine":"kimi"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		(&assistantHandlers{deps: &deps{}}).answerCLIInstall(c, "kimi", start)
		return rec
	}

	for _, start := range []bool{true, false} {
		rec := local(t, start)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("local mode install (start=%v): status %d body %s, want 403", start, rec.Code, rec.Body.String())
		}
		var rejection struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &rejection); err != nil {
			t.Fatalf("decode rejection: %v (%s)", err, rec.Body.String())
		}
		if rejection.Code != config.LocalModeErrorCode {
			t.Fatalf("rejection code = %q, want %q", rejection.Code, config.LocalModeErrorCode)
		}
		if !strings.Contains(rejection.Error, "kimi") {
			t.Fatalf("rejection error = %q, want it to name kimi", rejection.Error)
		}
	}

	t.Run("platform mode", func(t *testing.T) {
		t.Setenv("AGENTFLOW_LOCAL_MODE", "")
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/assistant/cli-install?engine=kimi", nil)
		(&assistantHandlers{deps: &deps{}}).answerCLIInstall(c, "kimi", false)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("platform mode install status: status 403 body %s, want the previous behaviour", rec.Body.String())
		}
		var body struct {
			Engine string `json:"engine"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode status: %v (%s)", err, rec.Body.String())
		}
		if body.Engine != "kimi" {
			t.Fatalf("status engine = %q, want kimi", body.Engine)
		}
	})
}
