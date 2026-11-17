package runtime

import (
	"strings"
	"testing"
)

// TestControlledBaseEnvironmentStripsControlPlaneSecrets proves the inherited
// child environment never carries the platform's own secrets, in either mode.
// The bootstrap in cmd/agentflow/main.go exports the session signing key into
// the process environment, and every workspace script inherits os.Environ().
func TestControlledBaseEnvironmentStripsControlPlaneSecrets(t *testing.T) {
	secrets := []string{
		"AGENTFLOW_SESSION_SECRET=signing-key",
		"AGENTFLOW_MYSQL_DSN=loopforge:pass@tcp(127.0.0.1:3306)/loopforge",
		"AGENTFLOW_WORKER_CONTROL_TOKEN=control",
		"AGENTFLOW_CONTROL_TOKEN=control",
		"AGENTFLOW_PLATFORM_MCP_TOKEN=mcp",
		"AGENTFLOW_RUNNER_BOOTSTRAP_TOKEN=bootstrap",
		"AGENTFLOW_MCP_CALL_TOKEN=call",
		"AGENTFLOW_API_TOKEN=api",
		"LOOPFORGE_DB_PASSWORD=db",
		"LOOPFORGE_DB_ROOT_PASSWORD=db-root",
		"LOOPFORGE_MYSQL_DSN=loopforge:pass@tcp(127.0.0.1:3306)/loopforge",
		"AGENTFLOW_IDENTITY_HEADER_SECRET=gateway-signing",
		"AGENTFLOW_GONGFENG_OAUTH_CLIENT_SECRET=oauth",
		"AGENTFLOW_MCP_TOKEN=parent-mcp",
		"AGENTFLOW_INSIGHT_TOKEN=insight",
		"AGENTFLOW_GIT_HTTP_PASSWORD=git-pass",
		"AGENTFLOW_DATABASE_URL=postgres://user:pass@127.0.0.1:5432/db",
		"AGENTFLOW_API_KEY=parent-api",
		"SESSION_SECRET=legacy-signing",
		"AGENTFLOW_NOTIFICATION_URL__BOT=https://hook.example.com/secret",
	}
	for _, mode := range []struct {
		name  string
		local string
	}{
		{"platform", ""},
		{"local", "1"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv("AGENTFLOW_LOCAL_MODE", mode.local)
			base := append([]string{"PATH=/usr/bin"}, secrets...)
			filtered := controlledBaseEnvironment(base)
			for _, entry := range secrets {
				name, _, _ := strings.Cut(entry, "=")
				for _, kept := range filtered {
					if strings.HasPrefix(kept, name+"=") {
						t.Fatalf("%s mode leaks %s to child processes: %v", mode.name, name, filtered)
					}
				}
			}
		})
	}
}

// TestControlledBaseEnvironmentKeepsProviderCredentials checks the intentional
// asymmetry: local mode keeps the host's CLI logins (they are the credential
// source), platform mode still strips them.
func TestControlledBaseEnvironmentKeepsProviderCredentials(t *testing.T) {
	base := []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=sk-host", "OPENAI_API_KEY=sk-host", "CODEBUDDY_API_KEY=host"}

	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	local := controlledBaseEnvironment(base)
	for _, name := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CODEBUDDY_API_KEY"} {
		if !environmentHas(local, name) {
			t.Fatalf("local mode must keep the host login %s: %v", name, local)
		}
	}

	t.Setenv("AGENTFLOW_LOCAL_MODE", "")
	platform := controlledBaseEnvironment(base)
	for _, name := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CODEBUDDY_API_KEY"} {
		if environmentHas(platform, name) {
			t.Fatalf("platform mode must strip %s: %v", name, platform)
		}
	}
}

// TestControlledBaseEnvironmentKeepsOrdinaryVariables guards against a blanket
// "AGENTFLOW_" prefix: task-scoped values are harmless and some tools read them.
func TestControlledBaseEnvironmentKeepsOrdinaryVariables(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "")
	// AGENTFLOW_CLI_API_KEY is the key the CLI bindings inject for the child, so
	// it must never be swept up by a blanket rule.
	base := []string{"PATH=/usr/bin", "AGENTFLOW_RUN_ID=run-1", "AGENTFLOW_NODE_ID=node-1", "AGENTFLOW_CLI_API_KEY=cli-key", "HOME=/home/ops", "LANG=C.UTF-8"}
	filtered := controlledBaseEnvironment(base)
	if len(filtered) != len(base) {
		t.Fatalf("ordinary variables were filtered: %v", filtered)
	}
	for _, name := range []string{"AGENTFLOW_RUN_ID", "AGENTFLOW_NODE_ID", "AGENTFLOW_CLI_API_KEY"} {
		if !environmentHas(filtered, name) {
			t.Fatalf("%s must survive the filter: %v", name, filtered)
		}
	}
}

// TestControlledEnvironmentKeepsOverlayVariables is the AUD-01 guard for the
// other half of the contract: the inherited base is filtered, but the per-run
// overlay is applied afterwards and must still reach the child. The ordinary
// base-variable test above already covers AGENTFLOW_RUN_ID and
// AGENTFLOW_CLI_API_KEY; AGENTFLOW_REPOSITORIES_JSON is the overlay key that
// had no guard, and the blocked AGENTFLOW_GIT_TOKEN proves an overlay is
// allowed to re-inject a key the base filter removed (its per-run credential).
func TestControlledEnvironmentKeepsOverlayVariables(t *testing.T) {
	overlay := map[string]string{
		"AGENTFLOW_REPOSITORIES_JSON": `[{"id":"repo-1","url":"https://example.test/r.git"}]`,
		"AGENTFLOW_CLI_API_KEY":       "cli-key",
		"AGENTFLOW_RUN_ID":            "run-1",
		"AGENTFLOW_GIT_TOKEN":         "per-run-token",
	}
	for _, mode := range []struct {
		name  string
		local string
	}{
		{"platform", ""},
		{"local", "1"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv("AGENTFLOW_LOCAL_MODE", mode.local)
			base := []string{
				"PATH=/usr/bin",
				"AGENTFLOW_SESSION_SECRET=signing-key",
				"AGENTFLOW_REPOSITORIES_JSON=stale-parent-value",
			}
			env := appendEnvironment(controlledBaseEnvironment(base), overlay)

			if environmentHas(env, "AGENTFLOW_SESSION_SECRET") {
				t.Fatalf("%s mode leaks AGENTFLOW_SESSION_SECRET: %v", mode.name, env)
			}
			for name, want := range overlay {
				got, ok := environmentValue(env, name)
				if !ok {
					t.Fatalf("%s mode dropped overlay variable %s: %v", mode.name, name, env)
				}
				if got != want {
					t.Fatalf("%s mode %s=%q want overlay value %q", mode.name, name, got, want)
				}
			}
		})
	}
}

// environmentValue returns the value of name in the KEY=VALUE list.
func environmentValue(entries []string, name string) (string, bool) {
	for _, entry := range entries {
		if key, value, ok := strings.Cut(entry, "="); ok && key == name {
			return value, true
		}
	}
	return "", false
}

// environmentHas reports whether the KEY=VALUE list carries name.
func environmentHas(entries []string, name string) bool {
	for _, entry := range entries {
		if strings.HasPrefix(entry, name+"=") {
			return true
		}
	}
	return false
}
