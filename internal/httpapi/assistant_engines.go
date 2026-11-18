package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
)

// AI 对话 runs on CodeBuddy by default. A model ID prefixed with another CLI
// ("codex:", "claude-code:" or an agent CLI kind such as "opencode:") runs the
// turn on that CLI with the caller's own key and Base URL from 个人配置; the
// part after the prefix is passed to the CLI as-is.
const (
	assistantEngineCodeBuddy = "codebuddy"
	assistantEngineCodex     = "codex"
	assistantEngineClaude    = "claude-code"
)

// assistantCLIEngines are the CLIs other than CodeBuddy, in switch order. The
// local-mode filter applies here so every caller (engine switch, model
// prefixes, model probes) sees the same set; platform mode keeps all of them.
func assistantCLIEngines() []string {
	engines := append([]string{assistantEngineCodex, assistantEngineClaude}, runtime.AgentCLIKinds...)
	if !config.LocalMode() {
		return engines
	}
	kept := make([]string, 0, len(engines))
	for _, engine := range engines {
		if config.LocalModeSupportsExecutor(engine) {
			kept = append(kept, engine)
		}
	}
	return kept
}

func assistantModelEngine(id string) (engine, model string) {
	id = strings.TrimSpace(id)
	for _, engine := range assistantCLIEngines() {
		if rest, ok := strings.CutPrefix(id, engine+":"); ok {
			return engine, strings.TrimSpace(rest)
		}
	}
	return assistantEngineCodeBuddy, id
}

// assistantHostModel empties a CLI engine's placeholder model in local mode so
// the CLI falls back to the model its own host configuration names. "default"
// is the catalog's "CLI 默认模型" entry, not a model any provider serves, so
// passing it through makes the CLI reject the turn. CodeBuddy keeps its model:
// it is part of the LoopForge-managed configuration, not the host's.
func assistantHostModel(engine, model string) string {
	model = strings.TrimSpace(model)
	if !config.LocalMode() || engine == assistantEngineCodeBuddy {
		return model
	}
	if model == "default" {
		return ""
	}
	return model
}

// assistantEngineOption is one entry of the chat composer's CLI switch.
// Credential names the 个人配置 field that enables it.
type assistantEngineOption struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
	Credential string `json:"credential"`
}

// assistantEngineCredentialField is the 个人配置 field that enables a CLI.
func assistantEngineCredentialField(engine string) string {
	switch engine {
	case assistantEngineCodeBuddy:
		return "codeBuddyApiKey"
	case assistantEngineCodex:
		return "codexApiKey"
	case assistantEngineClaude:
		return "claudeCodeApiKey"
	}
	return "agentCli." + engine
}

// assistantEngineOptions lists every CLI, configured or not, so the switch
// also shows where to add a key. CodeBuddy counts as configured when the
// platform sponsors chats without one.
func assistantEngineOptions(user domain.User, sponsored bool) []assistantEngineOption {
	engines := append([]string{assistantEngineCodeBuddy}, assistantCLIEngines()...)
	out := make([]assistantEngineOption, 0, len(engines))
	for _, engine := range engines {
		key, _ := assistantEngineCredential(user, engine)
		// Local mode runs on the machine's own CLI logins, so a personal key
		// is never what enables an engine — the host login is. Reporting the
		// host state keeps the switch honest: a CLI the operator never logged
		// into stays unconfigured instead of failing on the first turn.
		configured := key != "" || (engine == assistantEngineCodeBuddy && sponsored)
		if config.LocalMode() {
			configured = assistantHostLoginAvailable(engine)
		}
		out = append(out, assistantEngineOption{
			ID:         engine,
			Name:       assistantEngineLabel(engine),
			Configured: configured,
			Credential: assistantEngineCredentialField(engine),
		})
	}
	return out
}

// assistantHostLoginAvailable reports whether a CLI can run on the operator's
// own login in local mode. Engines whose login cannot be probed stay
// available: a false negative would hide a working CLI from the switch.
func assistantHostLoginAvailable(engine string) bool {
	if runtime.LocalCredentialStrength(engine) != runtime.LocalCredentialConfirmed {
		return true
	}
	return runtime.LocalCLILoginAvailable(engine)
}

func assistantEngineLabel(engine string) string {
	switch engine {
	case assistantEngineCodex:
		return "Codex"
	case assistantEngineClaude:
		return "Claude Code"
	case assistantEngineCodeBuddy, "":
		return "CodeBuddy"
	}
	return runtime.AgentCLILabel(engine)
}

