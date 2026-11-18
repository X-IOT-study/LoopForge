package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// hostCodexHome writes a codex home the way an operator's machine has it: a
// config.toml naming the gateway, and a model catalog describing the models it
// already knows.
func hostCodexHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	catalogPath := filepath.Join(dir, "host-models.json")
	catalog := `{"models":[{"slug":"group/known","display_name":"Known","shell_type":"unified_exec","context_window":1000000,"base_instructions":"x"}]}`
	if err := os.WriteFile(catalogPath, []byte(catalog), 0o600); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	config := fmt.Sprintf(`model = "group/known"
model_provider = "gateway"
model_catalog_json = %q

[model_providers.gateway]
base_url = "http://127.0.0.1:3425/v1"
experimental_bearer_token = "test-gateway-token"
`, catalogPath)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return dir
}

func TestHostModelSourceReadsCodexGateway(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	hostCodexHome(t)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CODEX_API_KEY", "")

	source, ok := HostModelSource("codex")
	if !ok {
		t.Fatal("expected the host configuration to be read")
	}
	if source.BaseURL != "http://127.0.0.1:3425/v1" {
		t.Fatalf("base url = %q", source.BaseURL)
	}
	if source.APIKey != "test-gateway-token" {
		t.Fatalf("api key = %q", source.APIKey)
	}
	if len(source.Models) != 1 || source.Models[0].ID != "group/known" {
		t.Fatalf("models = %v", source.Models)
	}
}

func TestHostModelSourceIsEmptyOnThePlatform(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "")
	hostCodexHome(t)
	if _, ok := HostModelSource("codex"); ok {
		t.Fatal("platform mode must keep using the platform catalog")
	}
}

