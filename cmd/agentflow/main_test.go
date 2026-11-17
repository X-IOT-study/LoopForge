package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestManagedCLIShutdown(t *testing.T) {
	t.Setenv("AGENTFLOW_MANAGED_CLI_CONFIG", "invalid-policy.json")
	// Never let a startup check reach the public registry in an entrypoint test.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		role    string
		execute bool
		root    string
	}{
		{"worker", true, t.TempDir()}, {"all", true, t.TempDir()},
		{"api", true, t.TempDir()}, {"runner", true, t.TempDir()},
		{"worker", false, t.TempDir()}, {"all", false, t.TempDir()},
		{"worker", true, ""}, {"all", true, "  "},
	} {
		t.Setenv("AGENTFLOW_MANAGED_CLI_ROOT", tc.root)
		stop := startManagedCLIUpdates(ctx, tc.role, tc.execute)
		done := make(chan struct{})
		go func() { stop(); stop(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("shutdown blocked: %+v", tc)
		}
	}
}

func TestCLIAdmin(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENTFLOW_MANAGED_CLI_CONFIG", filepath.Join(root, "invalid.json"))
	for _, tc := range []struct {
		action, provider, root string
		timeout                time.Duration
		wantError              string
	}{
		{"status", "", root, time.Minute, ""},
		{"update", "", root, time.Nanosecond, "context deadline exceeded"},
		{"rollback", "unknown", root, time.Minute, "known provider"},
		{"rollback", "", root, time.Minute, "known provider"},
		{"rollback", "codex", root, time.Minute, "no previous managed version"},
		{"install", "", root, time.Minute, "invalid -cli-action"},
		{"status", "codex", root, time.Minute, "only supported with rollback"},
		{"update", "codex", root, time.Minute, "only supported with rollback"},
		{"status", "", "relative", time.Minute, "absolute"},
		{"status", "", "", time.Minute, "required"},
		{"status", "", root, 0, "positive"},
		{"status", "", root, -time.Second, "positive"},
	} {
		t.Setenv("AGENTFLOW_MANAGED_CLI_ROOT", tc.root)
		var out bytes.Buffer
		err := runCLI(context.Background(), tc.action, tc.provider, tc.timeout, &out)
		if tc.wantError == "" {
			if err != nil || !json.Valid(out.Bytes()) {
				t.Fatalf("%+v: output=%s err=%v", tc, out.String(), err)
			}
		} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
			t.Fatalf("%+v: output=%s err=%v", tc, out.String(), err)
		}
	}
	t.Setenv("AGENTFLOW_MANAGED_CLI_ROOT", root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runCLI(ctx, "update", "", time.Minute, &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled administration: %v", err)
	}
}

func TestCLIAdminIgnoresLegacyConfig(t *testing.T) {
	for _, content := range []string{`{"enabled":false}`, "invalid JSON"} {
		root := filepath.Join(t.TempDir(), "absent")
		config := filepath.Join(t.TempDir(), "legacy.json")
		if err := os.WriteFile(config, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AGENTFLOW_MANAGED_CLI_ROOT", root)
		t.Setenv("AGENTFLOW_MANAGED_CLI_CONFIG", config)
		var out bytes.Buffer
		if err := runCLI(context.Background(), "status", "", time.Minute, &out); err != nil || !json.Valid(out.Bytes()) {
			t.Fatalf("status with legacy config %q: %s %v", content, out.String(), err)
		}
		// Core tests cover all three providers with fake metadata/npm. A deadline
		// here guarantees ignoring disabled policy cannot trigger real updates.
		if err := runCLI(context.Background(), "update", "", time.Nanosecond, &out); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("update with legacy config %q: %v", content, err)
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("read-only status/expired operation created root: %v", err)
		}
	}
}

