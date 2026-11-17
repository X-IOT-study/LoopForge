package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/engine"
	"agentflow/internal/httpapi"
	"agentflow/internal/managedcli"
	"agentflow/internal/notification"
	"agentflow/internal/preview"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"agentflow/internal/tracker"
	"github.com/gin-gonic/gin"
)

// loadDotEnv loads KEY=VALUE pairs from the first .env file found. Search
// order: the current working directory and up to three parents (so running
// from a subdirectory still finds the project root), then next to the
// executable. Existing environment variables take precedence and are never
// overwritten.
func loadDotEnv() {
	var candidates []string
	if cwd, err := os.Getwd(); err == nil {
		dir := cwd
		for i := 0; i < 4; i++ {
			candidates = append(candidates, filepath.Join(dir, ".env"))
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), ".env"))
	}
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimPrefix(line, "export ")
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			k = strings.TrimSpace(k)
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			if k != "" && os.Getenv(k) == "" {
				os.Setenv(k, v)
			}
		}
		return
	}
}

// applyLocalMode enforces the local-only deployment contract: no gateway
// identity headers, loopback-only listening, a self-bootstrapped session key,
// and mandatory sign-in. It runs after loadDotEnv so .env values count, and
// before the AGENTFLOW_SESSION_SECRET check so the bootstrap can satisfy it.
//
// The guard is the first statement: administrative one-shot commands
// (-cli-action, -browser-image) must not be killed by listen or key checks.
func applyLocalMode(role, addr, workspaces, cliAction string, browserImage bool) {
	if !config.LocalMode() || cliAction != "" || browserImage {
		return
	}
	if enabled, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("AGENTFLOW_TRUST_IDENTITY_HEADERS"))); err == nil && enabled {
		log.Fatal("local mode has no gateway in front of it; unset AGENTFLOW_TRUST_IDENTITY_HEADERS")
	}
	listen := localListenAddress(role, addr)
	if !config.LocalModeAllowRemote() && !loopbackListen(listen) {
		log.Fatalf("local mode listens on loopback only; use -addr 127.0.0.1:8080 or set AGENTFLOW_LOCAL_ALLOW_REMOTE=1 (got %q)", listen)
	}
	if os.Getenv("AGENTFLOW_SESSION_SECRET") == "" {
		path, err := localSessionKeyPath(workspaces)
		if err != nil {
			log.Fatal(err)
		}
		secret, created, err := localSessionSecret(path)
		if err != nil {
			log.Fatal(err)
		}
		os.Setenv("AGENTFLOW_SESSION_SECRET", secret)
		if created {
			log.Printf("local mode: session key generated at %s", path)
		} else {
			log.Printf("local mode: session key loaded from %s", path)
		}
	}
	forceLocalAuth()
	if strings.TrimSpace(os.Getenv("AGENTFLOW_LOCAL_SANDBOX")) == "docker" {
		// Local mode keeps the process sandbox; an explicit docker driver still
		// works, but every node then runs in a container.
		log.Printf("local mode: AGENTFLOW_LOCAL_SANDBOX=docker keeps the docker sandbox driver")
	}
	log.Printf("local mode: cloud runner pool, image prebuild, tapd sync, notification delivery disabled")
}

// localListenAddress resolves the address this process will really bind,
// through config.LoadEndpoints, so the loopback check cannot drift from it: the
// worker role binds its own default (:8081), not the API port.
func localListenAddress(role, addrFlag string) string {
	if v := strings.TrimSpace(addrFlag); v != "" {
		return v
	}
	role = strings.ToLower(strings.TrimSpace(role))
	if endpoints, err := config.LoadEndpoints(role, ""); err == nil && strings.TrimSpace(endpoints.ListenAddr) != "" {
		return endpoints.ListenAddr
	}
	// LoadEndpoints rejects a malformed port; mirror its fallback rather than
	// skipping the check, so a bad configuration still fails closed.
	if role == "worker" {
		return ":8081"
	}
	port := strings.TrimSpace(os.Getenv("AGENTFLOW_HTTP_PORT"))
	if port == "" {
		port = "8080"
	}
	return ":" + port
}

// loopbackListen reports whether an address binds this host only. An empty
// host part (":8080") is every interface, not loopback.
func loopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	host = strings.TrimSpace(host)
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// localSessionKeyPath is <data dir>/local/session.key. The data directory
// comes from AGENTFLOW_DATA_DIR and defaults to the parent of the workspace
// root, so a default install keeps everything under ~/.agentflow.
func localSessionKeyPath(workspaces string) (string, error) {
	dir := strings.TrimSpace(os.Getenv("AGENTFLOW_DATA_DIR"))
	if dir == "" {
		root, err := resolveWorkspaceRoot(workspaces)
		if err != nil {
			return "", err
		}
		dir = filepath.Join(filepath.Dir(root), "data")
	}
	return filepath.Join(dir, "local", "session.key"), nil
}