func assistantEngineCredential(user domain.User, engine string) (apiKey, baseURL string) {
	switch engine {
	case assistantEngineCodex:
		return strings.TrimSpace(user.CodexAPIKey), strings.TrimSpace(user.CodexBaseURL)
	case assistantEngineClaude:
		return strings.TrimSpace(user.ClaudeCodeAPIKey), strings.TrimSpace(user.ClaudeCodeBaseURL)
	case assistantEngineCodeBuddy:
		return strings.TrimSpace(user.CodeBuddyAPIKey), ""
	}
	cred := user.AgentCLICredentials[engine]
	if runtime.IsAgentCLI(engine) && engine != runtime.AgentCLICursor {
		baseURL = strings.TrimSpace(cred.BaseURL)
	}
	return strings.TrimSpace(cred.APIKey), baseURL
}

// assistantEngineProtocol is the API protocol an agent CLI speaks; "" for
// the other CLIs.
func assistantEngineProtocol(user domain.User, engine string) string {
	if !runtime.IsAgentCLI(engine) {
		return ""
	}
	return runtime.AgentCLIProtocol(engine, user.AgentCLICredentials[engine].Protocol)
}

// assistantEngineAnthropicModels reports whether the model list behind a
// Base URL is the Anthropic one ({base}/v1/models, x-api-key).
func assistantEngineAnthropicModels(engine, protocol string) bool {
	return engine == assistantEngineClaude || protocol == runtime.AgentCLIProtocolMessages
}

func assistantEngineCustomModels(user domain.User, engine string) []domain.CLIModel {
	switch engine {
	case assistantEngineCodex:
		return user.CodexModels
	case assistantEngineClaude:
		return user.ClaudeCodeModels
	}
	return user.AgentCLICredentials[engine].Models
}

// assistantEngineBuiltinModels is the executor catalog for a CLI, used on the
// official endpoint (no Base URL) or when the Base URL lists no model.
func assistantEngineBuiltinModels(engine string) []assistantDiscoveredModel {
	catalogKey := engine
	if engine == assistantEngineClaude {
		catalogKey = "claude"
	}
	var out []assistantDiscoveredModel
	for _, model := range executorModelCatalogs()[catalogKey].Models {
		out = append(out, assistantDiscoveredModel{ID: model.ID, Name: model.Name})
	}
	return out
}

// assistantValidEngineModel guards model IDs that reach a CLI argument or a
// TOML config override.
func assistantValidEngineModel(model string) bool {
	return runtime.LocalModelNameOK(model)
}

// firstPositive returns the first value above zero, 0 when none is.
func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

// assistantHostEngineModels lists the models the machine's own CLI
// configuration points at: the gateway its Base URL names, or the models its
// config already spells out. Local mode only — without a Base URL of the
// operator's own the CLI runs on this configuration, so it, not the built-in
// catalog, says which models exist.
func assistantHostEngineModels(ctx context.Context, engine string) []assistantDiscoveredModel {
	host, ok := runtime.HostModelSource(engine)
	if !ok {
		return nil
	}
	if host.BaseURL != "" {
		if listed := cachedAssistantEngineDiscovery(ctx, host.Anthropic, host.APIKey, host.BaseURL); len(listed) > 0 {
			return listed
		}
	}
	// The gateway is unreachable or publishes no list: the names the config
	// gives are still the ones the CLI can run.
	out := make([]assistantDiscoveredModel, 0, len(host.Models))
	for _, model := range host.Models {
		out = append(out, assistantDiscoveredModel{ID: model.ID, Name: model.Name, Efforts: model.Efforts, DefaultEffort: model.DefaultEffort})
	}
	return out
}

// assistantConversationEngine is the CLI that wrote the conversation's latest
// own reply, "" before the first one. Role replies (SpeakerAgentID) run on
// their own executors and do not count. A turn that failed before producing
// anything leaves no reply, so it does not pin the CLI.
//
// Replies saved before model prefixes carry the CLI on the session Executor
// (legacyExecutor) instead. An unprefixed model is not itself proof of CodeBuddy.
func assistantConversationEngine(messages []assistantMessage, legacyExecutor ...string) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != "assistant" || m.SpeakerAgentID != "" || strings.TrimSpace(m.Model) == "" {
			continue
		}
		if assistantModelHasEnginePrefix(m.Model) {
			engine, _ := assistantModelEngine(m.Model)
			return engine
		}
		if legacy := assistantLegacyExecutor(legacyExecutor...); legacy != "" && legacy != assistantEngineCodeBuddy {
			return legacy
		}
		engine, _ := assistantModelEngine(m.Model)
		return engine
	}
	return ""
}

