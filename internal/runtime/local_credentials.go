package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agentflow/internal/config"
	"agentflow/internal/domain"
)

// Local mode credential policy. A local-mode machine already has the user's own
// Agent CLI logins, so the execution side prefers them and only falls back to a
// personal key. Everything here observes the host; nothing writes to it.

const (
	// LocalCredentialConfirmed providers expose credential-class environment
	// variables (and, for codex/claude-code, a documented login file), so a
	// missing value is real evidence that the host is not logged in.
	LocalCredentialConfirmed = "confirmed"
	// LocalCredentialUnknown providers keep their login state only inside an
	// interactive host directory. A missing key is not evidence of a missing
	// login, so local mode must not fail the invocation. Every provider without
	// a confirmed table entry is unknown: there is no third class.
	LocalCredentialUnknown = "unknown"
)

const (
	// localCredentialMissingCode prefixes the readable local-mode credential
	// error so the panel and logs can match it.
	localCredentialMissingCode = "local_credentials_missing"

	// LocalSandboxUnsupportedCode is the machine-readable failure of a local-mode
	// node that asks for the container (docker) driver. A container has its own
	// HOME and no host CLI login state, so local mode's "use this machine's own
	// login" contract cannot hold inside it; failing closed with this code beats
	// letting the CLI report an authentication error no user can act on.
	LocalSandboxUnsupportedCode = "local_sandbox_unsupported"
)

// localProviderAliases maps provider spellings onto the canonical kind.
var localProviderAliases = map[string]string{
	"claude":       "claude-code",
	"claude-code":  "claude-code",
	"cursor-agent": "cursor",
	"cursor-cli":   "cursor",
	"antigravity":  "agy",
	"deepseek":     "dsh",
	"kimi-code":    "kimi",
	"deep-seek":    "dsh",
	"gemini":       "agy",
	"open-code":    "opencode",
	"z-code":       "zcode",
}

// localCredentialEnvs lists the credential-class environment variables of a
// confirmed provider. Locator variables (OPENCODE_CONFIG*, CODEX_HOME,
// CLAUDE_CONFIG_DIR, KIMI_CODE_HOME, DSH_HOME, PI_CODING_AGENT_DIR,
// ZCODE_DATA_BASE_DIR) and platform-injected values (AGENTFLOW_CLI_API_KEY and
// its KIMI_MODEL_API_KEY derivative) are deliberately absent: they select or
// carry platform configuration, never the host's own login.
var localCredentialEnvs = map[string][]string{
	"codebuddy":   {"CODEBUDDY_API_KEY"},
	"codex":       {"OPENAI_API_KEY", "CODEX_API_KEY"},
	"claude-code": {"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"},
	"cursor":      {"CURSOR_API_KEY", "CURSOR_AUTH_TOKEN"},
	"agy":         {"GEMINI_API_KEY", "GOOGLE_API_KEY", "AGY_LLM_GATEWAY_API_KEY"},
	"kimi":        {"KIMI_API_KEY", "MOONSHOT_API_KEY"},
	"dsh":         {"DEEPSEEK_API_KEY"},
}

// localCredentialFiles lists the host login files of a confirmed provider,
// relative to the host HOME. They are external facts: only existence and a
// non-empty size are checked, and an unreadable or empty file simply means the
// probe falls back to the environment. Measured on a logged-in host:
// ~/.codex/auth.json holds the CLI's OPENAI_API_KEY. ~/.claude/.credentials.json
// had no anchor in this repository and could not be measured, so claude-code is
// confirmed by environment only (tech-design §4.1).
var localCredentialFiles = map[string][]string{
	"codex": {".codex/auth.json"},
}

// localProviderLoginCommand is the terminal command that logs the host CLI in,
// when the CLI documents one.
var localProviderLoginCommand = map[string]string{
	"codex":       "codex login",
	"claude-code": "claude login",
}

// localProviderKind canonicalizes a provider name for the local-mode tables.
func localProviderKind(provider string) string {
	kind := strings.ToLower(strings.TrimSpace(provider))
	if canonical, ok := localProviderAliases[kind]; ok {
		return canonical
	}
	return kind
}