// errSessionKeyEmpty marks a key file that exists but holds no usable key, and
// stopped changing while it was read: either a crash left it behind or a
// concurrent first start has not written it yet.
var errSessionKeyEmpty = errors.New("session key file is empty")

// localSessionSecret returns the persisted session key, creating it on first
// start. Persisting it is what keeps a browser session valid across restarts;
// the file is 0600 inside a 0700 directory because it signs every cookie.
//
// Creation is exclusive (O_EXCL) so two processes starting at once cannot each
// mint a key and invalidate the other's cookies: the loser reads the winner's
// key instead. The content is fsynced before the winner reports success.
func localSessionSecret(path string) (string, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		secret, err := createLocalSessionSecret(path)
		if err == nil {
			return secret, true, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", false, err
		}
		// Another process owns the file; wait for its key.
		secret, err = awaitLocalSessionSecret(path)
		if err == nil {
			return secret, false, nil
		}
		if !errors.Is(err, errSessionKeyEmpty) {
			return "", false, err
		}
		// The file holds no key and stopped changing: drop the leftover and race
		// for creation again.
		if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			return "", false, removeErr
		}
	}
	return "", false, fmt.Errorf("session key %s could not be initialised", path)
}

// createLocalSessionSecret mints a key into path, failing with fs.ErrExist when
// another process got there first.
func createLocalSessionSecret(path string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	secret := base64.RawURLEncoding.EncodeToString(buf)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, writeErr := file.WriteString(secret)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = os.Remove(path)
		return "", writeErr
	}
	return secret, nil
}

// awaitLocalSessionSecret reads a key another process is creating. A fresh but
// still empty file is retried for a short while so a slow winner is not
// mistaken for a stale one.
func awaitLocalSessionSecret(path string) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		secret, err := readLocalSessionSecret(path)
		if !errors.Is(err, errSessionKeyEmpty) {
			return secret, err
		}
		time.Sleep(3 * time.Millisecond)
	}
	return "", errSessionKeyEmpty
}

// readLocalSessionSecret loads an existing key, refusing anything but a regular
// file and tightening permissions that were widened by hand.
func readLocalSessionSecret(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("session key %s must be a regular file, got %s", path, info.Mode().Type())
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		if chmodErr := os.Chmod(path, 0o600); chmodErr != nil {
			return "", chmodErr
		}
		log.Printf("local mode: session key %s had mode %04o, tightened to 0600", path, perm)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(content))
	if secret == "" {
		return "", errSessionKeyEmpty
	}
	return secret, nil
}

// forceLocalAuth makes sign-in mandatory. A local instance owns the user's own
// machine data, so serving it to anyone who can reach the port without a
// password is never the intended state.
func forceLocalAuth() {
	if raw := strings.TrimSpace(os.Getenv("AGENTFLOW_AUTH_REQUIRED")); raw != "" {
		if enabled, err := strconv.ParseBool(raw); err == nil && enabled {
			return
		}
		log.Printf("local mode: AGENTFLOW_AUTH_REQUIRED=%q overridden; sign-in is mandatory", raw)
	}
	os.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
}

