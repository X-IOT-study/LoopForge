package engine

import (
	"strings"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

// TestResolveNodeExecutorRejectsExcludedCLIInLocalMode covers the run-time
// backstop: a workflow saved before the CLIs were withdrawn (or imported from a
// platform export) still resolves to kimi, and the engine must refuse with a
// readable error instead of trying to start it. The test is deliberately not
// parallel: t.Setenv forbids it, and a kept kind must still resolve.
func TestResolveNodeExecutorRejectsExcludedCLIInLocalMode(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	agent := domain.AgentDefinition{ID: "kimi-analyst", Name: "Kimi analyst", ExecutorID: "kimi"}
	if _, err := resolveNodeExecutor(s, domain.WorkflowNode{AgentID: agent.ID}, agent); err == nil || !strings.Contains(err.Error(), "本地模式不支持执行器 kimi") {
		t.Fatalf("kimi executor must be refused in local mode: %v", err)
	}
	zcode := domain.AgentDefinition{ID: "zcode-analyst", Name: "ZCode analyst", ExecutorID: "zcode"}
	if _, err := resolveNodeExecutor(s, domain.WorkflowNode{AgentID: zcode.ID}, zcode); err == nil || !strings.Contains(err.Error(), "本地模式不支持执行器 zcode") {
		t.Fatalf("zcode executor must be refused in local mode: %v", err)
	}

	kept := domain.AgentDefinition{ID: "codex-analyst", Name: "Codex analyst", ExecutorID: "codex"}
	got, err := resolveNodeExecutor(s, domain.WorkflowNode{AgentID: kept.ID}, kept)
	if err != nil {
		t.Fatalf("codex executor must still resolve in local mode: %v", err)
	}
	if got.ID != "codex" {
		t.Fatalf("executor = %q, want codex", got.ID)
	}
}
