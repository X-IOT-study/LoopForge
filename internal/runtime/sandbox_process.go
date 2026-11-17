package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"

	"agentflow/internal/config"
	"agentflow/internal/domain"
)

// projectAgentPaths are deliberately absent from the executor's filesystem
// view. AgentFlow definitions are the only source of instructions, skills and
// CLI configuration for unattended runs.
var rootProjectAgentPaths = []string{
	"AGENTS.md",
	"AGENTS.override.md",
	"CLAUDE.md",
	"CLAUDE.local.md",
	"CODEBUDDY.md",
	".agents",
	".codex",
	".claude",
	".codebuddy",
	".mcp.json",
}

var projectAgentDirNames = map[string]bool{
	".agents": true, ".codex": true, ".claude": true, ".codebuddy": true,
}

var projectAgentFileNames = map[string]bool{
	"AGENTS.md": true, "AGENTS.override.md": true,
	"CLAUDE.md": true, "CLAUDE.local.md": true,
	"CODEBUDDY.md": true, ".mcp.json": true,
}

func discoverProjectAgentPaths(workspace string) ([]string, error) {
	seen := map[string]bool{}
	for _, rel := range rootProjectAgentPaths {
		seen[filepath.Clean(rel)] = true
	}
	err := filepath.WalkDir(workspace, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(workspace, path)
		if err != nil || rel == "." {
			return err
		}
		name := entry.Name()
		if projectAgentDirNames[name] {
			seen[rel] = true
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if name == ".git" || name == ".agentflow" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if projectAgentFileNames[name] {
			seen[rel] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for rel := range seen {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out, nil
}

func validateAgentConfigTarget(workspace, rel string) (os.FileInfo, error) {
	target := filepath.Join(workspace, rel)
	info, err := os.Lstat(target)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return info, nil
	}
	if projectAgentDirNames[filepath.Base(rel)] {
		return nil, fmt.Errorf("refusing symlinked Agent CLI directory: %s", target)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, err
	}
	inside, err := filepath.Rel(root, resolved)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(os.PathSeparator)) || !projectAgentFileNames[filepath.Base(resolved)] {
		return nil, fmt.Errorf("refusing Agent CLI configuration symlink outside the known project configuration set: %s", target)
	}
	return os.Stat(target)
}

func appendEnvironment(base []string, overlays ...map[string]string) []string {
	values := map[string]string{}
	order := make([]string, 0, len(base))
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			continue
		}
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = item
	}
	for _, overlay := range overlays {
		keys := make([]string, 0, len(overlay))
		for key := range overlay {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if _, exists := values[key]; !exists {
				order = append(order, key)
			}
			values[key] = key + "=" + overlay[key]
		}
	}
	out := make([]string, 0, len(order))
	for _, key := range order {
		out = append(out, values[key])
	}
	return out
}

