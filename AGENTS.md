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
- 2026-06-17: merged backend upstream `main` through `644ba74b` (`v7.2.14`) and frontend upstream through `e95cc2b` (`v1.16.8`). Conflict resolutions kept fork-owned auth-file archival, AI provider cooldown visibility/clear-cooldown management behavior, Codex quota persistence from WebSocket `codex.rate_limits`, quota snapshot refresh metadata, and legacy plugin executor empty-format compatibility while preserving upstream config API-key exclusion management, pluginhost model routing/stream callbacks, xAI websocket/executor updates, model registry updates, and the upstream Amp/Ampcode removal. Frontend conflict resolution removed Ampcode UI/API surfaces, kept provider cooldown details/actions on `/api-key-usage`, and adapted the quota page to upstream Antigravity `retrieveUserQuotaSummary` group/bucket payloads plus Gemini CLI grouped quota buckets.
- 2026-06-14: merged upstream `main` through `83c53d05` (`v7.1.75`). Conflict resolutions kept fork-owned AI provider cooldown visibility/clear-cooldown management behavior, GPT 5.5/static catalog tests, and scheduler fill-first sticky nearest-reset behavior while preserving upstream plugin store latest-release/install/delete APIs, Antigravity native web search routing, plugin CORS/config fixes, translator mid-conversation system consolidation, and model registry updates. Plugin executor adapters should continue to accept legacy empty input/output format declarations by defaulting to OpenAI, but follow upstream's split input/response format contract: `RequestToFormat` resolves the executor input format, `ResponseFormat` controls downstream response translation, and plugin executor `SourceFormat` reflects the selected native input format for the executor call.
- 2026-06-08: merged upstream `main` through `c989cdd9`. Conflict resolutions kept fork-owned credential creation timestamps, auth status/cooldown persistence, Codex quota persistence from already-observed HTTP/WebSocket metadata, and quota cooldown restore after model registration while preserving upstream pluginhost, Redis queue, request logging, Cloudflare challenge retry, Codex reasoning replay cache, Home auth retry, safemode, FreeBSD release workflow, and model registry updates. Keep plugin executor adapters backward-compatible with empty format declarations by defaulting to OpenAI payload format while preserving the original `SourceFormat` metadata; this keeps upstream pluginhost panic/fuse tests green and avoids forcing legacy plugin executors to declare formats immediately.
- 2026-06-01: merged upstream `main` through `05b97247` (`v7.1.37`). Conflict resolution kept fork GPT 5.5/static model catalog tests while adding upstream xAI `grok-imagine-video-1.5-preview` coverage. Upstream introduced Codex identity obfuscation refinements, signature replay compatibility checks, Home app log forwarding, and model registry updates. Keep `setHeaderCasePreserved` updating an existing case-equivalent key instead of recreating it, so HTTP Codex requests retain `Session_id` while WebSocket requests retain lowercase `session_id`.
- 2026-05-29: merged upstream `main` through `df0176a1`. Conflict resolutions kept fork-owned auth status/cooldown visibility, Codex quota persistence from WebSocket handshakes/events, image fallback usage event publishing, and Antigravity credits hints while preserving upstream service-tier/TTFT usage tracking, websocket auth metadata, request logging, signature validation extraction, and model registry updates. Keep Antigravity credits `loadCodeAssist` defaulting to the daily endpoint unless a custom `base_url` is configured, and keep bootstrap retry auth-selection errors surfaced as `auth_unavailable` after an upstream bootstrap failure.

