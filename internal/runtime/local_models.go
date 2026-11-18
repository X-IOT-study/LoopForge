package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/pelletier/go-toml/v2"

	"agentflow/internal/config"
	"agentflow/internal/domain"
)

// LocalModelSource is what a CLI's own host configuration says about models:
// the Base URL to ask for a list, the credential to ask with, and the model
// names the configuration already names. The local edition lists these instead
// of the platform's built-in catalog, which only knows the vendor's public
// models and not the gateway the operator actually runs.
type LocalModelSource struct {
	BaseURL   string
	APIKey    string
	Anthropic bool
	Models    []domain.CLIModel
}

// HostModelSource reads provider's host configuration. It reports false for a
// provider whose configuration names no model list, and always in platform
// mode: there the platform's catalogs are what the chat lists.
func HostModelSource(provider string) (LocalModelSource, bool) {
	if !config.LocalMode() {
		return LocalModelSource{}, false
	}
	var source LocalModelSource
	switch localProviderKind(provider) {
	case "codex":
		source = codexHostModelSource()
	case "claude-code":
		source = claudeHostModelSource()
	case "opencode":
		source = opencodeHostModelSource()
	case "pi":
		source = piHostModelSource()
	case "dsh":
		source = dshHostModelSource()
	}
	if source.BaseURL == "" && len(source.Models) == 0 {
		return LocalModelSource{}, false
	}
	return source, true
}

// LocalModelNameOK guards a model ID that comes from a host gateway before it
// reaches a CLI argument or a config override.
func LocalModelNameOK(model string) bool {
	if model == "" || len(model) > 128 {
		return false
	}
	for _, r := range model {
		if r <= ' ' || r == 0x7f || r == '"' || r == '\'' || r == '`' || r == '\\' {
			return false
		}
	}
	return true
}

// codexHostConfig is the part of ~/.codex/config.toml local mode reads.
type codexHostConfig struct {
	Model          string                       `toml:"model"`
	ModelProvider  string                       `toml:"model_provider"`
	ModelCatalog   string                       `toml:"model_catalog_json"`
	ModelProviders map[string]codexHostProvider `toml:"model_providers"`
}

type codexHostProvider struct {
	Name                    string `toml:"name"`
	BaseURL                 string `toml:"base_url"`
	ExperimentalBearerToken string `toml:"experimental_bearer_token"`
	EnvKey                  string `toml:"env_key"`
}

// codexHostModelSource reads $CODEX_HOME/config.toml (default ~/.codex). The
// active model_providers entry is the gateway the CLI sends turns to, so its
// base_url is the list worth asking and its bearer is the credential it
// already uses — local mode never makes the operator re-enter either.
// codexHostModelSource calls readCodexHostConfig.
func codexHostModelSource() LocalModelSource {
	cfg, configDir, ok := readCodexHostConfig()
	if !ok {
		return LocalModelSource{}
	}
	var source LocalModelSource
	if provider, ok := cfg.ModelProviders[strings.TrimSpace(cfg.ModelProvider)]; ok {
		source.BaseURL = strings.TrimSpace(provider.BaseURL)
		source.APIKey = strings.TrimSpace(provider.ExperimentalBearerToken)
		if key := strings.TrimSpace(provider.EnvKey); key != "" && source.APIKey == "" {
			source.APIKey = strings.TrimSpace(os.Getenv(key))
		}
	}
	if source.APIKey == "" {
		source.APIKey = firstEnv("OPENAI_API_KEY", "CODEX_API_KEY")
	}
	models := []domain.CLIModel{{ID: strings.TrimSpace(cfg.Model)}}
	models = append(models, codexCatalogModels(cfg.ModelCatalog, configDir)...)
	source.Models = localModelList(models)
	return source
}

// readCodexHostConfig parses $CODEX_HOME/config.toml and reports the directory
// its relative paths are resolved against.
func readCodexHostConfig() (codexHostConfig, string, bool) {
	dir := hostConfigDir("CODEX_HOME", ".codex")
	if dir == "" {
		return codexHostConfig{}, "", false
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		return codexHostConfig{}, "", false
	}
	var cfg codexHostConfig
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return codexHostConfig{}, "", false
	}
	return cfg, dir, true
}

// HostModelCatalogEntry is one model the host gateway publishes, with the
// limits it publishes for it.
type HostModelCatalogEntry struct {
	ID              string
	Name            string
	ContextWindow   int
	MaxOutputTokens int
}