func controlledBaseEnvironment(base []string) []string {
	// Platform plumbing is stripped for every child. The host's own provider
	// credentials are stripped too, except in local mode where the machine's
	// CLIs are meant to keep using their existing login.
	blocked := []string{
		"AGENTFLOW_GIT_TOKEN=",
		"AGENTFLOW_MCP_CONFIG=",
		"AGENTFLOW_BIN=",
		"GIT_CONFIG_COUNT=",
		"GIT_CONFIG_KEY_",
		"GIT_CONFIG_VALUE_",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"SSH_AUTH_SOCK=",
		"GIT_SSH=",
		"GIT_SSH_COMMAND=",
		"GIT_SSH_VARIANT=",
		// Control-plane secrets stay out of every child, in both modes. They are
		// inherited from os.Environ() (the bootstrap in cmd/agentflow/main.go does
		// os.Setenv for the session key), so a workspace script could otherwise
		// read them and forge session cookies or reach the database.
		//
		// Filtering only affects the inherited base: the overlay is applied later
		// by appendEnvironment (sandbox_process.go:131-141, values[key] =
		// overlay), and the local/process sandboxes append command.environment
		// after the base (sandbox_local.go:54, sandbox_process.go:291). That is
		// how AGENTFLOW_GIT_TOKEN is blocked here yet re-injected by
		// command_binding.go:412, and the same holds for AGENTFLOW_CLI_API_KEY
		// (agent_cli.go:435), AGENTFLOW_RUN_ID/AGENTFLOW_NODE_ID
		// (command_binding.go:254, hooks.go:369), AGENTFLOW_REPOSITORIES_JSON
		// (hooks.go:34), AGENTFLOW_HOOK_OUTPUT (command_binding.go:240,
		// daemon.go:899) and AGENTFLOW_WORKSPACE (hooks.go:188).
		"AGENTFLOW_SESSION_SECRET=",
		"AGENTFLOW_MYSQL_DSN=",
		"AGENTFLOW_WORKER_CONTROL_TOKEN=",
		"AGENTFLOW_CONTROL_TOKEN=",
		"AGENTFLOW_PLATFORM_MCP_TOKEN=",
		"AGENTFLOW_RUNNER_BOOTSTRAP_TOKEN=",
		"AGENTFLOW_MCP_CALL_TOKEN=",
		"AGENTFLOW_API_TOKEN=",
		"LOOPFORGE_DB_PASSWORD=",
		"LOOPFORGE_DB_ROOT_PASSWORD=",
		"LOOPFORGE_MYSQL_DSN=",
		// Gateway identity signing, SSO/robot/MCP credentials, git HTTP
		// passwords, notification webhooks and the legacy SESSION_SECRET name are
		// parent-side only. AGENTFLOW_GIT_HTTP_PASSWORD is injected per run
		// through the overlay (gitwork/auth.go:105) and already stripped from
		// inherited env there (gitwork/auth.go:129), so filtering it here keeps
		// that contract. AGENTFLOW_API_KEY has no child consumer (it is not the
		// CLI key; that one is AGENTFLOW_CLI_API_KEY, which stays inherited).
		"AGENTFLOW_IDENTITY_HEADER_SECRET=",
		"AGENTFLOW_GONGFENG_OAUTH_CLIENT_SECRET=",
		"AGENTFLOW_MCP_TOKEN=",
		"AGENTFLOW_INSIGHT_TOKEN=",
		"AGENTFLOW_GIT_HTTP_PASSWORD=",
		"AGENTFLOW_DATABASE_URL=",
		"AGENTFLOW_API_KEY=",
		"SESSION_SECRET=",
		"AGENTFLOW_NOTIFICATION_URL__",
	}
	if !config.LocalMode() {
		blocked = append(blocked, []string{
			"CODEBUDDY_API_KEY=",
			"OPENAI_API_KEY=", "CODEX_API_KEY=", "OPENAI_BASE_URL=",
			"ANTHROPIC_API_KEY=", "ANTHROPIC_AUTH_TOKEN=", "ANTHROPIC_BASE_URL=",
			"CLAUDE_CODE_OAUTH_TOKEN=", "CODEX_HOME=", "CLAUDE_CONFIG_DIR=",
			"CLAUDE_CODE_USE_BEDROCK=", "CLAUDE_CODE_USE_VERTEX=", "CLAUDE_CODE_USE_FOUNDRY=",
			"CODEBUDDY_INTERNET_ENVIRONMENT=",
		}...)
	}
	filtered := make([]string, 0, len(base))
	for _, entry := range base {
		drop := false
		for _, prefix := range blocked {
			if strings.HasPrefix(entry, prefix) {
				drop = true
				break
			}
		}
		if !drop {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

type processSandbox struct {
	id, workspace string
	driver        string
	profile       domain.SandboxProfile
	requirements  SandboxRequirements
	binary        string
}

func (s *processSandbox) ID() string        { return s.id }
func (s *processSandbox) Workspace() string { return s.workspace }
func (s *processSandbox) Driver() string    { return s.driver }
func (s *processSandbox) Close(context.Context) error {
	stopHostBrowsers(s.workspace)
	return nil
}

func createSeatbelt(id, workspace string, profile domain.SandboxProfile, requirements SandboxRequirements) (Sandbox, error) {
	return createProcessSandbox("seatbelt", "darwin", "macOS", "sandbox-exec", id, workspace, profile, requirements)
}

func createBwrap(id, workspace string, profile domain.SandboxProfile, requirements SandboxRequirements) (Sandbox, error) {
	return createProcessSandbox("bwrap", "linux", "Linux", "bwrap", id, workspace, profile, requirements)
}

// createProcessSandbox builds a processSandbox for driver once the host runs
// requiredOS (named osName in errors) and binaryName is on PATH.
func createProcessSandbox(driver, requiredOS, osName, binaryName, id, workspace string, profile domain.SandboxProfile, requirements SandboxRequirements) (Sandbox, error) {
	if goruntime.GOOS != requiredOS {
		return nil, fmt.Errorf("%s sandbox requires %s", driver, osName)
	}
	binary, err := exec.LookPath(binaryName)
	if err != nil {
		return nil, fmt.Errorf("%s sandbox unavailable: %w", driver, err)
	}
	return &processSandbox{id: id, workspace: workspace, driver: driver, profile: profile, requirements: requirements, binary: binary}, nil
}

func (s *processSandbox) Exec(ctx context.Context, command CommandSpec, sink EventSink) (string, error) {
	cmd, err := s.command(ctx, command, controlledBaseEnvironment(os.Environ()))
	if err != nil {
		return "", err
	}
	return runCommand(cmd, sink)
}

func (s *processSandbox) command(ctx context.Context, command CommandSpec, base []string) (*exec.Cmd, error) {
	if len(command.Argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	var cmd *exec.Cmd
	switch s.driver {
	case "seatbelt":
		profile, err := seatbeltProfileCommand(s.workspace, s.requirements, !trustedScriptCommand(command))
		if err != nil {
			return nil, err
		}
		args := append([]string{"-p", profile}, command.Argv...)
		cmd = exec.CommandContext(ctx, s.binary, args...)
	case "bwrap":
		args, err := bwrapArgsCommand(s.workspace, s.requirements, !trustedScriptCommand(command))
		if err != nil {
			return nil, err
		}
		args = bwrapPrivateBinds(args, command.privateBinds)
		args = append(args, command.Argv...)
		cmd = exec.CommandContext(ctx, s.binary, args...)
	default:
		return nil, fmt.Errorf("unsupported process sandbox %q", s.driver)
	}
	cmd.Dir = s.workspace
	cmd.Env = appendEnvironment(base, s.profile.Environment, command.environment)
	switch s.driver {
	case "seatbelt":
		cmd.Env = upsertEnv(cmd.Env, "TMPDIR", preferredSeatbeltTemp())
	case "bwrap":
		for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
			cmd.Env = upsertEnv(cmd.Env, key, "/tmp")
		}
	}
	return cmd, nil
}

func seatbeltWritableTemps() []string {
	seen := map[string]bool{}
	var paths []string
	candidates := []string{"/private/tmp"}
	if goruntime.GOOS == "darwin" {
		// macOS frameworks and native clients also use per-user cache and
		// temporary siblings below /private/var/folders. Restricting the
		// allowance to os.TempDir() (.../T) breaks Codex TLS trust evaluation
		// with UnknownIssuer even though ordinary curl requests still work.
		candidates = append(candidates, "/private/var/folders")
	}
	// Keep the concrete TMPDIR last so preferredSeatbeltTemp returns it rather
	// than the broader per-user container root.
	candidates = append(candidates, os.TempDir())
	for _, candidate := range candidates {
		if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
			candidate = resolved
		}
		candidate, _ = filepath.Abs(candidate)
		candidate = filepath.Clean(candidate)
		if candidate != "." && candidate != string(filepath.Separator) && !seen[candidate] {
			seen[candidate] = true
			paths = append(paths, candidate)
		}
	}
	return paths
}

func preferredSeatbeltTemp() string {
	paths := seatbeltWritableTemps()
	if len(paths) > 1 {
		return paths[len(paths)-1]
	}
	if len(paths) == 1 {
		return paths[0]
	}
	return "/private/tmp"
}

func agentStateDirectoryRequirements(requirements SandboxRequirements) (string, error) {
	directories, err := agentStateDirectories(requirements)
	if err != nil || len(directories) == 0 {
		return "", err
	}
	return directories[0].path, nil
}

type resolvedCLIState struct {
	path  string
	files []string
}

// agentStateDirectories resolves the CLI state requirements of one invocation:
// exactly the CLI selected for this run. Docker keeps that scope verbatim, so
// the local-mode host extension below never widens what a container mounts.
func agentStateDirectories(requirements SandboxRequirements) ([]resolvedCLIState, error) {
	states := append([]CLIStateRequirement(nil), requirements.CLIStates...)
	if len(states) == 0 {
		return nil, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(states))
	files := map[string][]string{}
	seen := map[string]bool{}
	for _, state := range states {
		name := strings.TrimSpace(state.Directory)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
		files[name] = append([]string(nil), state.Files...)
	}
	result := make([]resolvedCLIState, 0, len(names))
	for _, name := range names {
		result = append(result, resolvedCLIState{path: filepath.Join(home, filepath.FromSlash(name)), files: files[name]})
	}
	return result, nil
}

// localHostStateFiles are the HOME-relative regular files some host CLIs
// rewrite in place beside their state directory instead of inside it. bwrap
// mounts / read-only, so each one needs its own writable file-level bind:
// claude rewrites ~/.claude.json on every run and hits EROFS without it. The
// names match the files the docker path already mounts (taskSandboxSpec's
// CLIStates). The macOS seatbelt profile only allows directory-level writes, so
// it does not bind these files; that limitation is documented as such.
var localHostStateFiles = map[string][]string{
	".claude":    {".claude.json"},
	".codebuddy": {".codebuddy.json"},
}

// localStateFiles merges the requirement files of state with the host files
// that live beside its directory, keeping the first spelling of each name.
func localStateFiles(state resolvedCLIState) []string {
	files := append([]string(nil), state.files...)
	seen := make(map[string]bool, len(files))
	for _, name := range files {
		seen[name] = true
	}
	for _, name := range localHostStateFiles[filepath.Base(state.path)] {
		if seen[name] {
			continue
		}
		seen[name] = true
		files = append(files, name)
	}
	return files
}

// localHostStateDirectories extends agentStateDirectories with the host CLI
// state directories that host-process drivers must bind writable in local
// mode: the machine's own CLIs keep their login and session state there, and a
// read-only bind makes a token refresh fail with EROFS.
//
// Only bwrap and seatbelt call this. Docker intentionally keeps the strict
// agentStateDirectories scope: mounting every host CLI home writable into a
// container turns one selected CLI into a pile of host credentials the run
// never asked for, and container semantics were never host-first. A missing
// directory is dropped by the driver that binds it.
func localHostStateDirectories(requirements SandboxRequirements) ([]resolvedCLIState, error) {
	states, err := agentStateDirectories(requirements)
	if err != nil || !config.LocalMode() {
		return states, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(states))
	for _, state := range states {
		rel, relErr := filepath.Rel(home, state.path)
		if relErr != nil {
			continue
		}
		seen[filepath.ToSlash(rel)] = true
	}
	names := make([]string, 0, len(agentCLIStateDirs))
	for kind := range agentCLIStateDirs {
		names = append(names, kind)
	}
	sort.Strings(names)
	result := append(make([]resolvedCLIState, 0, len(states)+len(names)), states...)
	for _, kind := range names {
		name := agentCLIStateDirs[kind]
		if seen[name] {
			continue
		}
		seen[name] = true
		result = append(result, resolvedCLIState{path: filepath.Join(home, filepath.FromSlash(name))})
	}
	for i := range result {
		result[i].files = localStateFiles(result[i])
	}
	return result, nil
}

func seatbeltProfileRequirements(workspace string, requirements SandboxRequirements) (string, error) {
	return seatbeltProfileCommand(workspace, requirements, true)
}

func seatbeltProfileCommand(workspace string, requirements SandboxRequirements, maskAgentConfig bool) (string, error) {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(allow default)\n")
	// A broad write denial plus a narrower workspace allowance keeps the host
	// immutable while retaining the normal coding-agent workflow.
	b.WriteString("(deny file-write*)\n")
	fmt.Fprintf(&b, "(allow file-write* (subpath %s))\n", strconv.Quote(root))
	for _, temp := range seatbeltWritableTemps() {
		fmt.Fprintf(&b, "(allow file-write* (subpath %s))\n", strconv.Quote(temp))
	}
	stateDirs, err := localHostStateDirectories(requirements)
	if err != nil {
		return "", err
	}
	for _, state := range stateDirs {
		// Directory-level only: seatbelt has no file-level writable bind, so the
		// state files beside it (e.g. ~/.claude.json) stay read-only on macOS.
		// That is a documented limitation, not a TODO.
		fmt.Fprintf(&b, "(allow file-write* (subpath %s))\n", strconv.Quote(state.path))
	}
	b.WriteString("(allow file-write* (literal \"/dev/null\"))\n")
	if !maskAgentConfig {
		return b.String(), nil
	}
	maskedPaths, err := discoverProjectAgentPaths(root)
	if err != nil {
		return "", err
	}
	for _, rel := range maskedPaths {
		path := filepath.Join(root, rel)
		if _, err := os.Lstat(path); err == nil {
			if _, err := validateAgentConfigTarget(root, rel); err != nil {
				return "", err
			}
		} else if !os.IsNotExist(err) {
			return "", err
		}
		if projectAgentDirNames[filepath.Base(rel)] {
			fmt.Fprintf(&b, "(deny file-read* (subpath %s))\n", strconv.Quote(path))
		} else {
			fmt.Fprintf(&b, "(deny file-read* (literal %s))\n", strconv.Quote(path))
		}
	}
	return b.String(), nil
}

// bunFSMountPoint is the virtual root Bun-compiled CLIs (Claude Code,
// CodeBuddy) resolve their embedded files under. A variable for tests.
var bunFSMountPoint = "/$bunfs"

// ensureBunFSMountPoint makes sure bwrap can mount a private tmpfs at
// bunFSMountPoint. bwrap binds the host root read-only, so it cannot create a
// missing mount point and the whole sandbox would fail ("Can't create file
// /$bunfs: Read-only file system"). The host directory is created once when
// the runner may (root, as the builtin runner); otherwise the mount is
// skipped and Bun falls back to its in-process virtual filesystem.
func ensureBunFSMountPoint() bool {
	if info, err := os.Stat(bunFSMountPoint); err == nil {
		return info.IsDir()
	}
	if err := os.Mkdir(bunFSMountPoint, 0o755); err != nil && !os.IsExist(err) {
		return false
	}
	return true
}

func bwrapArgsRequirements(workspace string, requirements SandboxRequirements) ([]string, error) {
	return bwrapArgsCommand(workspace, requirements, true)
}

// bwrapPrivateBinds adds per-process binds before the "--" that ends the
// bwrap options. They come after --tmpfs /tmp, so a directory under the host
// /tmp is visible to this process only.
func bwrapPrivateBinds(args, binds []string) []string {
	if len(binds) == 0 || len(args) == 0 || args[len(args)-1] != "--" {
		return args
	}
	out := append([]string{}, args[:len(args)-1]...)
	for _, dir := range binds {
		out = append(out, "--bind", dir, dir)
	}
	return append(out, "--")
}

func bwrapArgsCommand(workspace string, requirements SandboxRequirements, maskAgentConfig bool) ([]string, error) {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	args := []string{
		"--die-with-parent", "--new-session",
		"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts",
		"--ro-bind", "/", "/",
		"--bind", root, root,
		"--chdir", root,
		"--tmpfs", "/tmp",
		"--proc", "/proc",
		"--dev-bind", "/dev", "/dev",
	}
	if requirements.BunFS && ensureBunFSMountPoint() {
		args = append(args, "--tmpfs", bunFSMountPoint)
	}
	stateDirs, err := localHostStateDirectories(requirements)
	if err != nil {
		return nil, err
	}
	for _, state := range stateDirs {
		if info, statErr := os.Stat(state.path); statErr == nil && info.IsDir() {
			args = append(args, "--bind", state.path, state.path)
		} else if statErr != nil && !os.IsNotExist(statErr) {
			return nil, statErr
		}
		// Files beside the state directory (e.g. ~/.claude.json) are siblings, the
		// same layout the docker driver mounts. "--ro-bind / /" makes them
		// read-only while the CLI rewrites them in place, so local mode binds each
		// existing regular file writable. A missing file or parent is skipped.
		// Platform mode keeps its previous argument list byte for byte.
		if !config.LocalMode() {
			continue
		}
		for _, name := range state.files {
			path := filepath.Join(filepath.Dir(state.path), filepath.FromSlash(name))
			info, statErr := os.Stat(path)
			if statErr != nil {
				if os.IsNotExist(statErr) {
					continue
				}
				return nil, statErr
			}
			if !info.Mode().IsRegular() {
				continue
			}
			args = append(args, "--bind", path, path)
		}
	}
	if maskAgentConfig {
		maskArgs, err := bwrapAgentConfigMasks(root)
		if err != nil {
			return nil, err
		}
		args = append(args, maskArgs...)
	}
	args = append(args, "--")
	return args, nil
}

func isolationMaskSources(workspace string) (emptyDir, emptyFile string, err error) {
	return isolationMaskSourcesAt(filepath.Join(workspace, ".agentflow", "isolation"))
}

func isolationMaskSourcesAt(root string) (emptyDir, emptyFile string, err error) {
	if err = os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return "", "", err
	}
	if err = ensureDirectoryNoSymlink(root, 0o700); err != nil {
		return "", "", err
	}
	emptyDir = filepath.Join(root, "empty-dir")
	emptyFile = filepath.Join(root, "empty-file")
	if err = ensureDirectoryNoSymlink(emptyDir, 0o700); err != nil {
		return "", "", err
	}
	if info, statErr := os.Lstat(emptyFile); statErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return "", "", fmt.Errorf("sandbox mask path is not a regular file: %s", emptyFile)
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return "", "", statErr
	}
	file, openErr := os.OpenFile(emptyFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if openErr != nil {
		return "", "", openErr
	}
	if err = file.Close(); err != nil {
		return "", "", err
	}
	return emptyDir, emptyFile, nil
}

func bwrapAgentConfigMasks(workspace string) ([]string, error) {
	emptyDir, emptyFile, err := isolationMaskSources(workspace)
	if err != nil {
		return nil, err
	}
	args := []string{"--ro-bind", emptyDir, emptyDir, "--ro-bind", emptyFile, emptyFile}
	maskedPaths, err := discoverProjectAgentPaths(workspace)
	if err != nil {
		return nil, err
	}
	for _, rel := range maskedPaths {
		target := filepath.Join(workspace, rel)
		info, statErr := validateAgentConfigTarget(workspace, rel)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return nil, statErr
		}
		source := emptyFile
		if info.IsDir() {
			source = emptyDir
		}
		args = append(args, "--ro-bind", source, target)
	}
	return args, nil
}

