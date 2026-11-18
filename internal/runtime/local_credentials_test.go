package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

// isolateLocalCredentials makes the host probes hermetic: a temporary HOME and
// every credential or locator variable cleared, so the test never depends on
// the machine it runs on.
func isolateLocalCredentials(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, names := range localCredentialEnvs {
		for _, name := range names {
			t.Setenv(name, "")
		}
	}
	for _, name := range []string{
		"AGENTFLOW_CLI_API_KEY", "KIMI_MODEL_API_KEY", "KIMI_MODEL_BASE_URL", "KIMI_MODEL_MODEL",
		"OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_CONFIG_CONTENT",
		"CODEX_HOME", "CLAUDE_CONFIG_DIR", "KIMI_CODE_HOME", "DSH_HOME",
		"PI_CODING_AGENT_DIR", "ZCODE_DATA_BASE_DIR", "ZCODE_STORAGE_DIR",
		"ZCODE_PERSONAL_PROVIDER_CONFIG_FILE",
	} {
		t.Setenv(name, "")
	}
	return home
}

// TestLocalCredentialStrengthTable pins the two-state classification: a
// provider is either confirmed by credential-class evidence or unknown, and an
// unlisted provider is unknown rather than a third "none" class.
func TestLocalCredentialStrengthTable(t *testing.T) {
	table := []struct {
		provider string
		want     string
	}{
		{"codebuddy", LocalCredentialConfirmed},
		{"codex", LocalCredentialConfirmed},
		{"claude", LocalCredentialConfirmed},
		{"claude-code", LocalCredentialConfirmed},
		{"cursor", LocalCredentialConfirmed},
		{"cursor-agent", LocalCredentialConfirmed},
		{"antigravity", LocalCredentialConfirmed},
		{"agy", LocalCredentialConfirmed},
		{"kimi-code", LocalCredentialConfirmed},
		{"deepseek", LocalCredentialConfirmed},
		{"opencode", LocalCredentialUnknown},
		{"pi", LocalCredentialUnknown},
		{"zcode", LocalCredentialUnknown},
		{"z-code", LocalCredentialUnknown},
		{"unlisted-provider", LocalCredentialUnknown},
		{"", LocalCredentialUnknown},
	}
	for _, tc := range table {
		if got := LocalCredentialStrength(tc.provider); got != tc.want {
			t.Errorf("LocalCredentialStrength(%q) = %q, want %q", tc.provider, got, tc.want)
		}
	}
}

// TestLocalCLILoginAvailableStates covers the four observation states of a
// confirmed provider: a valid env value, a valid login file, an empty login
// file and a directory squatting on the login path.
func TestLocalCLILoginAvailableStates(t *testing.T) {
	t.Run("env valid", func(t *testing.T) {
		isolateLocalCredentials(t)
		t.Setenv("CODEX_API_KEY", "sk-host")
		if !LocalCLILoginAvailable("codex") {
			t.Fatal("host CODEX_API_KEY must count as a login")
		}
	})
	t.Run("file valid", func(t *testing.T) {
		home := isolateLocalCredentials(t)
		writeLocalCredentialFile(t, filepath.Join(home, ".codex", "auth.json"), `{"OPENAI_API_KEY":"sk-host"}`)
		if !LocalCLILoginAvailable("codex") {
			t.Fatal("non-empty ~/.codex/auth.json must count as a login")
		}
	})
	t.Run("file empty", func(t *testing.T) {
		home := isolateLocalCredentials(t)
		writeLocalCredentialFile(t, filepath.Join(home, ".codex", "auth.json"), "")
		if LocalCLILoginAvailable("codex") {
			t.Fatal("an empty login file is not evidence of a login")
		}
	})
	t.Run("directory on the login path", func(t *testing.T) {
		home := isolateLocalCredentials(t)
		if err := os.MkdirAll(filepath.Join(home, ".codex", "auth.json"), 0o755); err != nil {
			t.Fatal(err)
		}
		if LocalCLILoginAvailable("codex") {
			t.Fatal("a directory is not a login file")
		}
	})
	t.Run("claude-code is env only", func(t *testing.T) {
		home := isolateLocalCredentials(t)
		writeLocalCredentialFile(t, filepath.Join(home, ".claude", ".credentials.json"), `{"token":"x"}`)
		if LocalCLILoginAvailable("claude-code") {
			t.Fatal("claude-code login state is judged by env only (tech-design §4.1)")
		}
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "host-token")
		if !LocalCLILoginAvailable("claude-code") {
			t.Fatal("host ANTHROPIC_AUTH_TOKEN must count as a login")
		}
	})
	t.Run("unknown provider never confirms", func(t *testing.T) {
		home := isolateLocalCredentials(t)
		if err := os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0o755); err != nil {
			t.Fatal(err)
		}
		if LocalCLILoginAvailable("opencode") {
			t.Fatal("an unknown-strength provider must never report a confirmed login")
		}
	})
}

