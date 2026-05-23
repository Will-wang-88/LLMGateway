# LLM Gateway

A transparent, enterprise-grade, OpenAI-compatible LLM Gateway.
Acts as a single entry point in front of one or more OpenAI-compatible backend
servers (vLLM, SGLang, TGI, LMDeploy, llama.cpp server, Ollama, custom) and
provides routing, load balancing, API-key authentication, rate limiting,
metrics, and an admin API on top.

## Design principles

- **OpenAI-compatible.** Speaks `/v1/models` and `/v1/chat/completions` (plus
  `/v1/completions`, `/v1/embeddings`, `/v1/responses`) so existing clients,
  SDKs, IDE plugins and agents (LangChain, LlamaIndex, Continue, Cline, Cursor,
  etc.) just work.
- **Transparent passthrough.** Unknown request fields are forwarded to the
  backend unchanged. Backend response fields - including `reasoning_content`,
  `reasoning_tokens`, vendor extensions and tool calls - are returned to the
  client unchanged. The gateway does **not** rewrite prompts, strip fields, or
  attempt to be "smarter than the backend".
- **No auto model fallback.** If `model=foo` has no healthy backend, the gateway
  returns `503 no_healthy_backend`. It never silently swaps to a different model.
- **Streaming first.** SSE chunks are forwarded byte-for-byte, including
  reasoning-stream deltas. TTFT and idle timeouts are tracked per backend.
- **Same-model load balancing.** Multiple backends can declare the same model
  name; the gateway picks one per request (weighted round-robin / least
  connections / round-robin / random) restricted to healthy and within-capacity
  backends.

See `spec.md` (in the project description) for the full design contract.

## Quick start

```bash
# Build
go build -o llmgateway ./cmd/gateway

# Run a fake OpenAI-compatible backend for smoke testing
go run examples/fake_backend.go --port 9001 --model llama-3.1-70b &

# Start the gateway (sample config points to localhost:8000-8002)
LLMGATEWAY_CONFIG=config/gateway.yaml ./llmgateway
```

Call it as if it were the OpenAI API:

```bash
curl -s http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-prod-team-a-CHANGE-ME" \
  -H "Content-Type: application/json" \
  -d '{
    "model":"llama-3.1-70b",
    "messages":[{"role":"user","content":"hello"}],
    "stream":false,
    "reasoning_effort":"high",
    "thinking_budget":4096
  }'
```

OpenAI SDK example:

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-prod-team-a-CHANGE-ME")
resp = client.chat.completions.create(
    model="llama-3.1-70b",
    messages=[{"role": "user", "content": "hello"}],
    extra_body={"reasoning_effort": "high", "thinking_budget": 4096},
)
```

## Running with Docker

```bash
docker build -f docker/Dockerfile -t llmgateway:dev .
docker run --rm -p 8080:8080 \
  -e LLMGATEWAY_HASH_SECRET=<your-secret> \
  -e LLMGATEWAY_ADMIN_TOKEN=<admin-token> \
  -v $(pwd)/config/gateway.yaml:/app/gateway.yaml:ro \
  llmgateway:dev