func assistantModelHasEnginePrefix(id string) bool {
	id = strings.TrimSpace(id)
	for _, engine := range append([]string{assistantEngineCodeBuddy}, assistantCLIEngines()...) {
		if _, ok := strings.CutPrefix(id, engine+":"); ok {
			return true
		}
	}
	return false
}

// assistantLegacyExecutor maps a session Executor saved before model prefixes.
// CodeBuddy and an empty value stay unset so an unprefixed model remains CodeBuddy.
func assistantLegacyExecutor(candidates ...string) string {
	for _, raw := range candidates {
		switch id := strings.ToLower(strings.TrimSpace(raw)); id {
		case assistantEngineCodex:
			return assistantEngineCodex
		case "claude", assistantEngineClaude:
			return assistantEngineClaude
		case "", assistantEngineCodeBuddy:
			continue
		default:
			for _, kind := range domain.AgentCLIExecutorKinds {
				if strings.EqualFold(kind, id) {
					return kind
				}
			}
		}
	}
	return ""
}

// assistantEngineSwitchRefusal keeps a conversation on one CLI. Its native
// session holds tool calls and reasoning that no other CLI can resume, so a
// switch would silently drop them; a new conversation is the only way.
func assistantEngineSwitchRefusal(locked, model string) string {
	engine, _ := assistantModelEngine(model)
	if locked == "" || locked == engine {
		return ""
	}
	return fmt.Sprintf("这个对话由 %s 回复，不能中途切换 CLI；请新建对话后使用 %s", assistantEngineLabel(locked), assistantEngineLabel(engine))
}

func assistantEngineMissingKey(engine string) string {
	return fmt.Sprintf("请先在个人配置中配置 %s API Key 后再使用 %s 模型", assistantEngineLabel(engine), assistantEngineLabel(engine))
}

// assistantTurnUser resolves the identity and model of one chat turn. Codex
// and Claude Code models need the caller's own key for that CLI and are never
// sponsored; CodeBuddy models keep the sponsor fallback. The returned user
// still carries a lent CodeBuddy key when there is one, so approval reviews
// keep working on Codex / Claude Code turns.
func (h *assistantHandlers) assistantTurnUser(stored domain.User, model string) (domain.User, string, string) {
	engine, _ := assistantModelEngine(model)
	if engine != assistantEngineCodeBuddy {
		// Local mode has no personal keys: the CLI runs with the machine's
		// own login, so the missing-key gate does not apply.
		if key, _ := assistantEngineCredential(stored, engine); key == "" && !config.LocalMode() {
			return stored, "", assistantEngineMissingKey(engine)
		}
		user, _, _ := h.assistantChatUser(stored)
		return user, model, ""
	}
	user, shared, ok := h.assistantChatUser(stored)
	if !ok {
		// Local mode has no key to demand: the turn runs on the machine's own
		// CLI login, so the caller's own record is what it runs as.
		if config.LocalMode() {
			return stored, model, ""
		}
		return stored, "", "请先在个人配置中配置自己的 CodeBuddy API Key"
	}
	if len(shared) > 0 {
		forced, allowed := assistantSponsoredModelFor(shared, model)
		if !allowed {
			return stored, "", errAssistantSponsoredModel(shared)
		}
		model = forced
	}
	return user, model, ""
}

// assistantAllowedUserModel is assistantAllowedModel plus the caller's CLI
// models. Any well-formed model ID is accepted for a configured CLI: the
// gateway behind the Base URL decides what exists, and a discovery outage
// must not silently move the conversation back to CodeBuddy.
func assistantAllowedUserModel(id string, catalog assistantModelCatalog, user domain.User) string {
	engine, model := assistantModelEngine(id)
	if engine == assistantEngineCodeBuddy {
		return assistantAllowedModel(id, catalog)
	}
	// Local mode runs on the machine's own CLI login, so a well-formed CLI
	// model needs no personal key there.
	if key, _ := assistantEngineCredential(user, engine); (key == "" && !config.LocalMode()) || !assistantValidEngineModel(model) {
		return catalog.Default
	}
	return engine + ":" + model
}

