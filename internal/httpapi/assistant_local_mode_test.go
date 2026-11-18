package httpapi

import (
	"context"
	"slices"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

// Local mode runs the CLIs with the machine's own login, so a chat turn must
// not be refused for a missing personal API Key.
func TestAssistantLocalModeTurnNeedsNoAPIKey(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	h := &assistantHandlers{}
	stored := domain.User{ID: 1, LoginName: "local-owner"}
	for _, model := range []string{"codex:gpt-5-codex", "claude-code:claude-sonnet-4", assistantChatDefaultModel} {
		user, gotModel, refusal := h.assistantTurnUser(stored, model)
		if refusal != "" {
			t.Fatalf("%s refused in local mode: %s", model, refusal)
		}
		if gotModel != model || user.ID != stored.ID {
			t.Fatalf("%s resolved to model=%q user=%+v", model, gotModel, user)
		}
	}
	if _, shared, ok := h.assistantChatUser(stored); !ok || shared != nil {
		t.Fatalf("local chat user: ok=%v shared=%v", ok, shared)
	}
}

// hostCLILogins fakes a machine where every CLI with a probeable login is
// signed in, so the switch state does not depend on the machine running the
// test. Engines whose login cannot be probed (opencode, pi) are always listed.
func hostCLILogins(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{
		"CODEBUDDY_API_KEY", "OPENAI_API_KEY", "CODEX_API_KEY", "ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CURSOR_API_KEY",
		"CURSOR_AUTH_TOKEN", "GEMINI_API_KEY", "GOOGLE_API_KEY", "DEEPSEEK_API_KEY",
	} {
		t.Setenv(name, "host-login")
	}
}

func TestAssistantLocalModeEngineOptionsAllConfigured(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	hostCLILogins(t)
	options := assistantEngineOptions(domain.User{}, false)
	if len(options) == 0 {
		t.Fatal("no engines listed")
	}
	for _, option := range options {
		if !option.Configured {
			t.Fatalf("%s must be configured in local mode", option.ID)
		}
		if option.Credential == "" {
			t.Fatalf("%s lost its credential field", option.ID)
		}
		if option.ID == "kimi" || option.ID == "zcode" {
			t.Fatalf("local mode listed unsupported engine %s", option.ID)
		}
	}
}

// TestAssistantLocalModeEngineFollowsHostLogin proves the switch reports the
// machine's real login state instead of claiming every CLI is ready: a CLI the
// operator never signed into stays unconfigured, so the first turn is not the
// place that failure shows up.
func TestAssistantLocalModeEngineFollowsHostLogin(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"CODEBUDDY_API_KEY", "OPENAI_API_KEY", "CODEX_API_KEY"} {
		t.Setenv(name, "")
	}
	states := map[string]bool{}
	for _, option := range assistantEngineOptions(domain.User{}, false) {
		states[option.ID] = option.Configured
	}
	for _, engine := range []string{assistantEngineCodeBuddy, assistantEngineCodex} {
		if states[engine] {
			t.Fatalf("%s must stay unconfigured without a host login", engine)
		}
	}
	// A CLI with no probeable login is listed as available rather than hidden.
	if !states["opencode"] {
		t.Fatal("opencode must stay available: its login cannot be probed")
	}
}

// A CLI model picked in local mode must survive model normalization and list
// in the catalog without a personal key.
func TestAssistantLocalModeKeepsCLIModel(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	// A clean home keeps this about the built-in catalog; a machine whose CLI
	// names a gateway lists that gateway's models instead.
	t.Setenv("HOME", t.TempDir())
	catalog := assistantModelCatalog{Default: "glm-5.1"}
	if got := assistantAllowedUserModel("codex:gpt-6-sol", catalog, domain.User{}); got != "codex:gpt-6-sol" {
		t.Fatalf("local mode dropped the CLI model: %q", got)
	}
	models := assistantEngineModels(context.Background(), domain.User{})
	if !slices.ContainsFunc(models, func(m assistantModel) bool { return m.ID == "codex:gpt-6-sol" }) {
		t.Fatalf("local mode catalog lacks the codex model: %+v", models)
	}
}

// Platform mode keeps the key gates and the "configure me" switch state.
func TestAssistantPlatformModeStillRequiresAPIKey(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "0")
	h := &assistantHandlers{}
	stored := domain.User{ID: 1, LoginName: "platform-owner"}
	if _, _, refusal := h.assistantTurnUser(stored, "codex:gpt-5-codex"); !strings.Contains(refusal, "API Key") {
		t.Fatalf("codex without a key: %q", refusal)
	}
	if _, _, refusal := h.assistantTurnUser(stored, assistantChatDefaultModel); !strings.Contains(refusal, "API Key") {
		t.Fatalf("codebuddy without a key: %q", refusal)
	}
	for _, option := range assistantEngineOptions(stored, false) {
		if option.Configured {
			t.Fatalf("%s must not be configured without a key", option.ID)
		}
	}
	catalog := assistantModelCatalog{Default: "glm-5.1"}
	if got := assistantAllowedUserModel("codex:gpt-6-sol", catalog, stored); got != "glm-5.1" {
		t.Fatalf("platform mode kept a keyless CLI model: %q", got)
	}
}

// TestAssistantHostModelDropsPlaceholderInLocalMode proves the "CLI 默认模型"
// placeholder is not passed to a host CLI: the host configuration names the
// model, and no provider serves a model literally called "default".
func TestAssistantHostModelDropsPlaceholderInLocalMode(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	if got := assistantHostModel(assistantEngineCodex, "default"); got != "" {
		t.Fatalf("local mode must drop the placeholder, got %q", got)
	}
	if got := assistantHostModel(assistantEngineClaude, "default"); got != "" {
		t.Fatalf("claude keeps the placeholder: %q", got)
	}
	if got := assistantHostModel(assistantEngineCodex, "gpt-5.1-codex"); got != "gpt-5.1-codex" {
		t.Fatalf("a real model must survive, got %q", got)
	}
	if got := assistantHostModel(assistantEngineCodeBuddy, "default"); got != "default" {
		t.Fatalf("CodeBuddy keeps its platform-managed model, got %q", got)
	}
	t.Setenv("AGENTFLOW_LOCAL_MODE", "")
	if got := assistantHostModel(assistantEngineCodex, "default"); got != "default" {
		t.Fatalf("platform mode must keep the model, got %q", got)
	}
}
