# AGENTS.md

Go 1.26+ proxy server providing OpenAI/Gemini/Claude/Codex compatible APIs with OAuth and round-robin load balancing.

## Repository
- Fork origin: https://github.com/yuanwangz/CLIProxyAPI.git
- Upstream GitHub: https://github.com/router-for-me/CLIProxyAPI

## Fork Maintenance Notes
- This file is the canonical agent note for this repository. If a request says `AGENT.md`, update `AGENTS.md` unless the user explicitly asks for a separate file.
- This repository is a maintained fork of `router-for-me/CLIProxyAPI`. Keep the fork close to upstream and prefer upstream-compatible changes so future upstream merges stay small.
- Local backend path: `/Users/yuanwan/CLIProxyAPI`
- Related frontend path: `/Users/yuanwan/Cli-Proxy-API-Management-Center`
- Frontend fork origin: https://github.com/yuanwangz/Cli-Proxy-API-Management-Center
- Frontend upstream GitHub: https://github.com/router-for-me/Cli-Proxy-API-Management-Center
- After any major fork-specific behavior change, upstream merge, frontend/backend contract change, or release-process change, update this `AGENTS.md` in the same work session so future agents do not need the same context repeated.

### Upstream Compatibility Rules
- Treat upstream files as shared ownership. Prefer narrow, additive patches over broad rewrites, wholesale formatting, or moving code between packages.
- Keep fork-only behavior behind clearly named packages, helper functions, feature flags, config fields, or adapter layers where practical. Avoid interleaving large fork-specific blocks directly inside upstream hot paths.
- When a fork feature must hook into an upstream-owned file, keep the hook minimal and delegate the real logic to a fork-owned package. This reduces conflict surface during upstream sync.
- Current project adjustment principle: keep feature changes low-intrusion, incremental, and fork-friendly. Prefer existing management APIs, persistence packages, and frontend integration points before touching request execution, scheduler, conductor, or provider executor hot paths.
- Do not delete or "simplify away" fork-specific features when resolving upstream conflicts. First identify whether upstream removed a feature globally or only refactored the surrounding code.
- During upstream merges, preserve upstream improvements while restoring local fork features. For conflicts, compare both sides, identify the behavioral intent, then re-apply the fork feature in the smallest compatible shape.
- Avoid repository-wide format churn. Run `gofmt` only after Go changes, and keep frontend formatting scoped to touched files.
- Add or keep tests around fork-owned behavior so merge regressions are caught before deployment.

### Recent Upstream Sync Notes
- 2026-05-29: merged upstream `main` through `df0176a1`. Conflict resolutions kept fork-owned auth status/cooldown visibility, Codex quota persistence from WebSocket handshakes/events, image fallback usage event publishing, and Antigravity credits hints while preserving upstream service-tier/TTFT usage tracking, websocket auth metadata, request logging, signature validation extraction, and model registry updates. Keep Antigravity credits `loadCodeAssist` defaulting to the daily endpoint unless a custom `base_url` is configured, and keep bootstrap retry auth-selection errors surfaced as `auth_unavailable` after an upstream bootstrap failure.

### Fork-Owned Backend Features
- `internal/usagepersist`: fork-owned SQLite persistence for usage statistics, request event history, request status codes, and auth/model cooldown state. Upstream may not contain this package; keep imports under the module path `github.com/router-for-me/CLIProxyAPI/v7/...` because the Go module path follows upstream even in this fork.
- Usage persistence integration points include server startup, management APIs, usage event building, status-code persistence for provider failures, and auth cooldown restore/migration.
- `internal/imagesfallback` and OpenAI image fallback handlers: fork-owned image fallback path that can route failed image generation/edit requests through Codex OAuth-capable auths.
- Management/usage analytics contract: backend responses should continue to provide enough detail for the frontend usage page, including persisted events, status codes when known, credential identifiers, and cooldown summaries.
- Codex quota visibility is persisted opportunistically from already-observed Codex HTTP rate-limit headers and WebSocket `codex.rate_limits` events via `internal/codexquota`; keep this persistence best-effort and asynchronous, and do not add quota polling or extra provider calls to normal request execution for this management view.
- Provider-neutral quota reset routing hints are stored in `usage.sqlite` through `quota_routing_hints` and loaded into auth scheduler memory on startup/register. Existing background credential refresh/maintenance may collect provider quota snapshots for Codex, Claude, Gemini CLI, Antigravity, Kimi, and xAI when their OAuth quota endpoints expose reset metadata. The scheduler may prefer credentials with the nearest known quota reset within the same manual priority, but request execution must remain best-effort and non-blocking: no hot-path database reads, no request-path quota polling, and no provider-specific probes from normal proxy requests. Keep nearest-reset ordering consistent for scheduler fast paths and built-in legacy selector fallbacks, and publish Codex management snapshot writes into in-memory routing hints immediately after persistence.
- Cooldown persistence compatibility matters. Preserve legacy auth index matching and migration behavior so existing `usage.sqlite` databases remain readable after syncs.