// assistantLocalEngineModel rewrites a stored scheduled-task model to a CLI
// this machine can actually run. A task keeps the engine it was saved with, so
// the local edition must not fail a run over a CLI the operator never logged
// into: it falls back to the first engine whose host login works.
func assistantLocalEngineModel(model string) string {
	engine, _ := assistantModelEngine(model)
	if assistantHostLoginAvailable(engine) {
		return model
	}
	for _, candidate := range assistantCLIEngines() {
		if assistantHostLoginAvailable(candidate) {
			return candidate + ":default"
		}
	}
	return model
}

// assistantScheduledModel normalizes the model of an AI scheduled task on
// save. The run uses the project owner's keys and CodeBuddy environment, not
// the editor's, so a well-formed model is kept as-is; it is checked against
// the owner's account when it runs (see scheduledRunModel).
func assistantScheduledModel(id string, catalog assistantModelCatalog) string {
	engine, model := assistantModelEngine(id)
	if engine != assistantEngineCodeBuddy && assistantValidEngineModel(model) {
		return engine + ":" + model
	}
	if engine == assistantEngineCodeBuddy && assistantValidEngineModel(model) {
		return model
	}
	return assistantAllowedModel(id, catalog)
}

// assistantEngineModels lists the models a configured CLI offers in chat.
// A personal catalog wins on the official endpoint and behind a Base URL.
// An empty catalog asks the Base URL for its published models, then falls
// back to the built-in executor catalog. Gemini publishes no OpenAI-style
// list, so an empty catalog there uses the built-in models directly.
func assistantEngineModels(ctx context.Context, user domain.User) []assistantModel {
	var out []assistantModel
	for _, engine := range assistantCLIEngines() {
		key, baseURL := assistantEngineCredential(user, engine)
		// Local mode lists the built-in CLI models even without a key: the
		// engine is configured by the machine's own login.
		if key == "" && !config.LocalMode() {
			continue
		}
		protocol := assistantEngineProtocol(user, engine)
		if protocol == runtime.AgentCLIProtocolGemini {
			baseURL = ""
		}
		var listed []assistantDiscoveredModel
		for _, model := range assistantEngineCustomModels(user, engine) {
			if assistantValidEngineModel(model.ID) {
				listed = append(listed, assistantDiscoveredModel{ID: model.ID, Name: model.Name, Efforts: model.Efforts, DefaultEffort: model.DefaultEffort})
			}
		}
		if len(listed) == 0 && baseURL != "" {
			listed = cachedAssistantEngineDiscovery(ctx, assistantEngineAnthropicModels(engine, protocol), key, baseURL)
		}
		if len(listed) == 0 && baseURL == "" {
			listed = assistantHostEngineModels(ctx, engine)
		}
		// Cursor's list comes from the CLI; the saved one (the last list
		// saveCursorCatalog kept) answers until a fresh one is cached.
		if engine == runtime.AgentCLICursor {
			if fresh := cachedCursorModels(key); len(fresh) > 0 {
				listed = fresh
			}
		}
		if len(listed) == 0 {
			listed = assistantEngineBuiltinModels(engine)
		}
		catalog := cliModelsOf(listed)
		for _, model := range listed {
			name := strings.TrimSpace(model.Name)
			if name == "" {
				name = model.ID
			}
			id := engine + ":" + model.ID
			out = append(out, assistantModel{
				ID:      id,
				Name:    name,
				Family:  assistantModelFamily(model.ID),
				Engine:  engine,
				Efforts: assistantModelEffortsIn(id, catalog),
			})
		}
	}
	return out
}

type assistantDiscoveredModel struct {
	ID   string
	Name string
	// Efforts and DefaultEffort describe a Cursor family folded out of the
	// CLI's list (domain.CLIModel).
	Efforts       map[string]string
	DefaultEffort string
	// ContextWindow and MaxOutputTokens are what the gateway publishes for the
	// model. Codex sizes a thread from model metadata it keeps itself, which
	// says nothing about a gateway model, so a turn it runs needs these.
	ContextWindow   int
	MaxOutputTokens int
}

type assistantDiscoveryEntry struct {
	models  []assistantDiscoveredModel
	expires time.Time
}

const (
	assistantDiscoveryTTL        = 10 * time.Minute
	assistantDiscoveryFailureTTL = time.Minute
	assistantDiscoveryTimeout    = 4 * time.Second
	assistantDiscoveryMaxModels  = 100
)

var (
	assistantDiscoveryCache sync.Map // sha256(engine, baseURL, key) -> assistantDiscoveryEntry
	// assistantEngineDiscover is a variable so tests can stub the network.
	assistantEngineDiscover = discoverAssistantEngineModels
)