func TestCLIAdminExitsBeforeServices(t *testing.T) {
	if args := os.Getenv("TEST_CLI_ARGS"); args != "" {
		os.Args = append([]string{os.Args[0]}, strings.Fields(args)...)
		main()
		os.Exit(0)
	}
	root := filepath.Join(t.TempDir(), "absent")
	t.Setenv("AGENTFLOW_MANAGED_CLI_ROOT", root)
	t.Setenv("AGENTFLOW_MANAGED_CLI_CONFIG", filepath.Join(root, "invalid.json"))
	t.Setenv("AGENTFLOW_MYSQL_DSN", "invalid")
	t.Setenv("AGENTFLOW_ROLE", "invalid")
	for _, tc := range []struct{ args, wantError string }{
		{"-cli-action=status", ""},
		{"-cli-action=update -cli-timeout=1ns", "context deadline exceeded"},
		{"-cli-action=rollback -cli-provider=codex -cli-timeout=1ns", "context deadline exceeded"},
		{"-cli-action=unknown", "invalid -cli-action"},
		{"-cli-action=status -cli-timeout=0", "positive"},
		{"-cli-action=status -cli-provider=codex", "only supported with rollback"},
		{"-cli-action=status extra", "does not accept positional arguments"},
	} {
		t.Setenv("TEST_CLI_ARGS", tc.args)
		cmd := exec.Command(os.Args[0], "-test.run=^TestCLIAdminExitsBeforeServices$")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if tc.wantError == "" {
			if err != nil || !json.Valid(out) {
				t.Fatalf("%s: output=%s stderr=%s err=%v", tc.args, out, stderr.String(), err)
			}
		} else if err == nil || !strings.Contains(stderr.String(), tc.wantError) {
			t.Fatalf("%s: output=%s stderr=%s err=%v", tc.args, out, stderr.String(), err)
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("read-only status/expired operation created root: %v", err)
	}
}

func TestResolveRole(t *testing.T) {
	t.Setenv("AGENTFLOW_ROLE", "")
	for input, want := range map[string]string{"": "all", "API": "api", "scheduler": ""} {
		got, err := resolveRole(input)
		if got != want || (err != nil) != (want == "") {
			t.Fatalf("role %q: %q, %v", input, got, err)
		}
	}
}

// TestLocalSessionSecretCreatesAndReuses covers the ordinary path: the first
// start mints a key, every later start reads the same one back.
func TestLocalSessionSecretCreatesAndReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local", "session.key")
	first, created, err := localSessionSecret(path)
	if err != nil || !created {
		t.Fatalf("first start: created=%v err=%v", created, err)
	}
	if len(first) < 32 {
		t.Fatalf("key looks too short: %q", first)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key mode = %04o, want 0600", perm)
	}
	second, created, err := localSessionSecret(path)
	if err != nil || created {
		t.Fatalf("second start: created=%v err=%v", created, err)
	}
	if second != first {
		t.Fatalf("key changed between starts")
	}
}

// TestLocalSessionSecretRefusesSymlink makes sure a planted symlink cannot
// redirect the signing key to a file the operator does not control.
func TestLocalSessionSecretRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.key")
	if err := os.WriteFile(target, []byte("attacker-chosen-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session.key")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, err := localSessionSecret(path); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlinked key must be refused, got %v", err)
	}
}

// TestLocalSessionSecretTightensPermissions proves a widened key keeps its value
// but loses the extra group/other bits.
func TestLocalSessionSecretTightensPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.key")
	if err := os.WriteFile(path, []byte("preexisting-key"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret, created, err := localSessionSecret(path)
	if err != nil || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if secret != "preexisting-key" {
		t.Fatalf("existing key must be kept, got %q", secret)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key mode = %04o, want 0600", perm)
	}
}

// TestLocalSessionSecretReplacesEmptyFile covers a crash that left a zero-byte
// key: it cannot sign anything, so a fresh key is minted.
func TestLocalSessionSecretReplacesEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.key")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	secret, created, err := localSessionSecret(path)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(content)) != secret {
		t.Fatalf("file content %q does not match returned key", content)
	}
	if secret == "" {
		t.Fatal("empty key returned")
	}
}

// TestLocalSessionSecretConcurrentFirstStart starts several processes' worth of
// bootstraps at once: exactly one may claim creation and every caller must end
// up with the key that is on disk, otherwise half the cookies would be invalid.
func TestLocalSessionSecretConcurrentFirstStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local", "session.key")
	const workers = 8
	secrets := make([]string, workers)
	createdFlags := make([]bool, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			secrets[i], createdFlags[i], errs[i] = localSessionSecret(path)
		}(i)
	}
	close(start)
	wg.Wait()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	onDisk := strings.TrimSpace(string(content))
	created := 0
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d failed: %v", i, errs[i])
		}
		if createdFlags[i] {
			created++
		}
		if secrets[i] != onDisk {
			t.Fatalf("worker %d returned a key that is not on disk", i)
		}
	}
	if created != 1 {
		t.Fatalf("%d workers claimed to create the key, want 1", created)
	}
}