func main() {
	if runtime.RunMCPCallCLI(os.Args[1:]) {
		return
	}
	addr := flag.String("addr", "", "HTTP listen address; overrides AGENTFLOW_HTTP_PORT")
	workspaces := flag.String("workspaces", "", "sandbox workspace root")
	workerConcurrency := flag.Int("worker-concurrency", 8, "maximum concurrent node sandboxes for this worker")
	basicUser := flag.String("basic-user", "", "optional HTTP Basic Auth username")
	basicPass := flag.String("basic-pass", "", "optional HTTP Basic Auth password")
	mcpToken := flag.String("mcp-token", "", "MCP gateway bearer token (or set AGENTFLOW_MCP_TOKEN env)")
	tlsCert := flag.String("tls-cert", "", "optional TLS certificate path")
	tlsKey := flag.String("tls-key", "", "optional TLS private key path")
	mysqlDSN := flag.String("mysql-dsn", "", "MySQL DSN; overrides AGENTFLOW_MYSQL_DSN. MySQL is the only data source.")
	roleFlag := flag.String("role", "", "process role: api, worker, or all (default all)")
	cliAction := flag.String("cli-action", "", "local managed CLI administration: status, update, or rollback; exits without starting services")
	cliProvider := flag.String("cli-provider", "", "provider for CLI rollback: codex, codebuddy, or claude-code")
	cliTimeout := flag.Duration("cli-timeout", 80*time.Minute, "total managed CLI operation timeout")
	browserImage := flag.Bool("browser-image", false, "build or pull the agent browser sidecar image, print its name and exit")
	flag.Parse()
	loadDotEnv()
	applyLocalMode(*roleFlag, *addr, *workspaces, *cliAction, *browserImage)
	if *browserImage {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		image, err := runtime.EnsureBrowserImage(ctx)
		stop()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(image)
		return
	}
	if os.Getenv("AGENTFLOW_SESSION_SECRET") == "" && *cliAction == "" {
		log.Fatal("AGENTFLOW_SESSION_SECRET is required; generate with openssl rand -hex 32")
	}
	if os.Getenv("AGENTFLOW_AUTH_REQUIRED") == "" {
		os.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
	}
	if *cliAction != "" || *cliProvider != "" {
		if flag.NArg() != 0 {
			log.Fatal("CLI administration does not accept positional arguments")
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		err := runCLI(ctx, *cliAction, *cliProvider, *cliTimeout, os.Stdout)
		stop()
		if err != nil {
			log.Fatal(err)
		}
		return
	}
	role, err := resolveRole(*roleFlag)
	if err != nil {
		log.Fatal(err)
	}
	root, err := resolveWorkspaceRoot(*workspaces)
	if err != nil {
		log.Fatal(err)
	}
	s, err := repo.Open(strings.TrimSpace(*mysqlDSN))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("storage driver=%v database=%v role=%s", s.StorageInfo()["driver"], s.StorageInfo()["database"], role)
	e := engine.New(s, runtime.NewManagerWithLimit(root, *workerConcurrency))
	// Local mode never reaches the network: the isolated image would be pulled
	// from a registry, so it is skipped there (see applyLocalMode).
	if !config.LocalMode() {
		runtime.PrebuildIsolatedImage()
	}
	host, _ := os.Hostname()
	instanceID := strings.TrimSpace(os.Getenv("AGENTFLOW_INSTANCE_ID"))
	if instanceID == "" {
		instanceID = fmt.Sprintf("%s-%s-%d", role, host, os.Getpid())
	}
	leaseTTL := 30 * time.Second
	if raw := strings.TrimSpace(os.Getenv("AGENTFLOW_RUN_LEASE_TTL")); raw != "" {
		parsed, parseErr := time.ParseDuration(raw)
		if parseErr != nil || parsed <= 0 {
			log.Fatalf("invalid AGENTFLOW_RUN_LEASE_TTL %q", raw)
		}
		leaseTTL = parsed
	}
	executeRuns := role != "api"
	if role == "all" {
		if raw := strings.TrimSpace(os.Getenv("AGENTFLOW_EXECUTE_RUNS")); raw != "" {
			parsed, parseErr := strconv.ParseBool(raw)
			if parseErr != nil {
				log.Fatalf("invalid AGENTFLOW_EXECUTE_RUNS %q", raw)
			}
			executeRuns = parsed
		}
	}
	e.ConfigureExecution(instanceID, host, leaseTTL, executeRuns)
	// The cloud runner pool belongs to a platform deployment; local mode runs
	// every node on this host so the pool is never configured.
	if !config.LocalMode() {
		e.ConfigureCloudRunnerPool(engine.CloudRunnerPool{
			Label:    os.Getenv("AGENTFLOW_CLOUD_RUNNER_LABEL"),
			Projects: engine.ParseCloudRunnerProjects(os.Getenv("AGENTFLOW_CLOUD_RUNNER_PROJECTS")),
			// AGENTFLOW_LOCAL_EXECUTION=false keeps this host from running nodes
			// of pool projects: they wait for, or move to, a pool runner.
			Required: strings.EqualFold(strings.TrimSpace(os.Getenv("AGENTFLOW_LOCAL_EXECUTION")), "false"),
		})
	}
	attachPreviewStore(e, s)
	log.Printf("engine instance=%s role=%s execute_runs=%t run_lease_ttl=%s", instanceID, role, executeRuns, leaseTTL)
	claimCtx, stopClaim := context.WithCancel(context.Background())
	defer stopClaim()
	endpoints, err := config.LoadEndpoints(role, strings.TrimSpace(*addr))
	if err != nil {
		log.Fatal(err)
	}
	config.SetCurrent(endpoints)
	e.ConfigurePlatformMCP(endpoints.PlatformBase, endpoints.PlatformMCPToken)
	if endpoints.PlatformBase == "" {
		log.Printf("platform MCP / knowledge proxy: no sandbox-reachable platform URL; cloud runs skip them (set AGENTFLOW_PLATFORM_MCP_URL)")
	} else {
		log.Printf("platform MCP / knowledge proxy base=%s", endpoints.PlatformBase)
	}

	mainLn, inherited, err := listenMain(endpoints.ListenAddr)
	if err != nil {
		log.Fatal(err)
	}
	var srv *http.Server
	var syncSvc *tracker.Service
	var background *httpapi.Background
	var agentToolSrv *http.Server
	readyStatus := "serving"
	switch role {
	case "worker":
		e.ConfigureControl(engine.ControlConfig{Role: role, URL: endpoints.ControlURL, Token: endpoints.ControlToken, APIURL: endpoints.APIBase})
		e.StartInstanceHeartbeat(claimCtx)
		srv = &http.Server{Addr: endpoints.ListenAddr, Handler: workerHandler(e), ReadHeaderTimeout: 10 * time.Second}
	default:
		readyStatus = handoverStatus
		agentToolLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatal(err)
		}
		endpoints = endpoints.WithPrivateAgentToolGateway("http://" + agentToolLn.Addr().String())
		config.SetCurrent(endpoints)
		log.Printf("agent tool gateway=%s", endpoints.AgentToolGateway)
		e.ConfigureControl(engine.ControlConfig{Token: endpoints.ControlToken, FallbackURL: endpoints.WorkerFallbackURL, APIURL: endpoints.APIBase})
		listenAddr := endpoints.ListenAddr
		syncSvc = tracker.NewService()
		token := *mcpToken
		if token == "" {
			token = os.Getenv("AGENTFLOW_MCP_TOKEN")
		}
		if token != "" {
			syncSvc.SetAuthToken(token)
		}
		gin.SetMode(gin.ReleaseMode)
		cfg := s.SyncConfig()
		syncSvc.ConfigureWithStore(cfg, s)
		// Tapd sync and MR state polling both call the upstream platform, which
		// a local-only instance does not have.
		if !config.LocalMode() {
			if cfg.AutoSyncEnabled {
				syncSvc.StartAutoSync(s)
			}
			syncSvc.StartMRStateSync(s)
		}
		notifications := notification.New(s)
		opts := httpapi.BackgroundOptions{}
		if inherited {
			log.Printf("taking over listener %s from pid %d; startup recovery waits for it to exit", mainLn.Addr(), os.Getppid())
			opts.RecoveryGate = predecessorExited()
		}
		handler, bg := httpapi.NewWithNotificationsAndBackground(claimCtx, s, e, syncSvc, *basicUser, *basicPass, notifications, opts)
		background = bg
		defer background.Stop()
		// Notification delivery needs the platform's bot credentials.
		if !config.LocalMode() {
			notifications.Start(claimCtx)
		}
		srv = &http.Server{Addr: listenAddr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
		agentToolSrv = &http.Server{Handler: httpapi.AgentToolHandler(handler), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := agentToolSrv.Serve(agentToolLn); err != nil && err != http.ErrServerClosed {
				log.Printf("agent tool gateway: %v", err)
			}
		}()
	}

	// Configure control endpoints and wire the API's Runner transport before
	// claiming work: startup must not execute nodes against an unconfigured engine.
	e.StartMaintenance(claimCtx)
	if executeRuns {
		e.StartClaimLoop(claimCtx)
	}
	stopCLIUpdates := startManagedCLIUpdates(claimCtx, role, executeRuns)
	defer stopCLIUpdates()
	var handedOver atomic.Bool
	stopped := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		if role != "worker" {
			signal.Notify(sig, syscall.SIGHUP)
		}
	wait:
		for received := range sig {
			if received != syscall.SIGHUP {
				log.Printf("shutting down %s on %s: stop claiming, drain local work, then stop HTTP...", role, received)
				break
			}
			log.Printf("%s: handing listener %s over to a new process...", received, mainLn.Addr())
			successor, err := handover(mainLn, handoverReadyTimeout)
			if err != nil {
				log.Printf("handover failed, this process keeps serving: %v", err)
				continue
			}
			handedOver.Store(true)
			srv.SetKeepAlivesEnabled(false)
			_ = mainLn.Close()
			background.HandedOver()
			log.Printf("handover complete: pid %d serves new requests; draining in-flight work", successor.Pid)
			break wait
		}
		go func() {
			for received := range sig {
				log.Printf("ignoring %s while draining", received)
			}
		}()
		stopClaim()
		background.StopClaiming()
		if syncSvc != nil {
			syncSvc.StopMRStateSync()
			syncSvc.StopAutoSync()
		}
		stopCLIUpdates() // Cancel updater subprocesses and join before draining.
		drainTimeout := resolveDrainTimeout()
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), drainTimeout)
		defer cancelDrain()
		if err := e.Drain(drainCtx); err != nil {
			log.Printf("drain timeout with %d active run(s): %v", e.ActiveRunCount(), err)
		} else {
			log.Printf("all local runs drained")
		}
		if background != nil {
			if n := background.Inflight(); n > 0 {
				log.Printf("waiting up to %s for %d in-process task(s) (conversations, clarifications, follow-ups, handoffs)", drainTimeout, n)
			}
			if err := background.WaitInflight(drainCtx); err != nil {
				log.Printf("drain timeout with %d in-process task(s): %v", background.Inflight(), err)
			} else {
				log.Printf("all in-process tasks drained")
			}
		}
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelShutdown()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("http shutdown: %v", err)
		}
		background.Stop()
		if agentToolSrv != nil {
			_ = agentToolSrv.Shutdown(shutdownCtx)
		}
		s.Close()
		close(stopped)
	}()
	displayAddr := srv.Addr
	if strings.HasPrefix(displayAddr, ":") {
		displayAddr = "localhost" + displayAddr
	}
	serve := func() error { return srv.Serve(mainLn) }
	if role != "worker" && *tlsCert != "" && *tlsKey != "" {
		log.Printf("LoopForge %s listening on https://%s", role, displayAddr)
		serve = func() error { return srv.ServeTLS(mainLn, *tlsCert, *tlsKey) }
	} else {
		log.Printf("LoopForge %s listening on http://%s", role, displayAddr)
	}
	signalReady(readyStatus)
	if err := serve(); err != nil && err != http.ErrServerClosed && !handedOver.Load() {
		stopClaim()
		stopCLIUpdates() // log.Fatal bypasses deferred cleanup.
		log.Fatal(err)
	}
	<-stopped
}