// LocalCredentialStrength reports how provider's host login can be observed:
// LocalCredentialConfirmed or LocalCredentialUnknown. A provider without a
// confirmed table entry is unknown, never "none".
func LocalCredentialStrength(provider string) string {
	switch localProviderKind(provider) {
	case "codebuddy", "codex", "claude-code", "cursor", "agy", "kimi", "dsh":
		return LocalCredentialConfirmed
	}
	return LocalCredentialUnknown
}

// LocalCLILoginAvailable reports whether the host is logged in to provider.
// Only confirmed providers answer true; unknown providers always defer to the
// host directory the CLI reads itself. The probe never fails the caller: a
// missing, empty or unreadable credential file is just "not observed".
func LocalCLILoginAvailable(provider string) bool {
	kind := localProviderKind(provider)
	if LocalCredentialStrength(kind) != LocalCredentialConfirmed {
		return false
	}
	for _, name := range localCredentialEnvs[kind] {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return true
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	for _, rel := range localCredentialFiles[kind] {
		if localCredentialFilePresent(filepath.Join(home, filepath.FromSlash(rel))) {
			return true
		}
	}
	return false
}

// localCredentialFilePresent reports whether path is a non-empty regular file.
func localCredentialFilePresent(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() <= 0 {
		return false
	}
	return true
}

// localProviderLabel is the human-readable provider name used in local-mode
// messages.
func localProviderLabel(kind string) string {
	switch kind {
	case "codebuddy":
		return "CodeBuddy"
	case "codex":
		return "Codex"
	case "claude-code":
		return "Claude Code"
	}
	if label := AgentCLILabel(kind); label != kind {
		return label
	}
	return kind
}

// LocalCredentialHint is the readable local-mode error for a failed node: the
// host has no login state and no personal key is configured.
func LocalCredentialHint(provider string) error {
	kind := localProviderKind(provider)
	label := localProviderLabel(kind)
	if LocalCredentialStrength(kind) == LocalCredentialUnknown {
		return fmt.Errorf("%s: 未检测到宿主 %s 登录态且未配置 API Key，请在「个人配置」填写 %s 的 Key",
			localCredentialMissingCode, label, label)
	}
	steps := ""
	if command := localProviderLoginCommand[kind]; command != "" {
		steps += fmt.Sprintf("请先在终端执行 `%s`，或", command)
	}
	if names := localCredentialEnvs[kind]; len(names) > 0 {
		steps += fmt.Sprintf("配置 `%s`，或", names[0])
	}
	return fmt.Errorf("%s: 未检测到宿主 %s 登录态，%s在「个人配置」填写 Key",
		localCredentialMissingCode, label, steps)
}

// LocalCredentialEnvironments returns the confirmed credential-class
// environment variables of provider that the host currently sets. Local mode
// passes them through to the child instead of clearing them.
func LocalCredentialEnvironments(provider string) map[string]string {
	kind := localProviderKind(provider)
	values := map[string]string{}
	for _, name := range localCredentialEnvs[kind] {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			values[name] = value
		}
	}
	return values
}

// LocalSandboxUnsupported reports whether local mode must reject driver. Only
// the container driver is rejected: it carries no host login state, while the
// host-process drivers (bwrap / seatbelt / local) bind the host CLI state the
// local-mode credential policy depends on.
func LocalSandboxUnsupported(driver string) bool {
	return config.LocalMode() && strings.EqualFold(strings.TrimSpace(driver), domain.SandboxDriverDocker)
}

// LocalSandboxUnsupportedError is the readable local-mode failure for a node
// that requests the container driver.
func LocalSandboxUnsupportedError(driver string) error {
	return fmt.Errorf("%s: 本地模式不支持 %s 沙箱，请改用本机沙箱（bwrap / seatbelt / local）；本地模式依赖宿主已登录的 CLI，容器内没有该登录态",
		LocalSandboxUnsupportedCode, driver)
}
