package runner

import (
	"context"
	"strings"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

// TestAcquireTaskSandboxRejectsDockerInLocalMode pins the early rejection at
// run_sandbox.go:45-47: a local-mode node asked to run a docker sandbox fails
// closed with the documented code before any sandbox is opened, instead of
// surfacing an opaque CLI-login error from inside the container. A zero
// Daemon is enough because the rejection is the first thing after the nil
// definition check.
func TestAcquireTaskSandboxRejectsDockerInLocalMode(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	d := &Daemon{}
	task := domain.RunnerTask{
		ID:    "task-1",
		RunID: "run-1",
		Sandbox: &domain.RunnerSandboxSpec{
			Profile: domain.SandboxProfile{ID: "docker-profile", Driver: domain.SandboxDriverDocker},
		},
	}

	sandbox, release, err := d.acquireTaskSandbox(context.Background(), task)
	if err == nil {
		t.Fatal("local mode accepted a docker sandbox; want a fail-closed error")
	}
	if sandbox != nil || release != nil {
		t.Fatalf("rejection returned sandbox/release: sandbox=%v releaseSet=%v", sandbox, release != nil)
	}
	for _, want := range []string{runtime.LocalSandboxUnsupportedCode, domain.SandboxDriverDocker} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err.Error(), want)
		}
	}
}

// TestAcquireTaskSandboxRequiresFrozenDefinition keeps the neighbouring
// rejection (nil sandbox) covered: it runs before the docker check and must
// stay there.
func TestAcquireTaskSandboxRequiresFrozenDefinition(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "")
	d := &Daemon{}
	sandbox, release, err := d.acquireTaskSandbox(context.Background(), domain.RunnerTask{ID: "task-1", RunID: "run-1"})
	if err == nil {
		t.Fatal("nil sandbox definition was accepted")
	}
	if !strings.Contains(err.Error(), "frozen run sandbox definition is required") {
		t.Fatalf("error=%q", err.Error())
	}
	if sandbox != nil || release != nil {
		t.Fatalf("rejection returned sandbox/release: sandbox=%v releaseSet=%v", sandbox, release != nil)
	}
}