func TestWriteCodexModelCatalogAddsUnknownGatewayModels(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	hostCodexHome(t)
	target := filepath.Join(t.TempDir(), "codex-model-catalog.json")

	path, err := WriteCodexModelCatalog(target, []HostModelCatalogEntry{
		{ID: "group/known", Name: "Known"},
		{ID: "group/new", Name: "New", ContextWindow: 2000000},
	})
	if err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	if path != target {
		t.Fatalf("path = %q, want %q", path, target)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	var catalog struct {
		Models []struct {
			Slug             string `json:"slug"`
			DisplayName      string `json:"display_name"`
			ShellType        string `json:"shell_type"`
			BaseInstructions string `json:"base_instructions"`
			ContextWindow    int    `json:"context_window"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(catalog.Models) != 2 {
		t.Fatalf("models = %d, want the host entry plus the new one", len(catalog.Models))
	}
	known, added := catalog.Models[0], catalog.Models[1]
	if known.Slug != "group/known" || known.ContextWindow != 1000000 {
		t.Fatalf("host entry was rewritten: %+v", known)
	}
	if added.Slug != "group/new" || added.DisplayName != "New" || added.ContextWindow != 2000000 {
		t.Fatalf("added entry = %+v", added)
	}
	// Codex validates every entry field, so the added one carries the template's.
	if added.ShellType != "unified_exec" || added.BaseInstructions != "x" {
		t.Fatalf("added entry lost the template: %+v", added)
	}
}

func TestWriteCodexModelCatalogSkipsWhenNothingIsMissing(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	hostCodexHome(t)

	path, err := WriteCodexModelCatalog(filepath.Join(t.TempDir(), "catalog.json"), []HostModelCatalogEntry{{ID: "group/known"}})
	if err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	if path != "" {
		t.Fatalf("path = %q, want no override when the host catalog already covers it", path)
	}
}

func TestWriteCodexModelCatalogIsOffOnThePlatform(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "")
	hostCodexHome(t)

	path, err := WriteCodexModelCatalog(filepath.Join(t.TempDir(), "catalog.json"), []HostModelCatalogEntry{{ID: "group/new"}})
	if err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	if path != "" {
		t.Fatalf("path = %q, want no override in platform mode", path)
	}
}

// hostPiHome writes the pi agent directory the way an operator's machine has
// it: one gateway with its models, named as the default in settings.
func hostPiHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	dir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	models := `{"providers":{"other":{"models":[{"id":"other-model","name":"Other"}]},"magpie":{"api":"openai-completions","apiKey":"magpie","baseUrl":"http://127.0.0.1:3425/v1","models":[{"id":"group/known","name":"Known"},{"id":"group/other","name":"Other"}]}}}`
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(models), 0o600); err != nil {
		t.Fatalf("write models: %v", err)
	}
	settings := `{"defaultProvider":"magpie","defaultModel":"group/known"}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	return dir
}

func TestHostModelSourceReadsPiProviders(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	hostPiHome(t)

	source, ok := HostModelSource("pi")
	if !ok || len(source.Models) != 3 {
		t.Fatalf("ok = %v, models = %v", ok, source.Models)
	}
	// The provider the operator starts in comes first, and every model keeps
	// the provider prefix pi's own command line needs.
	if source.Models[0].ID != "magpie/group/known" || source.Models[0].Name != "Known" {
		t.Fatalf("first model = %v", source.Models[0])
	}
	if source.Models[1].ID != "magpie/group/other" {
		t.Fatalf("second model = %v", source.Models[1])
	}
	if source.Models[2].ID != "other/other-model" {
		t.Fatalf("third model = %v", source.Models[2])
	}
}

func TestHostModelSourceReadsOpenCodeProviders(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	config := `{"model":"magpie/group/known","providers":{"magpie":{"models":{"group/known":{"name":"Known"},"group/other":{"name":"Other"}}}},"disabled_providers":["xcs"]}`
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	source, ok := HostModelSource("opencode")
	if !ok || len(source.Models) != 2 {
		t.Fatalf("ok = %v, models = %v", ok, source.Models)
	}
	// The config's default model is the first entry, the rest follow sorted.
	if source.Models[0].ID != "magpie/group/known" || source.Models[1].ID != "magpie/group/other" {
		t.Fatalf("models = %v", source.Models)
	}
}

func TestHostModelSourceReadsDeepSeekHarnessProviders(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DSH_HOME", "")
	dir := filepath.Join(home, ".dsh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	patch := filepath.Join(dir, "agent-default-model.json")
	settings := `llm-pi-ai:
  providers:
    magpie:
      baseURL: http://127.0.0.1:3425/v1
      models:
        - id: group/known
          name: Known
        - id: group/other
          name: Other
    magpie-anthropic:
      baseURL: http://127.0.0.1:3425
      models:
        - id: api-xcdw-us/claude-sonnet-5-5
          name: Sonnet
` + fmt.Sprintf(`patches:
  - id: agent-default-model
    config:
      provider: magpie-anthropic
      model: api-xcdw-us/claude-sonnet-5-5
`)
	if err := os.WriteFile(patch, nil, 0o600); err != nil {
		t.Fatalf("write patch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(settings), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	source, ok := HostModelSource("dsh")
	if !ok || len(source.Models) != 3 {
		t.Fatalf("ok = %v, models = %v", ok, source.Models)
	}
	// The configured default model leads; its provider is named in the ID
	// because that is how DeepSeek Harness addresses a model.
	if source.Models[0].ID != "magpie-anthropic/api-xcdw-us/claude-sonnet-5-5" {
		t.Fatalf("first model = %v", source.Models[0])
	}
}

func TestHostModelSourceIgnoresMissingCLIConfiguration(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, provider := range []string{"opencode", "pi", "dsh"} {
		if source, ok := HostModelSource(provider); ok || len(source.Models) != 0 {
			t.Fatalf("%s: ok = %v, models = %v", provider, ok, source.Models)
		}
	}
}

func TestApplyAgentCLILocalModelPointsDeepSeekHarnessAtTheHostModel(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	argv := []string{"dsh", "--profile", "headless", "--json", "task"}

	out, err := ApplyAgentCLILocalModel("dsh", "magpie/group/known", argv)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(out) != len(argv)+2 || out[3] != "--patch" {
		t.Fatalf("argv = %v", out)
	}
	raw, err := os.ReadFile(out[4])
	if err != nil {
		t.Fatalf("read patch: %v", err)
	}
	var patches []struct {
		ID     string `json:"id"`
		Config struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &patches); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if len(patches) != 1 || patches[0].ID != "agent-default-model" ||
		patches[0].Config.Provider != "magpie" || patches[0].Config.Model != "group/known" {
		t.Fatalf("patch = %+v", patches)
	}
}

func TestApplyAgentCLILocalModelLeavesOtherCLIsAlone(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	argv := []string{"pi", "--mode", "json", "task"}
	for _, kind := range []string{"pi", "opencode", "codex"} {
		out, err := ApplyAgentCLILocalModel(kind, "magpie/group/known", argv)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if len(out) != len(argv) {
			t.Fatalf("%s: argv = %v", kind, out)
		}
	}
}

func TestApplyAgentCLILocalModelIsOffOnThePlatform(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	argv := []string{"dsh", "--profile", "headless", "task"}
	out, err := ApplyAgentCLILocalModel("dsh", "magpie/group/known", argv)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(out) != len(argv) {
		t.Fatalf("argv = %v", out)
	}
}

func TestAgentCLIModelArgKeepsTheProviderOnlyInLocalMode(t *testing.T) {
	t.Setenv("AGENTFLOW_LOCAL_MODE", "1")
	if got := agentCLIModelArg("magpie/group/known"); got != "magpie/group/known" {
		t.Fatalf("local model arg = %q", got)
	}
	t.Setenv("AGENTFLOW_LOCAL_MODE", "")
	if got := agentCLIModelArg("group/known"); got != "agentflow/group/known" {
		t.Fatalf("platform model arg = %q", got)
	}
}