// TestLocalCredentialCategoryAIsNotEvidence proves the platform-managed
// configuration written by the Agent CLI bindings (MCP, model, permissions) and
// the platform-injected key are never read as a host login.
func TestLocalCredentialCategoryAIsNotEvidence(t *testing.T) {
	home := isolateLocalCredentials(t)
	for _, rel := range []string{
		".codex/config.toml",
		".claude/settings.json",
		".codebuddy/config.json",
		".config/opencode/opencode.json",
		".kimi-code/mcp.json",
		".dsh/agentflow-mcp.json",
	} {
		writeLocalCredentialFile(t, filepath.Join(home, filepath.FromSlash(rel)), "{}")
	}
	// Platform-injected credential: it belongs to AgentFlow, not to the host.
	t.Setenv("AGENTFLOW_CLI_API_KEY", "agentflow-injected")
	t.Setenv("KIMI_MODEL_API_KEY", "agentflow-injected")
	for _, provider := range []string{"codex", "claude-code", "codebuddy", "kimi", "dsh", "opencode"} {
		if LocalCLILoginAvailable(provider) {
			t.Errorf("%s: platform-managed config counted as a host login", provider)
		}
	}
	if got := LocalCredentialEnvironments("kimi"); len(got) != 0 {
		t.Errorf("platform-injected KIMI_MODEL_API_KEY leaked into the host environment: %v", got)
	}
}

// TestLocalCredentialLocatorEnvIsNotEvidence proves locator variables point at
// configuration locations, so a config file they name is still not a login.
func TestLocalCredentialLocatorEnvIsNotEvidence(t *testing.T) {
	home := isolateLocalCredentials(t)
	locators := map[string]string{
		"codex":       "CODEX_HOME",
		"claude-code": "CLAUDE_CONFIG_DIR",
		"kimi":        "KIMI_CODE_HOME",
		"dsh":         "DSH_HOME",
		"pi":          "PI_CODING_AGENT_DIR",
		"opencode":    "OPENCODE_CONFIG",
		"zcode":       "ZCODE_DATA_BASE_DIR",
	}
	for provider, name := range locators {
		dir := filepath.Join(home, "locator", provider)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeLocalCredentialFile(t, filepath.Join(dir, "auth.json"), `{"OPENAI_API_KEY":"sk-locator"}`)
		writeLocalCredentialFile(t, filepath.Join(dir, "config.json"), "{}")
		t.Setenv(name, dir)
		if LocalCLILoginAvailable(provider) {
			t.Errorf("%s: locator variable %s counted as a credential", provider, name)
		}
	}
	// A locator pointing at a real codex login file is still not the probe path.
	t.Setenv("CODEX_HOME", filepath.Join(home, "locator", "codex"))
	if LocalCLILoginAvailable("codex") {
		t.Fatal("CODEX_HOME is a locator, not login evidence")
	}
}

// TestLocalCredentialEnvironmentsPassesOnlyConfirmedNames keeps the pass-through
// list identical to the confirmed credential table.
func TestLocalCredentialEnvironmentsPassesOnlyConfirmedNames(t *testing.T) {
	isolateLocalCredentials(t)
	t.Setenv("ANTHROPIC_API_KEY", "host-anthropic")
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/not-a-credential")
	t.Setenv("AGENTFLOW_CLI_API_KEY", "agentflow-injected")
	got := LocalCredentialEnvironments("claude-code")
	if got["ANTHROPIC_API_KEY"] != "host-anthropic" {
		t.Fatalf("host credential missing from pass-through: %v", got)
	}
	if _, ok := got["CLAUDE_CONFIG_DIR"]; ok {
		t.Fatalf("locator variable passed through: %v", got)
	}
	if _, ok := got["AGENTFLOW_CLI_API_KEY"]; ok {
		t.Fatalf("platform key passed through: %v", got)
	}
}

// TestLocalCredentialHintIsReadable pins the user-facing guidance: the code,
// the login command when the CLI documents one, and personal-config fallback.
func TestLocalCredentialHintIsReadable(t *testing.T) {
	table := []struct {
		provider string
		want     []string
	}{
		{"codex", []string{"local_credentials_missing", "codex login", "个人配置"}},
		{"claude-code", []string{"local_credentials_missing", "claude login", "个人配置"}},
		{"codebuddy", []string{"local_credentials_missing", "CODEBUDDY_API_KEY", "个人配置"}},
		{"opencode", []string{"local_credentials_missing", "个人配置"}},
	}
	for _, tc := range table {
		err := LocalCredentialHint(tc.provider)
		if err == nil {
			t.Fatalf("%s: hint is nil", tc.provider)
		}
		for _, fragment := range tc.want {
			if !strings.Contains(err.Error(), fragment) {
				t.Errorf("%s: hint %q lacks %q", tc.provider, err.Error(), fragment)
			}
		}
	}
}

