package config

import (
	"os"
	"slices"
	"strconv"
	"strings"
)

// Local mode turns this process into a single-user, loopback-only instance:
// no platform accounts, no cloud runners, no upstream calls. The switch is
// read from the environment on every call so a test can flip it in place and a
// long-running process never caches a stale decision.
const (
	// LocalModeErrorCode is the machine-readable code carried by every
	// local-mode rejection. The front end and the API docs key off this value.
	LocalModeErrorCode = "local_mode_unsupported"

	// localModeAdminLogin is the only login name a local-mode instance has.
	localModeAdminLogin = "local"
)

// LocalMode reports whether AGENTFLOW_LOCAL_MODE is enabled. An unset or
// unparseable value means off, which keeps every existing deployment on the
// current code path.
func LocalMode() bool {
	enabled, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("AGENTFLOW_LOCAL_MODE")))
	return err == nil && enabled
}

// LocalModeAdminLogin is the fixed login name of the single local account.
func LocalModeAdminLogin() string { return localModeAdminLogin }

// localModeExecutorKinds are the executor CLI kinds a local-mode instance can
// run: everything that executes on the machine itself. kimi and zcode are
// absent because their local form still depends on platform-side model and
// credential distribution; see LocalModeUnsupportedExecutors.
var localModeExecutorKinds = []string{
	"codebuddy",
	"codex",
	"claude-code",
	"opencode",
	"cursor",
	"agy",
	"pi",
	"dsh",
}

// LocalModeSupportsExecutor reports whether an executor kind can run in the
// current mode. Platform mode supports the whole catalog, so every caller can
// ask this unconditionally and never branch on the switch itself.
func LocalModeSupportsExecutor(kind string) bool {
	if !LocalMode() {
		return true
	}
	return slices.Contains(localModeExecutorKinds, strings.TrimSpace(kind))
}

// LocalModeUnsupportedExecutors lists the built-in CLI kinds local mode
// withdraws. It is the complement of localModeExecutorKinds and is empty in
// platform mode, so callers can use it in messages without checking the
// switch. local_mode_executors_test.go keeps the two lists in sync with the
// built-in catalog.
func LocalModeUnsupportedExecutors() []string {
	if !LocalMode() {
		return nil
	}
	unsupported := make([]string, 0, 2)
	for _, kind := range []string{"kimi", "zcode"} {
		if !LocalModeSupportsExecutor(kind) {
			unsupported = append(unsupported, kind)
		}
	}
	return unsupported
}

// LocalModeAllowRemote lifts the loopback-only listen check so the instance
// can be reached from another device on the user's own network. It is an
// explicit escape hatch: the process still talks to nothing outside itself,
// only the bind is no longer restricted to this host.
func LocalModeAllowRemote() bool {
	enabled, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("AGENTFLOW_LOCAL_ALLOW_REMOTE")))
	return err == nil && enabled
}
