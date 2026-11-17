package runner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentflow/internal/domain"
)

// writeCLI drops an executable sh script into dir and returns its path.
func writeCLI(t *testing.T, dir, name, script string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// runtimeFor picks one provider out of a detection report.
func runtimeFor(runtimes []domain.RunnerRuntime, provider string) (domain.RunnerRuntime, bool) {
	for _, rt := range runtimes {
		if rt.Provider == provider {
			return rt, true
		}
	}
	return domain.RunnerRuntime{}, false
}

// TestDetectRuntimesSkipsExcludedCLIInLocalMode covers the CLIs local mode does
// not offer: it must neither report them nor run their binaries, because the
// name on PATH may be a desktop application rather than a CLI (ZCode ships
// exactly that). Platform mode must keep probing and reporting them.
func TestDetectRuntimesSkipsExcludedCLIInLocalMode(t *testing.T) {
	setup := func(t *testing.T) string {
		t.Helper()
		bin := t.TempDir()
		t.Setenv("HOME", t.TempDir())
		t.Setenv("PATH", bin)
		return bin
	}
	// The sentinel is written next to the script ("$0.ran") by the shell
	// builtin, so an empty PATH cannot hide whether the binary really ran.
	const script = "#!/bin/sh\necho ran > \"$0.ran\"\necho 9.9.9\n"

	t.Run("local mode", func(t *testing.T) {
		bin := setup(t)
		writeCLI(t, bin, "zcode", script)
		writeCLI(t, bin, "kimi", script)
		writeCLI(t, bin, "codex", script)
		t.Setenv("AGENTFLOW_LOCAL_MODE", "1")

		runtimes := DetectRuntimes(context.Background(), nil)
		if rt, ok := runtimeFor(runtimes, "zcode"); ok {
			t.Fatalf("local mode reported zcode: %+v", rt)
		}
		if rt, ok := runtimeFor(runtimes, "kimi"); ok {
			t.Fatalf("local mode reported kimi: %+v", rt)
		}
		for _, name := range []string{"zcode.ran", "kimi.ran"} {
			if _, err := os.Stat(filepath.Join(bin, name)); !os.IsNotExist(err) {
				t.Fatalf("excluded CLI ran in local mode: %s exists (err=%v)", name, err)
			}
		}
		// Positive control: local mode filters by CLI, it does not skip detection
		// outright, so an allowed CLI must still be probed and reported.
		rt, ok := runtimeFor(runtimes, "codex")
		if !ok || rt.Version != "9.9.9" || rt.Status != domain.RuntimeStatusOnline {
			t.Fatalf("local mode codex = %+v ok=%v, want the 9.9.9 stub", rt, ok)
		}
		if _, err := os.Stat(filepath.Join(bin, "codex.ran")); err != nil {
			t.Fatalf("local mode did not probe an allowed CLI: %v", err)
		}
	})

	t.Run("platform mode", func(t *testing.T) {
		bin := setup(t)
		writeCLI(t, bin, "zcode", script)
		t.Setenv("AGENTFLOW_LOCAL_MODE", "")

		runtimes := DetectRuntimes(context.Background(), nil)
		rt, ok := runtimeFor(runtimes, "zcode")
		if !ok || rt.Version != "9.9.9" || rt.Status != domain.RuntimeStatusOnline {
			t.Fatalf("platform mode zcode = %+v ok=%v, want version 9.9.9", rt, ok)
		}
		if _, err := os.Stat(filepath.Join(bin, "zcode.ran")); err != nil {
			t.Fatalf("platform mode did not execute the probe: %v", err)
		}
	})
}

// TestProbeVersionKillsWholeProcessGroupOnTimeout covers a binary that answers
// nothing but starts children: the timeout must reclaim the whole group, not
// only the direct child, or every probe leaves a tree running.
func TestProbeVersionKillsWholeProcessGroupOnTimeout(t *testing.T) {
	previous := probeTimeout
	probeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { probeTimeout = previous })
	t.Setenv("PATH", "/usr/bin:/bin")

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	binary := writeCLI(t, dir, "zcode", `#!/bin/sh
( trap '' TERM; sleep 30 ) &
echo $! > `+pidFile+`
sleep 30
`)
	started := time.Now()
	if version := probeVersion(context.Background(), binary, nil); version != "" {
		t.Fatalf("probe returned %q, want empty on timeout", version)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("probe took %s, want it to stop at the shortened timeout", elapsed)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("child pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("child pid = %q (%v)", raw, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		// ESRCH is the portable proof a pid is gone, but a container whose PID 1
		// is not init can keep the child as an unreaped zombie; there kill(0)
		// still succeeds forever and this assertion would fail spuriously.
		err := syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("child %d survived the probe timeout (kill(0) err=%v)", pid, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestProbeVersionStillReadsAQuickCLI keeps the fast path honest: a CLI that
// prints a version and exits is reported even though the probe now owns a
// process group.
func TestProbeVersionStillReadsAQuickCLI(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	binary := writeCLI(t, t.TempDir(), "codex", "#!/bin/sh\necho 1.2.3\n# ignored second line\n")
	if version := probeVersion(context.Background(), binary, nil); version != "1.2.3" {
		t.Fatalf("version = %q, want 1.2.3", version)
	}
}