# or
docker compose up --build
```

## Configuration

The gateway is configured via a YAML file pointed to by `LLMGATEWAY_CONFIG`
(default `config/gateway.yaml`). See `config/gateway.yaml` for the full
schema. Key sections:

- `server`, `auth`, `routing`, `health_check`, `rate_limit`, `logging`,
  `metrics`, `admin` - global settings.
- `backends` - list of upstream OpenAI-compatible backends with the models
  they support, weight, concurrency limit, timeout, etc.
- `models` - optional model registry. `capability_mode: passthrough` (default)
  performs no schema validation on requests.
- `model_aliases` - alias external model names to internal names.
- `api_keys` - tenant API keys with per-key allowed models, rate limits,
  delay, logging policy, etc.

Secrets can be supplied via environment variables instead of the YAML file:

```
LLMGATEWAY_HASH_SECRET=...     # HMAC secret used to hash API keys
LLMGATEWAY_ADMIN_TOKEN=...     # Bearer token for /admin/*
LLMGATEWAY_LISTEN=0.0.0.0:8080 # Override host:port
LLMGATEWAY_CONFIG=/path/to/gateway.yaml
```

## API surface

Client-facing (OpenAI-compatible, requires Bearer API key):

| Method | Path                       | Notes                                    |
|--------|----------------------------|------------------------------------------|
| GET    | `/v1/models`               | Catalog of allowed models for this key   |
| POST   | `/v1/chat/completions`     | Streaming and non-streaming, passthrough |
| POST   | `/v1/completions`          | Passthrough                              |
| POST   | `/v1/embeddings`           | Passthrough                              |
| POST   | `/v1/responses`            | Passthrough (Responses API)              |

Operational:

| Method | Path        | Notes                              |
|--------|-------------|------------------------------------|
| GET    | `/healthz`  | Liveness                           |
| GET    | `/readyz`   | At least one healthy backend       |
| GET    | `/metrics`  | Prometheus exposition              |

Admin (Bearer `admin.bind_token` or HTTP basic auth):

| Method | Path                                       |
|--------|--------------------------------------------|
| POST   | `/admin/auth/login`                        |
| POST   | `/admin/auth/logout`                       |
| GET    | `/admin/backends`                          |
| POST   | `/admin/backends`                          |
| GET    | `/admin/backends/{id}`                     |
| PATCH  | `/admin/backends/{id}`                     |
| DELETE | `/admin/backends/{id}`                     |
| POST   | `/admin/backends/{id}/enable`              |
| POST   | `/admin/backends/{id}/disable`             |
| POST   | `/admin/backends/{id}/health-check`        |
| POST   | `/admin/backends/{id}/maintenance`         |
| GET    | `/admin/models`                            |
| POST   | `/admin/models`                            |
| GET    | `/admin/models/{name}`                     |
| PATCH  | `/admin/models/{name}`                     |
| DELETE | `/admin/models/{name}`                     |
| GET    | `/admin/model-aliases`                     |
| POST   | `/admin/model-aliases`                     |
| PATCH  | `/admin/model-aliases/{alias}`             |
| DELETE | `/admin/model-aliases/{alias}`             |
| GET    | `/admin/api-keys`                          |
| POST   | `/admin/api-keys`                          |
| GET    | `/admin/api-keys/{id}`                     |
| PATCH  | `/admin/api-keys/{id}`                     |
| DELETE | `/admin/api-keys/{id}`                     |
| POST   | `/admin/api-keys/{id}/enable`              |
| POST   | `/admin/api-keys/{id}/disable`             |
| POST   | `/admin/api-keys/{id}/rotate`              |
| GET    | `/admin/api-keys/{id}/usage`               |
| GET    | `/admin/stats/overview`                    |
| GET    | `/admin/stats/range`                       |
| GET    | `/admin/stats/models`                      |
| GET    | `/admin/stats/backends`                    |
| GET    | `/admin/stats/api-keys`                    |
| GET    | `/admin/metrics`                           |
| GET    | `/admin/logs`                              |
| GET    | `/admin/audit`                             |
| GET    | `/admin/users`                             |
| POST   | `/admin/users`                             |
| DELETE | `/admin/users/{username}`                  |
| GET    | `/admin/notifications/status`              |
| GET    | `/admin/me`                                |
| GET    | `/admin/settings`                          |

## Error contract

Standard OpenAI-compatible error envelope:

```json
{ "error": { "message": "...", "type": "...", "code": "..." } }
```

Gateway-specific codes:

| HTTP | code                       | Meaning                                |
|------|----------------------------|----------------------------------------|
| 400  | `invalid_json`             | Malformed JSON                         |
| 400  | `missing_model`            | Missing required `model` field         |
| 400  | `invalid_body`             | Body read failure                      |
| 401  | `invalid_api_key`          | Missing / wrong / disabled API key     |
| 403  | `model_not_allowed`        | Key not permitted to use that model    |
| 404  | `model_not_found`          | No backend declares that model         |
| 413  | `payload_too_large`        | Body exceeds `request_body_limit_mb`   |
| 429  | `rate_limit_exceeded`      | Per-key request rate limit             |
| 429  | `token_rate_limit_exceeded`| Per-key token rate limit               |
| 429  | `concurrent_limit`         | Per-key concurrent-request limit       |
| 503  | `no_healthy_backend`       | No healthy backend for that model      |
| 503  | `backend_at_capacity`      | All matching backends saturated        |
| 504  | `backend_timeout`          | Upstream took too long                 |

Backend errors are forwarded with their original status code and body so that
clients see the original error semantics (e.g. invalid sampling parameters).

## Tests

```bash
go test ./...
```

The test suite covers the transparency contract: unknown request fields are
forwarded, `reasoning_content` and vendor response fields are preserved,
streaming SSE chunks pass through byte-for-byte, no model fallback happens,
permissions and load balancing behave correctly.

## Project layout

```
cmd/gateway/         entry point
internal/config/     YAML config & defaults
internal/store/      in-memory backend / model / api-key registry
internal/auth/       API key middleware
internal/backend/    health checker
internal/balancer/   load-balancing policies
internal/proxy/      request and SSE passthrough
internal/ratelimit/  in-memory rate and concurrency limiters
internal/handlers/   /v1/* request handlers
internal/admin/      /admin/* admin API
internal/metrics/    Prometheus collectors
internal/logging/    structured JSON logger
examples/            fake_backend.go - smoke-test backend
config/              sample gateway.yaml
docker/              Dockerfile
```

## Web dashboard

A built-in dashboard is served at `/ui/` (no external server required - the
HTML/CSS/JS is embedded into the binary). Pages:

- **Overview** - QPS, latency, backend health, error rate at a glance.
- **Models** - registry + alias management.
- **Backends** - add / enable / disable / health-check / remove backends.
- **API Keys** - create keys (shown once), per-key allowed models, rate-limits,
  quota, delay, usage drill-down.
- **Logs** - query persistent request logs by model / backend / key / status.
- **Analytics** - 24h top models / backends / API keys.
- **Audit** - admin action log.
- **Users** - manage RBAC users.

Authentication uses either the admin bearer token (`bind_token`) or HTTP basic
auth against `admin_users` (SHA-256 hashed in memory).

## Persistent storage

`storage.driver` selects the request_logs / audit_logs backend:

| driver   | notes                                                            |
|----------|------------------------------------------------------------------|
| memory   | bounded ring buffer (default, no persistence)                    |
| sqlite   | local file via pure-Go `modernc.org/sqlite` (WAL mode, no CGO)   |
| postgres | shared store via `pgx`; required for multi-replica deployments   |

Older records are purged after `storage.log_retention_days`. The Postgres
DSN can come from `storage.dsn` or the `LLMGATEWAY_PG_DSN` env var.

## Rate-limit / quota backend

`rate_limit.backend` selects where per-key counters live:

| backend | notes                                                              |
|---------|--------------------------------------------------------------------|
| memory  | process-local (default). Each replica enforces its own counters.   |
| redis   | shared via `rate_limit.redis_url`. Required for multi-replica HA.  |

> **Multi-replica deployments must use `storage.driver=postgres` AND
> `rate_limit.backend=redis`**. Otherwise each replica will have its own
> rate-limit counters and its own logs. Helm `values.yaml` defaults to
> `replicaCount: 1` for this reason.

## Quota and request queue

- `api_keys[].quota` enforces per-key daily / monthly request and token caps.
  Counters reset at UTC day / month boundaries.
- `queue.enabled` turns on a per-model wait list. Requests beyond
  `per_model_limit` block up to `queue_timeout_ms`, then return `429`. Beyond
  `max_queue_size` they immediately return `queue_full`.

## RBAC and audit log

Configure admin users via the `admin_users` section. Roles:

| role         | permissions                                                |
|--------------|------------------------------------------------------------|
| super_admin  | everything                                                 |
| admin        | everything except `manage_users`                           |
| operator     | read, backend enable/disable, view logs                    |
| viewer       | read-only + view logs                                      |
| auditor      | read + view logs + view audit                              |

Every mutating admin action emits an entry to the `audit_logs` table:
`backend.create`, `backend.update`, `backend.delete`, `backend.enable`,
`backend.disable`, `model.upsert`, `model.delete`, `alias.upsert`,
`alias.delete`, `api_key.create`, `api_key.delete`, `user.create`,
`user.delete`. Query via `GET /admin/audit` or the Audit page.

## Additional routing policies

`models[].routing_policy` (or `routing.default_policy`):

- `weighted_round_robin` (default)
- `round_robin`
- `least_connections`
- `least_latency` - prefers backends with the lowest recent health-probe latency
- `random`
- `hash` - hash of API key id -> deterministic backend
- `sticky` - first request from an API key pins to one backend for the model

## Audio endpoints

`/v1/audio/transcriptions`, `/v1/audio/translations` and `/v1/audio/speech` are
proxied with `multipart/form-data` passthrough. The gateway reads the `model`
field, applies the same auth / permission / load-balancing rules, then
re-emits the multipart body to the chosen backend.

## OpenTelemetry tracing

When `tracing.enabled: true` and an OTLP HTTP collector is configured, the
gateway emits span batches every 5 seconds to `tracing.endpoint + /v1/traces`.
A span named `gateway.forward` is created per `/v1/*` request with
`model`, `internal_model`, `backend_id`, `stream`, `routing_policy`,
`status_code`, `latency_ms`, `ttft_ms`, and `error` attributes (when
applicable). The exporter is best-effort: send failures are logged and
dropped without affecting request handling.

## Backend status notifications

When `notifications.email.enabled: true`, backend status transitions are
delivered via SMTP. Configure the recipient list with
`notifications.email.to` and choose which transitions to alert on with
`notify_on` (default: `backend_degraded`, `backend_unhealthy`,
`backend_recovered`). A per-backend, per-event cooldown
(`notifications.email.cooldown_ms`, default 5 min) prevents flapping
backends from flooding inboxes. Sends never block the health-check loop
and recent send results are exposed at `GET /admin/notifications/status`.

## Helm chart

`deploy/helm/llmgateway` provides a chart with:

- Deployment + Service (+ optional Ingress)
- ConfigMap built from the in-line `config` value
- Secret-backed env vars (`hashSecret`, `adminToken`) via `existingSecret`
- HPA, ServiceMonitor, PVC (for SQLite), nonroot pod security context

Install:

```bash
helm install llmgateway ./deploy/helm/llmgateway \
  --set existingSecret=llmgateway-secrets \
  --set image.tag=latest
```

## Implementation status

| Area | Status |
|---|---|
| OpenAI-compatible passthrough (JSON, SSE, audio multipart) | implemented |
| Health-aware routing (degraded backends excluded by default) | implemented |
| API-key auth with Bearer prefix enforcement | implemented |
| API-key allow/deny model lists honoring alias resolution | implemented |
| API-key allow/deny client IP lists (IPv4/IPv6/CIDR) + trusted-proxy XFF | implemented |
| Disabled-model registry as routing kill switch | implemented |
| Declared / strict capability mode (vision / tool_call / thinking) | implemented |
| Per-minute + per-day request / token rate limits, monthly quota | implemented |
| Token-quota fallback when backend omits `usage` | implemented (conservative estimate) |
| Persistent request_logs / audit_logs with `client_ip` | implemented (memory / sqlite / postgres) |
| Redis-backed rate limit / quota for multi-replica HA | implemented |
| Per-backend health probe scheduler with http/tcp probe types + method/body | implemented |
| SMTP notifications for backend status transitions with cooldown/dedupe | implemented |
| Admin API: backends / models / aliases / api-keys / stats / metrics / logs / audit | implemented |
| RBAC, audit log, session token login (no password caching) | implemented |
| Prometheus metrics with `routing_policy`, separate timeout/quota counters | implemented |
| OpenTelemetry spans per request (`gateway.forward`) | implemented |
| Docker (Go 1.25), `--healthcheck` flag, compose healthcheck via /healthz | implemented |
| Web dashboard: backends / models / api-keys (CRUD + rotate + IP lists) / logs filters / analytics / audit / users / settings | implemented |
| Helm chart with PVC + ServiceMonitor + HPA | implemented (replicaCount=1 unless postgres+redis) |

## Roadmap

- OIDC / SSO for the dashboard
- ClickHouse log store implementation
- Streaming raw-response capture into the persistent log
- Per-tenant analytics with retention tiers
- Completion-style health probe (lightweight `/v1/chat/completions` test)
