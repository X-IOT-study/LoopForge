# Local-only edition

Run LoopForge as a single-user application on your own machine: one password account, loopback-only listening, and no cloud-side features. Everything — runs, workspaces, credentials, knowledge — stays on this host.

This is a deployment mode of the same binary (`AGENTFLOW_LOCAL_MODE=1`), not a separate build. The full feature list stays available; the cloud capabilities listed in [What is unavailable](#what-is-unavailable) return `403 local_mode_unsupported` instead of silently degrading.

## What it changes

| Area | Platform mode (default) | Local-only edition |
|---|---|---|
| Identity | Gateway / SSO / invite, many accounts | Exactly one account, login name `local`, password sign-in |
| Sign-up | Self-service (configurable) | Closed; the first visit creates the only account |
| Listening | All interfaces | Loopback only (`127.0.0.1` / `::1`) |
| Provider credentials | Personal keys entered in the UI; host CLI state is cleared | Host CLI login state and host env vars first, personal keys as fallback |
| Execution sandbox | Docker sandbox and/or remote runners | Local process sandbox (Docker sandbox in local mode fails with `local_sandbox_unsupported`) |
| Cloud features | Notification bots, platform admin, markets, Tapd sync, shared schedules, cloud runners | Rejected with `403` + `code=local_mode_unsupported` |

## Requirements

- MySQL (the Compose `mysql` service is enough), or an existing `AGENTFLOW_MYSQL_DSN`.
- Node.js 22+ and the agent CLIs you want to use (`codebuddy`, and optionally `codex`, `claude`, …), on the `PATH` of the process that runs the platform.
- A browser on the same machine, or another device that can reach the loopback-bound port through your own tunnel.

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `AGENTFLOW_LOCAL_MODE` | unset (off) | `1` turns on the local-only edition. Read once at startup |
| `AGENTFLOW_LOCAL_ALLOW_REMOTE` | unset (off) | Allow a non-loopback listen address for the host process. Off by default; do not use it to work around container networking (see below) |
| `AGENTFLOW_SESSION_SECRET` | empty | Host process: leave empty, the first start generates `<AGENTFLOW_DATA_DIR>/local/session.key` and reuses it. Compose: required, generate it with `AGENTFLOW_SESSION_SECRET=$(openssl rand -hex 32)` |
| `AGENTFLOW_DATA_DIR` | parent of the workspace root | Where data lives. The session key lands in `<AGENTFLOW_DATA_DIR>/local/` |
| `AGENTFLOW_AUTH_REQUIRED` | forced `true` | Local mode always requires sign-in; a `false` value is overridden with a log line |
| `AGENTFLOW_LOCAL_SANDBOX` | unset | Must stay unset (or `process`). `AGENTFLOW_LOCAL_SANDBOX=docker` makes provider nodes fail with `local_sandbox_unsupported` |
| `AGENTFLOW_LOCAL_EXECUTION` | unset | `false` keeps this host from running nodes (you will then need an external execution machine, which the local edition does not offer) |
| `AGENTFLOW_LOCAL_LOGIN_USER` | unset | Legacy password-less acceptance login. Always disabled in local mode |
| `LOOPFORGE_BIND` | `127.0.0.1` | Compose publish address; the Compose default stays `0.0.0.0` for platform deployments, so local users set it explicitly |
| `AGENTFLOW_HTTP_PORT` | `8090` in the sample env, `8080` otherwise | Platform port |
| `AGENTFLOW_MYSQL_DSN` | sample value | MySQL DSN; the sample points at the Compose MySQL on `127.0.0.1:33315` |

Start from the sample: `cp .env.local.example .env`, then replace the `REPLACE_ME` values.

## Start the platform

Run the platform **as a host process**. Compose is only the database here, not a place to run the platform: compose injects `AGENTFLOW_LOCAL_SANDBOX=docker` into the `platform` service by default, and a container has neither the host's `HOME` nor its CLI logins. Local mode with the Docker driver is rejected explicitly with `local_sandbox_unsupported`, so there is no supported container layout for this edition.

```bash
cp .env.local.example .env        # fill in REPLACE_ME values
docker compose up -d mysql
go build -o .local/bin/agentflow ./cmd/agentflow
./.local/bin/agentflow -addr 127.0.0.1:8090 -workspaces .data/workspaces
```

The host process inherits your shell environment, so provider CLIs you logged into in your terminal are picked up directly. This is the layout the credential rules below describe.

Starting the whole stack with `docker compose up -d` also brings up the `platform` service, which needs `AGENTFLOW_SESSION_SECRET` (guarded by `:?` in `compose.yaml`). With `AGENTFLOW_LOCAL_MODE=1` that service starts but cannot execute a single node (docker driver, no host login state), so local mode uses `docker compose up -d mysql` only.

The process refuses any non-loopback listen address unless `AGENTFLOW_LOCAL_ALLOW_REMOTE=1`; the guard exists to keep a single-user instance unexposed, not to make a container deployment work. Compose's `platform` service defaults to `AGENTFLOW_LOCAL_SANDBOX=docker`, which is exactly why local mode runs the binary on the host instead of that service.

## Seed the catalog

The installer (`scripts/dev.sh`, `scripts/install-schema-extensions.sh`, `scripts/test-container.sh`) applies `migrations/seed/*.sql` for you. The host-process command above does **not**: it only connects and runs the incremental `AutoMigrate` DDL (`repo.Open` never seeds, see `AGENTS.md`). Without the seed the database is structurally fine but empty — no generic templates, no agents, no rules, no sandboxes, no sample project — so SOP lists, the project wizard's default-SOP dropdown and most settings pages render blank.

Apply them once, after `docker compose up -d mysql` and before the first sign-in (each file is `INSERT IGNORE`, so re-running is harmless):

```bash
# From the host, against the Compose MySQL (default published port 33315).
mysql -h 127.0.0.1 -P 33315 -u loopforge -p < migrations/seed/001_public_catalog.sql
mysql -h 127.0.0.1 -P 33315 -u loopforge -p < migrations/seed/002_welcome_project.sql

# Or pipe the files through the container.
docker compose exec -T mysql sh -c 'mysql -u loopforge -p"$MYSQL_PASSWORD"' < migrations/seed/001_public_catalog.sql
docker compose exec -T mysql sh -c 'mysql -u loopforge -p"$MYSQL_PASSWORD"' < migrations/seed/002_welcome_project.sql
```

`001_public_catalog.sql` seeds the four generic delivery templates (`generic-backend-delivery`, `generic-frontend-delivery`, `generic-product-delivery`), the `welcome-workflow`/`issue-fix-production` workflows, one agent, one rule, two sandboxes and one dockerfile. `002_welcome_project.sql` adds the 「LoopForge 体验项目」 project. That project's owner is looked up by login name `admin`; on a local edition the account is `local`, so the project is created without an owner — assign it to yourself in the project settings after signing in.

## First start: set the password

Open `http://127.0.0.1:8090/`. With no account yet, the login page shows a single field — **设置本地管理员密码** / *Set the local administrator password* — and explains that the account and data stay on this machine. On submit the browser calls `POST /api/auth/register` with only a password; the server creates the account with login name `local` and makes it the platform administrator.

After that the page shows **输入本地密码** / *Enter your local password* and calls `POST /api/auth/password`. Register, SSO, invitation and third-party login entries are hidden.

If you submit a password before any account exists, the API answers `409` with `code=local_mode_not_setup` and the page switches back to the set-password form. Sign-up stays closed for good: a second `POST /api/auth/register` is refused with `403 自助注册已关闭，请联系管理员创建账号` (`signupEnabled()` is always false in local mode, `internal/httpapi/auth_register.go:29-36`).

Forgot the password? There is no recovery flow in this edition. Stop the platform and reset the account row in MySQL (table `local_accounts` plus the matching `users` row), then set it again through the first-run form.

## Session key

Empty `AGENTFLOW_SESSION_SECRET` means the process creates its own signing key on first start:

```
<AGENTFLOW_DATA_DIR>/local/session.key     # file mode 0600, directory mode 0700
```

With the default `-workspaces .data/workspaces` the key is `.data/data/local/session.key`. Set `AGENTFLOW_DATA_DIR=.data` to pin it at `.data/local/session.key`.

- The key signs every session cookie. Anyone who can read it can forge sessions for this instance; keep it out of backups you share and out of version control.
- Deleting it signs everyone out (all sessions become invalid) but loses no data.
- Set `AGENTFLOW_SESSION_SECRET` explicitly when you run several instances or manage secrets centrally; the file is then not created.

## Provider credentials

In local mode the host owns the credentials, so the order is **host first, personal key as fallback** (the opposite of platform mode, where host CLI state is cleared and personal keys are the only source).

1. **Host environment variables** inherited by the process:
   - `codebuddy`: `CODEBUDDY_API_KEY`
   - `codex`: `OPENAI_API_KEY` / `CODEX_API_KEY` (plus optional `OPENAI_BASE_URL`)
   - `claude-code`: `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN` / `CLAUDE_CODE_OAUTH_TOKEN` (plus optional `ANTHROPIC_BASE_URL`)
   - `cursor`: `CURSOR_API_KEY` / `CURSOR_AUTH_TOKEN`
   - `antigravity`: `GEMINI_API_KEY` / `GOOGLE_API_KEY` / `AGY_LLM_GATEWAY_API_KEY`
   - `kimi`: `KIMI_API_KEY` / `MOONSHOT_API_KEY`
   - `dsh`: `DEEPSEEK_API_KEY`
2. **Host CLI login state** (`HOME` points at the host home and is mounted read-write, so the CLI can use its own login): `~/.codebuddy/`, `~/.codex/auth.json`, `~/.claude/.credentials.json`, plus the provider's own directory for Cursor/Kimi/DSH. These files are what the CLI itself reads; whether the platform treats the host as usable is decided by the environment variables above, with `~/.codex/auth.json` also serving as an anchor. Platform-written configuration such as MCP, model, permissions or hooks is never treated as "logged in".
3. **Fallback to the personal key** you saved in **个人配置 / Personal settings** when the host is not usable.

Unknown providers (`opencode`, `pi`, `zcode`) use the opposite order: **personal key first**. Without a key the CLI runs with an empty credential env and its own "not logged in" error is surfaced with a hint; the host directory is still mounted so you can log in manually.

**kimi and zcode are not offered in local mode.** Support for their local form is still incomplete (they depend on platform-side model and credential distribution), so `GET /api/executors`, `GET /api/executor-models` and bootstrap ship the other eight CLIs only, and a role or template pinned to them is rejected with `403` / `code=local_mode_unsupported`. Their credential cards are hidden under **个人配置**. The engine also refuses to start a run whose workflow still resolves to one of them.

When nothing is available a node fails with a readable error:

```
local_credentials_missing: 未检测到宿主 <provider> 登录态，请先在终端执行 codex login / claude login /
配置 CODEBUDDY_API_KEY，或在「个人配置」填写 Key
```

Fixes, in order of preference: log in with the CLI in a terminal on the host, export the provider's env var before starting the platform, or save a Key under **个人配置**.

**Claude Code `~/.claude.json`.** On Linux the bwrap sandbox now binds `~/.claude.json` as a file (when it exists and is a regular file), so `claude` can rewrite its OAuth account and project state as usual. On macOS the seatbelt profile only grants directory-level writes, so `~/.claude.json` stays read-only inside a run — a known limitation of this edition on that platform. If `claude` insists on rewriting it there, use an `ANTHROPIC_API_KEY`/`CLAUDE_CODE_OAUTH_TOKEN`, or set `CLAUDE_CONFIG_DIR` to a writable directory that the sandbox binds.

**Sandbox note.** Local mode runs provider nodes through the host process sandbox and needs the host filesystem; the Docker driver is refused with `local_sandbox_unsupported`. Leave `AGENTFLOW_LOCAL_SANDBOX` unset and run the binary on the host, not the Compose `platform` service.

## What is unavailable

These return `403` with `{"error":"该能力在本地模式下不可用","code":"local_mode_unsupported"}`; the matching navigation entries are hidden in the UI, and direct URLs are still rejected by the server:

- Notification bots and delivery channels (`/api/notification-*`, `/api/bot-bindings`)
- Platform administration (`/api/platform/*`, member account list `GET /api/users`)
- SSO / gateway login (`/api/auth/gongfeng`, `/api/auth/exchange`) and password-less local login (`POST /api/auth/local`)
- External API access tokens (`/api/auth/tokens`)
- Asset markets (`/api/mcp-market`, `/api/skillhub/*`)
- Team knowledge publishing (`/api/knowledge/submissions`) and the knowledge MCP proxy (`/runners/knowledge/*`)
- Tapd sync (`/api/sync/*`)
- Shared scheduled tasks (`/api/shared-schedule-tasks`, `POST /api/schedule-jobs/:id/publish`)
- Execution-machine binding codes (`POST /api/runners/bind-codes`)
- Preview subdomain hosting
- The two executor CLIs local mode withdraws, `kimi` and `zcode`: they are absent from the executor list and the model catalog, role/template writes using them return `403` with the code above, and a run that still resolves to one of them fails with `本地模式不支持执行器 <kind>`. The other eight (`codebuddy`, `codex`, `claude-code`, `opencode`, `cursor`, `agy`, `pi`, `dsh`) work as usual.

Runs, agents/rules/skills/MCP servers/executors/dockerfiles/environments/sandboxes/workflows, issues, project reads, local schedule jobs, assistant conversations, knowledge reads, and run previews all keep working. The auth endpoints and their local-mode error codes are documented in the change's API document; see also the [HTTP API reference](api/README.md) for the shared surface.

## Roll back to platform mode

1. Remove `AGENTFLOW_LOCAL_MODE` from `.env` (or set it to `0`), and remove `AGENTFLOW_LOCAL_ALLOW_REMOTE`.
2. Decide what to do with the account. The `local` account stays in MySQL and becomes an ordinary password account in platform mode; you can keep signing in with it, or create a named administrator first (**Platform administration → Member accounts** is available again in platform mode) and then delete or rename it.
3. Restart the process. Cloud features, self-service sign-up, remote runners and the Docker sandbox become available again; the session key file stays where it was, but platform mode requires `AGENTFLOW_SESSION_SECRET` to be set explicitly (the auto-bootstrap only runs in local mode).
4. If you relied on host CLI logins, move those credentials into **个人配置**: platform mode clears host credential env vars and uses personal keys only.

## Limitations

- One account, no roles, no account sharing. Sessions are per browser; there is no token API for scripts.
- No cloud execution: long runs occupy this machine, and the process must stay alive.
- Credentials follow the host user; running the platform as a different user loses the CLI logins, and running it as the Compose container is not supported at all.
- Not for multi-user or internet-facing use. The loopback default and the single account exist precisely to keep it local. If you must expose it, put it behind your own authenticated tunnel and understand that `AGENTFLOW_LOCAL_ALLOW_REMOTE=1` removes the loopback guard without adding authentication beyond the password.
