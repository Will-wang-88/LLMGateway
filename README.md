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
| GET    | `/admin/backends`                          |
| POST   | `/admin/backends`                          |
| GET    | `/admin/backends/{id}`                     |
| PATCH  | `/admin/backends/{id}`                     |
| DELETE | `/admin/backends/{id}`                     |
| POST   | `/admin/backends/{id}/enable`              |
| POST   | `/admin/backends/{id}/disable`             |
| POST   | `/admin/backends/{id}/health-check`        |
| GET    | `/admin/models`                            |
| POST   | `/admin/models`                            |
| DELETE | `/admin/models/{name}`                     |
| GET    | `/admin/model-aliases`                     |
| POST   | `/admin/model-aliases`                     |
| DELETE | `/admin/model-aliases/{alias}`             |
| GET    | `/admin/api-keys`                          |
| POST   | `/admin/api-keys`                          |
| DELETE | `/admin/api-keys/{id}`                     |
| GET    | `/admin/stats/overview`                    |

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

## Roadmap

The MVP shipped here delivers Phase 1 of the design spec. Out of scope for
this iteration but designed-for in interfaces:

- Persistent storage (PostgreSQL for config / metadata, ClickHouse for logs)
- Redis-backed rate limiter and concurrency counters (for horizontal scaling)
- Web dashboard (Next.js)
- RBAC, audit log, OIDC SSO
- OpenTelemetry tracing
- Helm chart and HPA