func dockerIsolationMaskRoot(workspace string) string {
	workspace = filepath.Clean(workspace)
	return filepath.Join(filepath.Dir(workspace), ".isolation-masks", filepath.Base(workspace))
}

// ensureDockerMaskTargets creates the in-workspace mount targets for the
// isolation mask sources: Docker Desktop cannot create missing nested mount
// targets on a VirtioFS share.
func ensureDockerMaskTargets(workspace string) error {
	if _, err := EnsureControlDirectory(workspace, "isolation", "empty-dir"); err != nil {
		return err
	}
	target := filepath.Join(workspace, ".agentflow", "isolation", "empty-file")
	root, rel, err := openWorkspacePath(workspace, target)
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := root.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		return file.Close()
	}
	if !os.IsExist(err) {
		return err
	}
	info, err := root.Lstat(rel)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("sandbox mask target is not a regular file: %s", target)
	}
	return nil
}

func dockerAgentConfigMasks(workspace string, aliases ...string) ([]string, error) {
	if err := ensureDockerMaskTargets(workspace); err != nil {
		return nil, err
	}
	emptyDir, emptyFile, err := isolationMaskSourcesAt(dockerIsolationMaskRoot(workspace))
	if err != nil {
		return nil, err
	}
	emptyDirTarget := filepath.ToSlash(filepath.Join("/workspace", ".agentflow", "isolation", "empty-dir"))
	emptyFileTarget := filepath.ToSlash(filepath.Join("/workspace", ".agentflow", "isolation", "empty-file"))
	args := []string{
		"--mount", "type=bind,src=" + emptyDir + ",dst=" + emptyDirTarget + ",readonly",
		"--mount", "type=bind,src=" + emptyFile + ",dst=" + emptyFileTarget + ",readonly",
	}
	maskedPaths, err := discoverProjectAgentPaths(workspace)
	if err != nil {
		return nil, err
	}
	for _, rel := range maskedPaths {
		info, statErr := validateAgentConfigTarget(workspace, rel)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return nil, statErr
		}
		source := emptyFile
		if info.IsDir() {
			source = emptyDir
		}
		containerTarget := filepath.ToSlash(filepath.Join("/workspace", rel))
		args = append(args, "--mount", "type=bind,src="+source+",dst="+containerTarget+",readonly")
		for _, alias := range aliases {
			args = append(args, "--mount", "type=bind,src="+source+",dst="+filepath.ToSlash(filepath.Join(alias, rel))+",readonly")
		}
	}
	return args, nil
}