func cachedAssistantEngineDiscovery(ctx context.Context, anthropic bool, apiKey, baseURL string) []assistantDiscoveredModel {
	sum := sha256.Sum256([]byte(fmt.Sprint(anthropic) + "\x00" + baseURL + "\x00" + apiKey))
	cacheKey := hex.EncodeToString(sum[:])
	now := time.Now()
	if cached, ok := assistantDiscoveryCache.Load(cacheKey); ok {
		if entry := cached.(assistantDiscoveryEntry); now.Before(entry.expires) {
			return entry.models
		}
	}
	ctx, cancel := context.WithTimeout(ctx, assistantDiscoveryTimeout)
	defer cancel()
	models, err := assistantEngineDiscover(ctx, anthropic, apiKey, baseURL)
	ttl := assistantDiscoveryTTL
	if err != nil || len(models) == 0 {
		models, ttl = nil, assistantDiscoveryFailureTTL
	}
	assistantDiscoveryCache.Store(cacheKey, assistantDiscoveryEntry{models: models, expires: now.Add(ttl)})
	return models
}

var (
	assistantCursorModelsCache    sync.Map // sha256(key) -> assistantDiscoveryEntry
	assistantCursorModelsInflight sync.Map
	// assistantCursorModels is a variable so tests can stub cursor-agent.
	assistantCursorModels = cursorAgentModels
)

// cachedCursorModels lists the models the key's Cursor plan offers. The CLI
// takes seconds, so a miss answers with the built-in catalog (nil) while a
// background refresh fills the cache; a stale list is served meanwhile.
func cachedCursorModels(apiKey string) []assistantDiscoveredModel {
	sum := sha256.Sum256([]byte("cursor\x00" + apiKey))
	cacheKey := hex.EncodeToString(sum[:])
	var stale []assistantDiscoveredModel
	if cached, ok := assistantCursorModelsCache.Load(cacheKey); ok {
		entry := cached.(assistantDiscoveryEntry)
		if time.Now().Before(entry.expires) {
			return entry.models
		}
		stale = entry.models
	}
	if _, running := assistantCursorModelsInflight.LoadOrStore(cacheKey, true); !running {
		go func() {
			defer assistantCursorModelsInflight.Delete(cacheKey)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			found, err := assistantCursorModels(ctx, apiKey)
			var models []assistantDiscoveredModel
			for _, model := range found {
				if assistantValidEngineModel(model.ID) && len(models) < assistantDiscoveryMaxModels {
					models = append(models, assistantDiscoveredModel{ID: model.ID, Name: model.Name, Efforts: model.Efforts, DefaultEffort: model.DefaultEffort})
				}
			}
			ttl := assistantDiscoveryTTL
			if err != nil || len(models) == 0 {
				models, ttl = stale, assistantDiscoveryFailureTTL
			}
			assistantCursorModelsCache.Store(cacheKey, assistantDiscoveryEntry{models: models, expires: time.Now().Add(ttl)})
		}()
	}
	return stale
}

// saveCursorCatalog keeps the user's freshly listed Cursor models, folded
// families with their effort→ID mapping included, in their Cursor
// credential. Effort checks and the --model of chat turns and SOP nodes
// resolve from that saved list, so a family keeps working on every instance
// and after a restart, and each user resolves against their own CLI's list.
func (h *assistantHandlers) saveCursorCatalog(user domain.User) {
	if h == nil || h.repo == nil || user.ID <= 0 {
		return
	}
	key, _ := assistantEngineCredential(user, runtime.AgentCLICursor)
	if key == "" {
		return
	}
	fresh := cachedCursorModels(key)
	if len(fresh) == 0 {
		return
	}
	models := cliModelsOf(fresh)
	if cursorCatalogEqual(models, assistantCursorCatalog(user)) {
		return
	}
	if _, err := h.repo.UpdateUserCredentials(user.ID, repo.UserCredentialUpdate{
		AgentCLICredentials: map[string]repo.AgentCLICredentialUpdate{runtime.AgentCLICursor: {Models: &models}},
	}); err != nil {
		log.Printf("cursor model catalog save skipped user=%d: %v", user.ID, err)
	}
}

func cursorCatalogEqual(a, b []domain.CLIModel) bool {
	return slices.EqualFunc(a, b, func(x, y domain.CLIModel) bool {
		return x.ID == y.ID && (x.Name == y.Name || x.Name == x.ID && y.Name == "" || y.Name == y.ID && x.Name == "") &&
			x.DefaultEffort == y.DefaultEffort && maps.Equal(x.Efforts, y.Efforts)
	})
}