// WriteCodexModelCatalog writes a codex model catalog to target that covers
// entries the host's own catalog does not describe yet, and returns the path.
// Codex keeps model metadata itself and guesses a context window for a model it
// does not know, which a full turn overruns; model_catalog_json is the only
// place it reads that metadata, so the operator's catalog is extended rather
// than replaced. It returns "" when there is nothing to add or no host catalog
// to extend - codex then behaves exactly as it does outside the platform.
func WriteCodexModelCatalog(target string, entries []HostModelCatalogEntry) (string, error) {
	if !config.LocalMode() {
		return "", nil
	}
	cfg, configDir, ok := readCodexHostConfig()
	if !ok {
		return "", nil
	}
	path := strings.TrimSpace(cfg.ModelCatalog)
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(configDir, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	// Numbers go through json.Number so the operator's own values are copied
	// back out with the digits they were written with.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var catalog struct {
		Models []map[string]any `json:"models"`
	}
	if err := decoder.Decode(&catalog); err != nil {
		return "", err
	}
	if len(catalog.Models) == 0 {
		return "", nil
	}
	// Codex validates every field of an entry, so a new model is the first real
	// entry with only its identity (and the gateway's window) replaced.
	template := catalog.Models[0]
	known := make(map[string]bool, len(catalog.Models))
	for _, model := range catalog.Models {
		if slug, _ := model["slug"].(string); slug != "" {
			known[slug] = true
		}
	}
	added := false
	for _, entry := range entries {
		if !LocalModelNameOK(entry.ID) || known[entry.ID] {
			continue
		}
		model := make(map[string]any, len(template)+3)
		for key, value := range template {
			model[key] = value
		}
		model["slug"] = entry.ID
		model["display_name"] = firstNonEmpty(entry.Name, entry.ID)
		model["description"] = firstNonEmpty(entry.Name, entry.ID)
		if entry.ContextWindow > 0 {
			model["context_window"] = entry.ContextWindow
		}
		catalog.Models = append(catalog.Models, model)
		known[entry.ID] = true
		added = true
	}
	if !added {
		return "", nil
	}
	out, err := json.Marshal(catalog)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(target, out, 0o600); err != nil {
		return "", err
	}
	return target, nil
}

// codexCatalogModels reads the model_catalog_json file, which names models the
// gateway serves without asking it.
func codexCatalogModels(path, dir string) []domain.CLIModel {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var catalog struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return nil
	}
	models := make([]domain.CLIModel, 0, len(catalog.Models))
	for _, model := range catalog.Models {
		models = append(models, domain.CLIModel{ID: model.Slug, Name: model.DisplayName})
	}
	return models
}

// claudeHostSettings is the part of ~/.claude/settings.json local mode reads.
// The gateway lives in its env block, which is the host configuration the CLI
// itself starts with, so it is there even when the server process did not
// inherit the same variables.
type claudeHostSettings struct {
	Model       string `json:"model"`
	ModelPicker struct {
		Options []struct {
			Label string `json:"label"`
			Model string `json:"model"`
		} `json:"options"`
	} `json:"modelPicker"`
	Env map[string]string `json:"env"`
}

func claudeHostModelSource() LocalModelSource {
	dir := hostConfigDir("CLAUDE_CONFIG_DIR", ".claude")
	if dir == "" {
		return LocalModelSource{}
	}
	var settings claudeHostSettings
	if raw, err := os.ReadFile(filepath.Join(dir, "settings.json")); err == nil {
		_ = json.Unmarshal(raw, &settings)
	}
	source := LocalModelSource{
		BaseURL:   firstNonEmpty(settings.Env["ANTHROPIC_BASE_URL"], os.Getenv("ANTHROPIC_BASE_URL")),
		APIKey:    firstNonEmpty(settings.Env["ANTHROPIC_AUTH_TOKEN"], settings.Env["ANTHROPIC_API_KEY"], os.Getenv("ANTHROPIC_AUTH_TOKEN"), os.Getenv("ANTHROPIC_API_KEY")),
		Anthropic: true,
	}
	models := []domain.CLIModel{{ID: strings.TrimSpace(settings.Model)}}
	for _, option := range settings.ModelPicker.Options {
		models = append(models, domain.CLIModel{ID: option.Model, Name: option.Label})
	}
	// CLAUDE_CODE_MODEL_CAPABILITIES is "model=efforts;model=efforts": the keys
	// are the models this machine's gateway is known to serve.
	for _, entry := range strings.Split(settings.Env["CLAUDE_CODE_MODEL_CAPABILITIES"], ";") {
		id, _, _ := strings.Cut(strings.TrimSpace(entry), "=")
		models = append(models, domain.CLIModel{ID: id})
	}
	for _, name := range []string{
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_DEFAULT_FABLE_MODEL",
		"ANTHROPIC_REASONING_MODEL",
		"ANTHROPIC_SMALL_FAST_MODEL",
		"CLAUDE_CODE_AUTO_MODE_MODEL",
	} {
		models = append(models, domain.CLIModel{ID: firstNonEmpty(settings.Env[name], os.Getenv(name))})
	}
	source.Models = localModelList(models)
	return source
}

// hostConfigDir resolves a CLI's config directory: its own override first, then
// the operator's home.
func hostConfigDir(envName, rel string) string {
	if dir := strings.TrimSpace(os.Getenv(envName)); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, rel)
}

// hostXDGConfigFile resolves a CLI config file kept under the XDG config
// directory, which is where a CLI that follows the spec writes it.
func hostXDGConfigFile(envName, sub, file string) string {
	if path := strings.TrimSpace(os.Getenv(envName)); path != "" {
		return path
	}
	base := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, sub, file)
}

