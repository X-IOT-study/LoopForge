package runtime

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestAgentStateDirectoriesKeepsCLIStatesOnly pins the docker scope: the
// resolution used by the container driver never widens with local mode.
func TestAgentStateDirectoriesKeepsCLIStatesOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	requirements := SandboxRequirements{CLIStates: []CLIStateRequirement{{Directory: ".claude", Files: []string{".claude.json"}}}}
	states, err := agentStateDirectories(requirements)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("agentStateDirectories = %d states, want exactly the requested one", len(states))
	}
	if states[0].path != filepath.Join(home, ".claude") {
		t.Fatalf("path = %q", states[0].path)
	}
	if got := strings.Join(states[0].files, ","); got != ".claude.json" {
		t.Fatalf("files = %q", got)
	}
	empty, err := agentStateDirectories(SandboxRequirements{})
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty requirements = %v %v, want none", empty, err)
	}
}

// TestLocalHostStateDirectoriesExtendsOnlyInLocalMode pins both directions:
// local mode adds every host CLI state directory plus the files beside it, the
// platform mode returns exactly the requested CLI states.
func TestLocalHostStateDirectoriesExtendsOnlyInLocalMode(t *testing.T) {
	t.Run("platform mode", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("AGENTFLOW_LOCAL_MODE", "")
		requirements := SandboxRequirements{CLIStates: []CLIStateRequirement{{Directory: ".codex"}}}
		states, err := localHostStateDirectories(requirements)
		if err != nil {
			t.Fatal(err)
		}
		if len(states) != 1 || states[0].path != filepath.Join(home, ".codex") {
			t.Fatalf("platform mode expanded the state list: %v", states)
		}
	})
	t.Run("local mode", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
		states, err := localHostStateDirectories(SandboxRequirements{})
		if err != nil {
			t.Fatal(err)
		}
		byPath := map[string][]string{}
		for _, state := range states {
			byPath[state.path] = state.files
		}
		for _, kind := range []string{"codebuddy", "codex", "claude-code", "opencode", "cursor", "agy", "kimi", "pi", "dsh", "zcode"} {
			dir := filepath.Join(home, filepath.FromSlash(agentCLIStateDirs[kind]))
			if _, ok := byPath[dir]; !ok {
				t.Errorf("local mode omitted the %s state directory %q", kind, dir)
			}
		}
		if got := strings.Join(byPath[filepath.Join(home, ".claude")], ","); got != ".claude.json" {
			t.Errorf("claude files = %q, want .claude.json", got)
		}
		if got := strings.Join(byPath[filepath.Join(home, ".codebuddy")], ","); got != ".codebuddy.json" {
			t.Errorf("codebuddy files = %q, want .codebuddy.json", got)
		}
	})
	t.Run("requested files merge without duplicates", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
		requirements := SandboxRequirements{CLIStates: []CLIStateRequirement{{Directory: ".claude", Files: []string{".claude.json", "settings.override.json"}}}}
		states, err := localHostStateDirectories(requirements)
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range states {
			if state.path != filepath.Join(home, ".claude") {
				continue
			}
			if got := strings.Join(state.files, ","); got != ".claude.json,settings.override.json" {
				t.Fatalf("merged files = %q", got)
			}
			return
		}
		t.Fatal("requested .claude state missing from the local list")
	})
}

// TestBwrapBindsStateFilesWritable proves the Linux driver binds the login file
// beside a state directory writable, and that a missing file is simply skipped
// instead of failing the sandbox.
func TestBwrapBindsStateFilesWritable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	claudeJSON := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(claudeJSON, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	args, err := bwrapArgsCommand(t.TempDir(), SandboxRequirements{}, false)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--bind "+claudeJSON+" "+claudeJSON) {
		t.Fatalf("bwrap does not bind %q writable: %s", claudeJSON, joined)
	}
	if strings.Contains(joined, ".codebuddy.json") {
		t.Fatalf("bwrap bound a file that does not exist: %s", joined)
	}

	// The file disappearing between resolution and execution must not fail.
	if err := os.Remove(claudeJSON); err != nil {
		t.Fatal(err)
	}
	args, err = bwrapArgsCommand(t.TempDir(), SandboxRequirements{}, false)
	if err != nil {
		t.Fatalf("missing state file failed the sandbox: %v", err)
	}
	if strings.Contains(strings.Join(args, " "), claudeJSON) {
		t.Fatalf("missing state file was still bound: %v", args)
	}
}

// TestSeatbeltStaysDirectoryLevel documents the macOS driver contract this
// cycle: directory-level writes only, no file-level bind.
func TestSeatbeltStaysDirectoryLevel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile, err := seatbeltProfileCommand(t.TempDir(), SandboxRequirements{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(profile, "(allow file-write* (subpath "+strconv.Quote(filepath.Join(home, ".claude"))+"))") {
		t.Fatalf("seatbelt lacks the state directory allowance: %s", profile)
	}
	if strings.Contains(profile, ".claude.json") {
		t.Fatalf("seatbelt must not bind the state file: %s", profile)
	}
}