### Frontend Coupling Notes
- The management UI is a separate React/Vite project at `/Users/yuanwan/Cli-Proxy-API-Management-Center`.
- Its upstream repository is `router-for-me/Cli-Proxy-API-Management-Center`; the local fork remote is `yuanwangz/Cli-Proxy-API-Management-Center`.
- Backend management API changes that affect usage analytics, credential health, cooldown counts, auth file metadata, or release asset loading must be coordinated with the frontend project.
- The frontend release workflow is tag-driven: pushing a `v*` tag builds and publishes `dist/management.html`. If the user asks for a same-version frontend release, do not change `package.json` version; update the release/tag flow deliberately and verify the workflow result.

## Commands
```bash
gofmt -w . # Format (required after Go changes)
go build -o cli-proxy-api ./cmd/server # Build
go run ./cmd/server # Run dev server
go test ./... # Run all tests
go test -v -run TestName ./path/to/pkg # Run single test
go build -o test-output ./cmd/server && rm test-output # Verify compile (REQUIRED after changes)
```
- Common flags: `--config <path>`, `--tui`, `--standalone`, `--local-model`, `--no-browser`, `--oauth-callback-port <port>`

## Config
- Default config: `config.yaml` (template: `config.example.yaml`)
- `.env` is auto-loaded from the working directory
- Auth material defaults under `auths/`
- Storage backends: file-based default; optional Postgres/git/object store (`PGSTORE_*`, `GITSTORE_*`, `OBJECTSTORE_*`)

## Architecture
- `cmd/server/` — Server entrypoint
- `internal/api/` — Gin HTTP API (routes, middleware, modules)
- `internal/api/modules/amp/` — Amp integration (Amp-style routes + reverse proxy)
- `internal/thinking/` — Main thinking/reasoning pipeline. `ApplyThinking()` (apply.go) parses suffixes (`suffix.go`, suffix overrides body), normalizes config to canonical `ThinkingConfig` (`types.go`), normalizes and validates centrally (`validate.go`/`convert.go`), then applies provider-specific output via `ProviderApplier`. Do not break this "canonical representation → per-provider translation" architecture.
- `internal/runtime/executor/` — Per-provider runtime executors (incl. Codex WebSocket)
- `internal/translator/` — Provider protocol translators (and shared `common`)
- `internal/registry/` — Model registry + remote updater (`StartModelsUpdater`); `--local-model` disables remote updates
- `internal/store/` — Storage implementations and secret resolution
- `internal/managementasset/` — Config snapshots and management assets
- `internal/cache/` — Request signature caching
- `internal/watcher/` — Config hot-reload and watchers
- `internal/wsrelay/` — WebSocket relay sessions
- `internal/usage/` — Usage and token accounting
- `internal/tui/` — Bubbletea terminal UI (`--tui`, `--standalone`)
- `sdk/cliproxy/` — Embeddable SDK entry (service/builder/watchers/pipeline)
- `test/` — Cross-module integration tests

## Code Conventions
- Keep changes small and simple (KISS)
- Comments in English only
- If editing code that already contains non-English comments, translate them to English (don’t add new non-English comments)
- For user-visible strings, keep the existing language used in that file/area
- New Markdown docs should be in English unless the file is explicitly language-specific (e.g. `README_CN.md`)
- As a rule, do not make standalone changes to `internal/translator/`. You may modify it only as part of broader changes elsewhere.
- If a task requires changing only `internal/translator/`, run `gh repo view --json viewerPermission -q .viewerPermission` to confirm you have `WRITE`, `MAINTAIN`, or `ADMIN`. If you do, you may proceed; otherwise, file a GitHub issue including the goal, rationale, and the intended implementation code, then stop further work.
- `internal/runtime/executor/` should contain executors and their unit tests only. Place any helper/supporting files under `internal/runtime/executor/helps/`.
- Follow `gofmt`; keep imports goimports-style; wrap errors with context where helpful
- Do not use `log.Fatal`/`log.Fatalf` (terminates the process); prefer returning errors and logging via logrus
- Shadowed variables: use method suffix (`errStart := server.Start()`)
- Wrap defer errors: `defer func() { if err := f.Close(); err != nil { log.Errorf(...) } }()`
- Use logrus structured logging; avoid leaking secrets/tokens in logs
- Avoid panics in HTTP handlers; prefer logged errors and meaningful HTTP status codes
- Timeouts are allowed only during credential acquisition; after an upstream connection is established, do not set timeouts for any subsequent network behavior. Intentional exceptions that must remain allowed are the Codex websocket liveness deadlines in `internal/runtime/executor/codex_websockets_executor.go`, the wsrelay session deadlines in `internal/wsrelay/session.go`, the management APICall timeout in `internal/api/handlers/management/api_tools.go`, and the `cmd/fetch_antigravity_models` utility timeouts