// opencodeHostConfig is the part of ~/.config/opencode/opencode.json local mode
// reads. Models are listed per provider and addressed as "provider/model",
// which is how the CLI names them on its own command line.
type opencodeHostConfig struct {
	Model     string                          `json:"model"`
	Providers map[string]opencodeHostProvider `json:"providers"`
}

type opencodeHostProvider struct {
	Models map[string]struct {
		Name string `json:"name"`
	} `json:"models"`
}

func opencodeHostModelSource() LocalModelSource {
	path := hostXDGConfigFile("OPENCODE_CONFIG", "opencode", "opencode.json")
	if path == "" {
		return LocalModelSource{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return LocalModelSource{}
	}
	var cfg opencodeHostConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return LocalModelSource{}
	}
	// The active provider is the one the config's default model names; that
	// model comes first so the CLI's default stays the first entry listed.
	active, activeModel, _ := strings.Cut(strings.TrimSpace(cfg.Model), "/")
	var models []domain.CLIModel
	for _, name := range orderedProviderNames(cfg.Providers, active) {
		provider := cfg.Providers[name]
		ids := make([]string, 0, len(provider.Models))
		for id := range provider.Models {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if name == active {
			ids = append([]string{activeModel}, ids...)
		}
		for _, id := range ids {
			model := provider.Models[id]
			models = append(models, domain.CLIModel{ID: name + "/" + id, Name: model.Name})
		}
	}
	return LocalModelSource{Models: localModelList(models)}
}

// orderedProviderNames lists the provider names with the active one first and
// the rest sorted, so the list is stable across reads.
func orderedProviderNames[T any](providers map[string]T, active string) []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		if name != active {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if _, ok := providers[active]; ok {
		return append([]string{active}, names...)
	}
	return names
}

// piHostConfig is the part of ~/.pi/agent/models.json local mode reads. Pi keeps
// the gateway and its models in one file and addresses a model by its own ID,
// which is what the CLI takes on the command line.
type piHostConfig struct {
	Providers map[string]struct {
		Models []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"models"`
	} `json:"providers"`
}

func piHostModelSource() LocalModelSource {
	dir := hostConfigDir("PI_CONFIG_DIR", ".pi/agent")
	if dir == "" {
		return LocalModelSource{}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		return LocalModelSource{}
	}
	var cfg piHostConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return LocalModelSource{}
	}
	active := piHostDefaultProvider(filepath.Join(dir, "settings.json"))
	var models []domain.CLIModel
	for _, name := range orderedProviderNames(cfg.Providers, active) {
		for _, model := range cfg.Providers[name].Models {
			models = append(models, domain.CLIModel{ID: name + "/" + model.ID, Name: model.Name})
		}
	}
	return LocalModelSource{Models: localModelList(models)}
}

// piHostDefaultProvider reads the provider pi starts in, so its models are the
// first ones offered.
func piHostDefaultProvider(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var settings struct {
		DefaultProvider string `json:"defaultProvider"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return ""
	}
	return strings.TrimSpace(settings.DefaultProvider)
}

// dshHostDefaultModel reads the model DeepSeek Harness starts with, so it is
// the first one offered. Both fields are needed: the model ID alone does not
// say which provider it belongs to.
func dshHostDefaultModel(path string) (string, string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	var cfg struct {
		Patches []struct {
			ID     string `json:"id"`
			Config struct {
				Provider string `json:"provider"`
				Model    string `json:"model"`
			} `json:"config"`
		} `json:"patches"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return "", ""
	}
	for _, patch := range cfg.Patches {
		if patch.ID == "agent-default-model" {
			return patch.Config.Provider, patch.Config.Model
		}
	}
	return "", ""
}

// dshHostConfig is the part of ~/.dsh/settings.yaml local mode reads: DeepSeek
// Harness keeps its gateways and their models in one YAML document.
type dshHostConfig struct {
	Harness struct {
		Providers map[string]struct {
			Models []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"models"`
		} `json:"providers"`
	} `json:"llm-pi-ai"`
}

func dshHostModelSource() LocalModelSource {
	dir := hostConfigDir("DSH_HOME", ".dsh")
	if dir == "" {
		return LocalModelSource{}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "settings.yaml"))
	if err != nil {
		return LocalModelSource{}
	}
	var cfg dshHostConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return LocalModelSource{}
	}
	active, _ := dshHostDefaultModel(filepath.Join(dir, "settings.yaml"))
	var models []domain.CLIModel
	for _, name := range orderedProviderNames(cfg.Harness.Providers, active) {
		for _, model := range cfg.Harness.Providers[name].Models {
			models = append(models, domain.CLIModel{ID: name + "/" + model.ID, Name: model.Name})
		}
	}
	return LocalModelSource{Models: localModelList(models)}
}

// localModelList keeps the distinct, usable entries of a host model list in the
// order the configuration named them: the first entry is the configured
// default, so order carries meaning.
func localModelList(models []domain.CLIModel) []domain.CLIModel {
	seen := make(map[string]bool, len(models))
	out := make([]domain.CLIModel, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if !LocalModelNameOK(id) || seen[id] {
			continue
		}
		seen[id] = true
		model.ID = id
		model.Name = strings.TrimSpace(model.Name)
		out = append(out, model)
	}
	return out
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}
