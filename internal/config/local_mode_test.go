package config

import (
	"slices"
	"testing"
)

// TestLocalModeSwitch pins the switch semantics: only the values strconv
// accepts count as "on"; anything unparseable stays off so a typo cannot
// silently turn a platform deployment into a local-only instance.
func TestLocalModeSwitch(t *testing.T) {
	table := []struct {
		value string
		want  bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{"t", true},
		// "yes"/"on" are not strconv spellings: they stay off.
		{"yes", false},
		{"on", false},
		{"0", false},
		{"false", false},
		{"", false},
		{"  ", false},
		{"maybe", false},
	}
	for _, tc := range table {
		t.Setenv("AGENTFLOW_LOCAL_MODE", tc.value)
		if got := LocalMode(); got != tc.want {
			t.Errorf("LocalMode() with %q = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// TestLocalModeAllowRemoteSwitch keeps the escape hatch on the same rules.
func TestLocalModeAllowRemoteSwitch(t *testing.T) {
	table := []struct {
		value string
		want  bool
	}{
		{"1", true},
		{"true", true},
		{"0", false},
		{"", false},
		{"on", false},
	}
	for _, tc := range table {
		t.Setenv("AGENTFLOW_LOCAL_ALLOW_REMOTE", tc.value)
		if got := LocalModeAllowRemote(); got != tc.want {
			t.Errorf("LocalModeAllowRemote() with %q = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// TestLocalModeIdentity pins the fixed admin login and the rejection code the
// front end and the API docs key off.
func TestLocalModeIdentity(t *testing.T) {
	if got := LocalModeAdminLogin(); got != "local" {
		t.Fatalf("LocalModeAdminLogin() = %q, want local", got)
	}
	if got := LocalModeErrorCode; got != "local_mode_unsupported" {
		t.Fatalf("LocalModeErrorCode = %q", got)
	}
}

// TestLocalModeSupportsExecutor pins the executor switch for every built-in
// CLI kind in both modes: platform mode keeps all ten, local mode drops the
// two cloud-hosted providers and nothing else.
func TestLocalModeSupportsExecutor(t *testing.T) {
	allowed := []string{"codebuddy", "codex", "claude-code", "opencode", "cursor", "agy", "pi", "dsh"}
	blocked := []string{"kimi", "zcode"}
	table := []struct {
		name      string
		localMode string
		want      map[string]bool
	}{
		{
			name:      "local mode",
			localMode: "1",
			want:      map[string]bool{"codebuddy": true, "codex": true, "claude-code": true, "opencode": true, "cursor": true, "agy": true, "kimi": false, "pi": true, "dsh": true, "zcode": false},
		},
		{
			name:      "platform mode",
			localMode: "",
			want:      map[string]bool{"codebuddy": true, "codex": true, "claude-code": true, "opencode": true, "cursor": true, "agy": true, "kimi": true, "pi": true, "dsh": true, "zcode": true},
		},
	}
	kinds := append(append([]string(nil), allowed...), blocked...)
	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENTFLOW_LOCAL_MODE", tc.localMode)
			for _, kind := range kinds {
				if got := LocalModeSupportsExecutor(kind); got != tc.want[kind] {
					t.Errorf("LocalModeSupportsExecutor(%q) = %v, want %v", kind, got, tc.want[kind])
				}
			}
			unsupported := LocalModeUnsupportedExecutors()
			if tc.localMode == "" {
				if len(unsupported) != 0 {
					t.Fatalf("platform mode LocalModeUnsupportedExecutors() = %v, want empty", unsupported)
				}
				return
			}
			if len(unsupported) != len(blocked) {
				t.Fatalf("local mode LocalModeUnsupportedExecutors() = %v, want %v", unsupported, blocked)
			}
			for _, kind := range blocked {
				if LocalModeSupportsExecutor(kind) {
					t.Errorf("LocalModeUnsupportedExecutors lists %q but LocalModeSupportsExecutor says it is supported", kind)
				}
				if !slices.Contains(unsupported, kind) {
					t.Errorf("LocalModeUnsupportedExecutors() = %v, missing %q", unsupported, kind)
				}
			}
		})
	}
}