// dockerAgentStateMount exposes only the state directory belonging to the CLI
// selected for this invocation. Authentication and session state stay in the
// operator's normal CLI home; they are never copied into the workspace, stored
// in AgentFlow definitions, or passed through docker exec environment flags.
// Command nodes and unknown executables receive no Agent CLI state mount.
func dockerAgentStateMountRequirements(requirements SandboxRequirements) ([]string, error) {
	stateDirs, err := agentStateDirectories(requirements)
	if err != nil || len(stateDirs) == 0 {
		return nil, err
	}
	var args []string
	for _, state := range stateDirs {
		info, statErr := os.Stat(state.path)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return nil, statErr
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("Agent CLI state path is not a directory: %s", state.path)
		}
		target := filepath.ToSlash(filepath.Join("/root", filepath.Base(state.path)))
		args = append(args, "--mount", "type=bind,src="+state.path+",dst="+target)
		for _, name := range state.files {
			stateFile := filepath.Join(filepath.Dir(state.path), name)
			fileInfo, fileErr := os.Stat(stateFile)
			if fileErr == nil {
				if !fileInfo.Mode().IsRegular() {
					return nil, fmt.Errorf("Agent CLI state path is not a regular file: %s", stateFile)
				}
				args = append(args, "--mount", "type=bind,src="+stateFile+",dst=/root/"+name)
			} else if !os.IsNotExist(fileErr) {
				return nil, fileErr
			}
		}
	}
	return args, nil
}