### Fork-Owned Backend Features
- `internal/usagepersist`: fork-owned SQLite persistence for usage statistics, request event history, request status codes, and auth/model cooldown state. Upstream may not contain this package; keep imports under the module path `github.com/router-for-me/CLIProxyAPI/v7/...` because the Go module path follows upstream even in this fork.
- Usage persistence integration points include server startup, management APIs, usage event building, status-code persistence for provider failures, and auth cooldown restore/migration.
- `internal/imagesfallback` and OpenAI image fallback handlers: fork-owned image fallback path that can route failed image generation/edit requests through Codex OAuth-capable auths. Default `gpt-image-2` `/v1/images/generations` and `/v1/images/edits` requests should try routed image providers first, then fall back to Codex OAuth/ChatGPT image fallback on retryable failures when a Codex OAuth auth exists; keep the primary routed phase free-auth-disallowed so free accounts are reserved for the fallback path. ChatGPT web image fallback may use `CLI_PROXY_WEB_IMAGE_PROXY_URL` or `WEB_IMAGE_PROXY_URL` as a dedicated proxy before falling back to global `proxy-url`; per-auth `proxy-url` still has priority and may use `direct`/`none` to bypass proxies.
- Management/usage analytics contract: backend responses should continue to provide enough detail for the frontend usage page, including persisted events, status codes when known, credential identifiers, and cooldown summaries.
- AI providers page channel health uses `/v0/management/api-key-usage`; keep its response shape as `provider -> "base_url|api_key" -> success/failed/recent_requests` with additive auth status fields. The endpoint should restore counts and 20-bucket status bars from persisted `usage_events` by live `auth_index` when available, then fall back to runtime `Auth.Success`/`Auth.Failed` and in-memory recent buckets when no persisted rows exist. Provider rows may show runtime API-key cooldown or model suspension state from the same response, and `POST /v0/management/api-key-usage/clear-cooldown` is the management action for manually clearing operator-retryable API-key availability blocks, including quota/429 cooldowns and model-level 404/not_found or model_not_supported pauses after the operator fixes configuration. It must not clear disabled or 401 unauthorized credentials.
- Auth file health contract: only upstream/provider 401s should mark credentials disabled or contribute to the auth-files 401 classification. Management UI to backend 401s must never be treated as credential failures. Generic management `/api-call` 401 handling must require a substituted credential token and a trusted provider HTTPS host before disabling an auth.
- Auth file creation timestamps are persisted in auth JSON metadata under `credential_created_at`. Do not use provider payload `created_at` for credential creation time because many provider response formats already define that field. OAuth login/re-login and management upload overwrite create a new credential timestamp; token refresh and disabled-state persistence preserve the existing `credential_created_at`.
- Auth file archival is a manual operator state persisted in auth JSON metadata under `archived`. Keep it independent from `disabled`: archiving a 401-disabled credential must preserve `disabled`, `last_error`, and the auth-files `status_code` evidence, and unarchiving must not automatically re-enable or clear a 401. Archived credentials should be hidden from normal auth-files views in the management UI and skipped by routing, scheduler ready sets, auto/manual refresh, model registration, quota/inspection refreshes, websocket availability, image fallback, and model-fetch utilities until manually unarchived.
- Codex quota visibility is persisted opportunistically from already-observed Codex HTTP rate-limit headers and WebSocket `codex.rate_limits` events via `internal/codexquota`; keep this persistence best-effort and asynchronous, and do not add quota polling or extra provider calls to normal request execution for this management view.
- Quota page token usage is per inferred quota refresh cycle when a stored quota snapshot exposes cycle/reset metadata; credentials without usable cycle metadata fall back to lifetime aggregation. Successful management quota snapshots that prove available capacity may clear stale persisted/runtime quota or 429 cooldowns, but never clear disabled credentials or HTTP 401 unauthorized auth state.
- Provider-neutral quota reset routing hints are stored in `usage.sqlite` through `quota_routing_hints` and loaded into auth scheduler memory on startup/register. Existing background credential refresh/maintenance may collect provider quota snapshots for Codex, Claude, Gemini CLI, Antigravity, Kimi, and xAI when their OAuth quota endpoints expose reset metadata. The scheduler may prefer credentials with the nearest known quota reset within the same manual priority, but request execution must remain best-effort and non-blocking: no hot-path database reads, no request-path quota polling, and no provider-specific probes from normal proxy requests. Keep nearest-reset ordering consistent for scheduler fast paths and built-in legacy selector fallbacks, and publish Codex management snapshot writes into in-memory routing hints immediately after persistence. Management snapshot writes that arrive with only `auth_index` must resolve and persist the live `auth_id` before publishing routing hints. Scheduler ready buckets must detect relevant in-memory hint changes and reorder affected shards without resetting unrelated provider/model queues. Fill-first remains sticky: nearest-reset ordering chooses the first ready credential, then the same credential should continue to be selected until it becomes unavailable, disabled, cooling down, or excluded from the current request.
- Cooldown persistence compatibility matters. Preserve legacy auth index matching and migration behavior so existing `usage.sqlite` databases remain readable after syncs.

### Frontend Coupling Notes
- The management UI is a separate React/Vite project at `/Users/yuanwan/Cli-Proxy-API-Management-Center`.
- Its upstream repository is `router-for-me/Cli-Proxy-API-Management-Center`; the local fork remote is `yuanwangz/Cli-Proxy-API-Management-Center`.
- Backend management API changes that affect usage analytics, credential health, cooldown counts, auth file metadata, or release asset loading must be coordinated with the frontend project.
- Auth file list responses expose `created_at` as the credential creation time sourced from `credential_created_at`; the management UI should display it without adding wide table columns that break dense credential lists.
- Auth file list responses expose `archived` and `status: "archived"` for sealed credentials. The management UI defaults to showing unarchived credentials, provides archived/all filters, and uses `PATCH /v0/management/auth-files/status` with `{ name, archived }` for manual archive/unarchive actions.
- Quota page availability filters may use a successful stored/refreshed quota snapshot to override stale quota-limited auth-file state, but disabled and 401 unauthorized credentials must remain unavailable until re-enabled or reauthorized.
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