// TestLocalSandboxUnsupported proves the container driver is rejected only in
// local mode, with the documented code.
func TestLocalSandboxUnsupported(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	if !LocalSandboxUnsupported("docker") {
		t.Fatal("local mode must reject the docker driver")
	}
	for _, driver := range []string{"bwrap", "seatbelt", "local", "native", ""} {
		if LocalSandboxUnsupported(driver) {
			t.Errorf("local mode rejected host driver %q", driver)
		}
	}
	err := LocalSandboxUnsupportedError("docker")
	if err == nil || !strings.Contains(err.Error(), "local_sandbox_unsupported") {
		t.Fatalf("unsupported-sandbox error lacks its code: %v", err)
	}
	t.Setenv("AGENTFLOW_LOCAL_MODE", "")
	if LocalSandboxUnsupported("docker") {
		t.Fatal("platform mode must keep the docker driver")
	}
}

// TestBindCommandLocalCredentialFallbacks proves the execution-side rule: host
// login first, personal key second, readable error last. Unknown-strength
// providers keep an empty environment and never fail the node.
func TestBindCommandLocalCredentialFallbacks(t *testing.T) {
	t.Run("no host login and no key fails readably", func(t *testing.T) {
		isolateLocalCredentials(t)
		t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
		cases := []struct {
			provider string
			binding  CredentialBinding
		}{
			{"codebuddy", BindCodeBuddy},
			{"codex", BindCodex},
			{"claude-code", BindClaudeCode},
		}
		for _, tc := range cases {
			command := CommandSpec{Argv: []string{tc.provider, "exec"}, Provider: tc.provider, CredentialBindings: []CredentialBinding{tc.binding}}
			_, err := bindCommand(nil, t.TempDir(), command, RunCredentials{})
			if err == nil {
				t.Fatalf("%s: local mode without a login or key must fail", tc.provider)
			}
			if !strings.Contains(err.Error(), localCredentialMissingCode) {
				t.Fatalf("%s: error lacks local_credentials_missing: %v", tc.provider, err)
			}
		}
	})
	t.Run("personal key fills the gap", func(t *testing.T) {
		isolateLocalCredentials(t)
		t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
		command := CommandSpec{Argv: []string{"codex", "exec"}, Provider: "codex", CredentialBindings: []CredentialBinding{BindCodex}}
		bound, err := bindCommand(nil, t.TempDir(), command, RunCredentials{Codex: ProviderCredential{APIKey: "sk-personal"}})
		if err != nil {
			t.Fatalf("personal key must satisfy the binding: %v", err)
		}
		if bound.environment["CODEX_API_KEY"] != "sk-personal" {
			t.Fatalf("personal key not injected: %v", bound.environment)
		}
	})
	t.Run("host login wins over the personal key", func(t *testing.T) {
		isolateLocalCredentials(t)
		t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
		t.Setenv("ANTHROPIC_API_KEY", "host-anthropic")
		command := CommandSpec{Argv: []string{"claude", "-p"}, Provider: "claude-code", CredentialBindings: []CredentialBinding{BindClaudeCode}}
		bound, err := bindCommand(nil, t.TempDir(), command, RunCredentials{ClaudeCode: ProviderCredential{APIKey: "sk-personal"}})
		if err != nil {
			t.Fatalf("host login must satisfy the binding: %v", err)
		}
		if bound.environment["ANTHROPIC_API_KEY"] != "host-anthropic" {
			t.Fatalf("host login was overridden: %v", bound.environment)
		}
	})
	t.Run("unknown provider without a key leaves an empty environment", func(t *testing.T) {
		isolateLocalCredentials(t)
		t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
		t.Setenv("AGENTFLOW_CLI_API_KEY", "agentflow-injected")
		command := CommandSpec{Argv: []string{"opencode", "run"}, Provider: AgentCLIOpenCode, CredentialBindings: []CredentialBinding{BindAgentCLI}}
		bound, err := bindCommand(nil, t.TempDir(), command, RunCredentials{})
		if err != nil {
			t.Fatalf("an unknown-strength provider must not fail the node: %v", err)
		}
		if bound.environment["AGENTFLOW_CLI_API_KEY"] != "" {
			t.Fatalf("platform key survived for a keyless local run: %v", bound.environment)
		}
	})
	t.Run("unknown provider with a personal key injects it", func(t *testing.T) {
		isolateLocalCredentials(t)
		t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
		command := CommandSpec{
			Argv:               []string{"pi", "run"},
			Provider:           AgentCLIPi,
			CredentialBindings: []CredentialBinding{BindAgentCLI},
			AgentCLI:           AgentCLIOptions{Model: "test-model"},
		}
		cred := RunCredentials{AgentCLI: map[string]ProviderCredential{
			AgentCLIPi: {APIKey: "sk-personal", BaseURL: "https://example.invalid/v1"},
		}}
		bound, err := bindCommand(nil, t.TempDir(), command, cred)
		if err != nil {
			t.Fatalf("personal agent CLI key must be applied: %v", err)
		}
		if bound.environment[agentCLIKeyEnv] != "sk-personal" {
			t.Fatalf("personal agent CLI key not injected: %v", bound.environment)
		}
	})
}