// resolveDrainTimeout bounds how long a stopping or handed-over process waits
// for local runs and in-process tasks; long tasks may outlive several deploys.
func resolveDrainTimeout() time.Duration {
	drainTimeout := 120 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("AGENTFLOW_DRAIN_TIMEOUT")); raw != "" {
		if parsed, parseErr := time.ParseDuration(raw); parseErr == nil && parsed > 0 {
			drainTimeout = parsed
		}
	}
	return drainTimeout
}

func startManagedCLIUpdates(ctx context.Context, role string, executeRuns bool) func() {
	root := strings.TrimSpace(os.Getenv("AGENTFLOW_MANAGED_CLI_ROOT"))
	// Never infer a root or opt personal runners/API-only processes into updates.
	if !executeRuns || (role != "worker" && role != "all") || root == "" {
		return func() {}
	}
	return managedcli.Start(ctx, root)
}

func workerHandler(e *engine.Engine) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = fmt.Fprintf(w, `{"status":"ok","role":"worker","activeRuns":%d}`, e.ActiveRunCount())
	})
	mux.HandleFunc("POST /internal/runs/{id}/interrupt", e.HandleRunInterrupt)
	mux.HandleFunc("POST /internal/workspace-checkpoint", e.HandleWorkspaceCheckpoint)
	return mux
}

func resolveRole(override string) (string, error) {
	role := strings.ToLower(strings.TrimSpace(override))
	if role == "" {
		role = strings.ToLower(strings.TrimSpace(os.Getenv("AGENTFLOW_ROLE")))
	}
	if role == "" {
		role = "all"
	}
	switch role {
	case "api", "worker", "all":
		return role, nil
	default:
		return "", fmt.Errorf("invalid role %q (want api, worker, or all)", role)
	}
}

func resolveWorkspaceRoot(override string) (string, error) {
	if root := strings.TrimSpace(override); root != "" {
		return root, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve default sandbox workspace root: %w", err)
	}
	if strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("resolve default sandbox workspace root: user home is empty")
	}
	return filepath.Join(home, ".agentflow", "workspace"), nil
}

func attachPreviewStore(e *engine.Engine, s *repo.Store) {
	cfg := preview.LoadConfig()
	e.SetPreview(preview.NewService(s, preview.NewDBStore(s), cfg))
	if cfg.PublicHost != "" {
		log.Printf("preview gateway path=/preview/{token}/ public=%s://%s", cfg.PublicScheme, cfg.PublicHost)
	} else {
		log.Printf("preview gateway path=/preview/{token}/")
	}
}