// discoverAssistantEngineModels reads the OpenAI-style model list behind a
// Base URL: {base}/models for OpenAI-style Base URLs (which already end in
// /v1) and {base}/v1/models for Anthropic ones. Gateways answer both with
// {"data":[{"id":…,"display_name"|"name":…}]}.
func discoverAssistantEngineModels(ctx context.Context, anthropic bool, apiKey, baseURL string) ([]assistantDiscoveredModel, error) {
	endpoint := strings.TrimRight(baseURL, "/")
	if anthropic {
		endpoint += "/v1/models"
	} else {
		endpoint += "/models"
	}
	if !strings.HasPrefix(endpoint, "https://") && !strings.HasPrefix(endpoint, "http://") {
		return nil, fmt.Errorf("Base URL 必须以 http:// 或 https:// 开头")
	}
	schemes := []string{"bearer"}
	if anthropic {
		schemes = append(schemes, "x-api-key")
	}
	var lastErr error
	for _, scheme := range schemes {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		if scheme == "bearer" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		} else {
			req.Header.Set("x-api-key", apiKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			var urlErr *url.Error
			if errors.As(err, &urlErr) {
				err = urlErr.Err
			}
			return nil, fmt.Errorf("无法连接 %s：%w", endpoint, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			lastErr = fmt.Errorf("%s 拒绝了 API Key（HTTP %d）", endpoint, resp.StatusCode)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s 返回 HTTP %d", endpoint, resp.StatusCode)
		}
		var payload struct {
			Data []struct {
				ID              string `json:"id"`
				Name            string `json:"name"`
				DisplayName     string `json:"display_name"`
				ContextLength   int    `json:"context_length"`
				ContextWindow   int    `json:"context_window"`
				MaxOutputTokens int    `json:"max_output_tokens"`
				MaxOutputLength int    `json:"max_output_length"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("%s 返回的不是模型列表", endpoint)
		}
		var out []assistantDiscoveredModel
		seen := map[string]bool{}
		for _, item := range payload.Data {
			id := strings.TrimSpace(item.ID)
			if !assistantValidEngineModel(id) || seen[id] {
				continue
			}
			seen[id] = true
			name := strings.TrimSpace(item.DisplayName)
			if name == "" {
				name = strings.TrimSpace(item.Name)
			}
			out = append(out, assistantDiscoveredModel{
				ID:              id,
				Name:            truncateRunes(name, 64),
				ContextWindow:   firstPositive(item.ContextLength, item.ContextWindow),
				MaxOutputTokens: firstPositive(item.MaxOutputTokens, item.MaxOutputLength),
			})
			if len(out) == assistantDiscoveryMaxModels {
				break
			}
		}
		return out, nil
	}
	return nil, lastErr
}

// assistantEngineState records which CLI holds a personal conversation's
// native transcript. A turn on another CLI cannot resume it: it starts a new
// CLI session from the full transcript, so switching back never continues a
// transcript that misses the turns in between.
type assistantEngineState struct {
	// Edited history starts a new journal once; errors resume that revision.
	HistoryRevision int    `json:"historyRevision,omitempty"`
	Engine          string `json:"engine"`
	SessionID       string `json:"sessionId"`
	Unstarted       bool   `json:"unstarted,omitempty"`
}

func readAssistantEngineState(root string) assistantEngineState {
	state := assistantEngineState{Engine: assistantEngineCodeBuddy}
	data, err := os.ReadFile(filepath.Join(root, "engine.json"))
	if err != nil || json.Unmarshal(data, &state) != nil || state.Engine == "" {
		return assistantEngineState{Engine: assistantEngineCodeBuddy}
	}
	return state
}

func writeAssistantEngineState(root string, state assistantEngineState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "engine.json"), data, 0o600)
}

// assistantRequestModel maps a legacy executor field onto the model prefix
// the conversation engines use. A model that already names its CLI is kept.
func assistantRequestModel(executor, model string) string {
	model = strings.TrimSpace(model)
	if engine, _ := assistantModelEngine(model); engine != assistantEngineCodeBuddy {
		return model
	}
	switch assistantExecutor(executor) {
	case assistantEngineCodex:
		return assistantEngineCodex + ":" + model
	case "claude", assistantEngineClaude:
		return assistantEngineClaude + ":" + model
	default:
		if runtime.IsAgentCLI(executor) {
			return strings.TrimSpace(executor) + ":" + model
		}
	}
	return model
}