// TestBindCommandPlatformModeUnchanged guards the platform behaviour while the
// local branches above exist.
func TestBindCommandPlatformModeUnchanged(t *testing.T) {
	isolateLocalCredentials(t)
	t.Setenv("AGENTFLOW_LOCAL_MODE", "")
	command := CommandSpec{Argv: []string{"codex", "exec"}, Provider: "codex", CredentialBindings: []CredentialBinding{BindCodex}}
	_, err := bindCommand(nil, t.TempDir(), command, RunCredentials{})
	if err == nil || !strings.Contains(err.Error(), "需求责任人未配置 Codex API Key") {
		t.Fatalf("platform mode must keep its personal-key error, got %v", err)
	}
}

func writeLocalCredentialFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLocalSandboxUnsupportedCodePinned pins the literals the workflow state and
// the local-mode gate compare against, so renaming a constant cannot silently
// change the wire contract.
func TestLocalSandboxUnsupportedCodePinned(t *testing.T) {
	if LocalSandboxUnsupportedCode != "local_sandbox_unsupported" {
		t.Fatalf("sandbox code drifted: %q", LocalSandboxUnsupportedCode)
	}
	if localCredentialMissingCode != "local_credentials_missing" {
		t.Fatalf("credential code drifted: %q", localCredentialMissingCode)
	}
}

// TestCreateDriverSandboxRejectsDockerLocally proves the driver factory itself
// refuses the container driver, so assistant conversations and engine runs fail
// closed too, not only runner tasks. Platform mode must stay untouched.
func TestCreateDriverSandboxRejectsDockerLocally(t *testing.T) {
	profile := domain.SandboxProfile{Driver: "docker"}
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	sandbox, err := createDriverSandbox(context.Background(), "test", "id", t.TempDir(), profile, SandboxRequirements{}, domain.DockerfileDefinition{})
	if err == nil {
		if sandbox != nil {
			_ = sandbox.Close(context.Background())
		}
		t.Fatal("local mode must reject the docker driver")
	}
	if !strings.Contains(err.Error(), LocalSandboxUnsupportedCode) {
		t.Fatalf("driver factory error lacks the code: %v", err)
	}

	t.Setenv("AGENTFLOW_LOCAL_MODE", "")
	if _, err := createDriverSandbox(context.Background(), "test", "id", t.TempDir(), profile, SandboxRequirements{}, domain.DockerfileDefinition{}); err != nil && strings.Contains(err.Error(), LocalSandboxUnsupportedCode) {
		t.Fatalf("platform mode must not use the local-mode code: %v", err)
	}
}

// TestWithPrivateHomeDirKeepsHostHomeInLocalMode proves the assistant and
// conversation paths cannot redirect HOME/CODEX_HOME at a CLI in local mode:
// the host login and CLI configuration live in the operator's own profile.
func TestWithPrivateHomeDirKeepsHostHomeInLocalMode(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	for _, provider := range []string{"codex", "claude-code", "codebuddy"} {
		// The real specs (provider_codex.go, provider_claude.go,
		// provider_codebuddy.go) set PrivateHome, so the case starts from true
		// and the assertion below proves the flag is cleared.
		command := CommandSpec{Argv: []string{"true"}, Provider: provider, PrivateHome: true}.WithPrivateHomeDir(t.TempDir())
		for _, key := range []string{"HOME", "TMPDIR", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "CODEBUDDY_CONFIG_DIR"} {
			if value, ok := command.environment[key]; ok {
				t.Fatalf("%s: local mode must keep the host %s, got %q", provider, key, value)
			}
		}
		if command.PrivateHome {
			t.Fatalf("%s: local mode must not mark the command private-home", provider)
		}
	}
	t.Setenv("AGENTFLOW_LOCAL_MODE", "")
	dir := t.TempDir()
	command := CommandSpec{Argv: []string{"true"}, Provider: "codex"}.WithPrivateHomeDir(dir)
	if command.environment["HOME"] != dir || command.environment["CODEX_HOME"] != filepath.Join(dir, ".codex") {
		t.Fatalf("platform mode must keep the private home, got %+v", command.environment)
	}
}
