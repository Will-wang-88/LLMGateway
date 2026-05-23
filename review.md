# Transparent Enterprise LLM Gateway 程式碼 Review

Review 日期：2026-05-23

Review 範圍：依照使用者提供的「企業級 OpenAI-Compatible LLM Gateway 規格書」檢查目前 repo 全部程式碼、設定、測試、Docker/Helm 與 dashboard。

重要限制：本次只做 review 與驗收建議，未修改任何功能程式。唯一新增檔案是本 `review.md`。

## 總結判定

目前專案已具備「Go HTTP gateway、OpenAI-compatible JSON 轉發、SSE streaming 轉發、basic API key、basic load balancing、basic health check、basic admin/dashboard、basic metrics/logging」的雛形。

但尚未達到規格書定義的企業級 MVP 驗收標準。最需要先修的是 routing/auth 正確性與安全邊界：

1. degraded backend 仍會被送流量，可能把已知會失敗的 backend 放回服務路徑。
2. alias 權限檢查只檢查外部 model 名稱，可能繞過 internal model 的 denied_models。
3. model registry 的 `enabled=false` 只影響 `/v1/models` 顯示，不阻擋實際 `/v1/chat/completions` 轉發。
4. rate limit / quota / request stats / logging 與規格仍有明顯缺口。
5. Admin API、Dashboard、Storage、Redis/PostgreSQL、Metrics、Tracing 距離企業級要求仍不足。
6. 所有 logs / stats 尚未完整記錄 client IP，API key 也尚未支援限制可連線的 client IP。
7. Backend 異常尚未支援 Mail 通知。
8. 本地無 Go toolchain，`go test ./...` 無法執行；Dockerfile 也使用 `golang:1.24-alpine`，但 `go.mod` 宣告 `go 1.25.0`，部署建置有風險。

## 測試狀態

已嘗試執行：

```bash
go test ./...
```

結果：

```text
zsh:1: command not found: go
```

本機目前沒有 Go，因此本 review 是靜態程式碼 review 加測試檔覆蓋檢查，尚未完成實際測試驗收。Claude 修完後必須在有 Go 1.25+ 的環境執行：

```bash
go test ./...
go test -race ./...
docker build -f docker/Dockerfile -t llmgateway:review .
```

## P0 必修：會影響安全、routing 正確性或核心驗收

### P0-1 degraded backend 仍被 routing 使用

位置：

- `internal/backend/health.go:116-128`
- `internal/handlers/handlers.go:622-650`

問題：

Health check 對 401/403/其他 4xx 會標記 backend 為 `degraded`。註解明確寫著 401/403 代表 backend up 但 credential 被拒絕，「不應 blindly route real traffic」。可是 `filterRoutable` 又把 `store.StatusDegraded` 列為可用狀態：

```go
case store.StatusHealthy, store.StatusUnknown, store.StatusDegraded:
```

這會造成已知 auth probe 失敗或版本不相容的 backend 仍接到正式請求，違反規格的 health aware routing。

修改要求：

- 預設 routing 只允許 `healthy`，可選擇短暫允許 `unknown` 作為啟動寬限，但不得允許 `degraded`。
- 如果要支援 degraded routing，必須做成明確 config，例如 `routing.allow_degraded_backends=false`，預設 false。
- 401/403 health check 應直接視為 unroutable；可維持狀態為 `degraded` 供 dashboard 顯示，但 routing 層要排除。

驗收測試：

- 新增測試：backend health check 回 401/403 後，該 backend 不會被 `Forward` 選中。
- 同 model 有 healthy + degraded backend 時，請求只打到 healthy backend。
- 只有 degraded backend 時，回 `503 no_healthy_backend`。

### P0-2 model alias 可繞過 internal model deny list

位置：

- `internal/handlers/handlers.go:209-219`
- `internal/store/store.go:234-249`

問題：

目前轉發流程先 resolve alias：

```go
internalModel, forwardName := h.store.ResolveAlias(peek.Model)
```

但 API key 權限只檢查 client 傳入的外部名稱：

```go
if apiKey != nil && !apiKey.ModelAllowed(peek.Model)
```

若 key 設定：

```yaml
allowed_models: ["*"]
denied_models: ["llama-3.1-70b"]
```

同時 alias：

```yaml
company-main-model -> llama-3.1-70b
```

client 呼叫 `company-main-model` 會通過外部名稱檢查，最後 route 到被 denied 的 `llama-3.1-70b`。這違反「Deny List 優先」與 API key model permission。

修改要求：

- 權限檢查必須同時考慮 `requestedModel` 與 `internalModel`。
- Deny 規則應對兩者任一命中即拒絕。
- Allow 規則建議允許任一名稱命中即可通過，但 deny 永遠優先。
- `/v1/models` 顯示 alias 時，也應確保該 key 對 alias 和 internal model 的權限語意一致，避免顯示可用但實際不可用。

建議 API：

```go
func (k *APIKey) ModelAllowedResolved(requested, internal string) bool
```

驗收測試：

- `allowed_models=["*"]`、`denied_models=["llama-3.1-70b"]`、alias `company-main-model -> llama-3.1-70b`，呼叫 alias 必須 403。
- `allowed_models=["company-main-model"]`、internal 不在 allow list，呼叫 alias 應依產品設計通過，但測試要明確固定語意。
- denied 同時命中 alias 和 internal 任一方時，永遠 403。

### P0-3 disabled model registry 沒有阻擋實際 routing

位置：

- `/v1/models` 有過濾：`internal/handlers/handlers.go:110-140`
- `/v1/chat/completions` 沒有過濾：`internal/handlers/handlers.go:221-229`
- store lookup：`internal/store/store.go:486-490`

問題：

`ListModels` 會把 registry 中 `enabled=false` 的 model 隱藏，但 `Forward` 只查 `BackendsForModel(internalModel)`，沒有檢查 model registry 的 `Enabled`。只要 backend 宣告支援該 model，client 仍能直接呼叫被停用的 model。

這會讓 Admin 停用 model 只影響 catalog，不影響實際權限與 routing。

修改要求：

- 在 alias resolve 後、backend lookup 前，檢查 explicit model registry：
  - 若 model registry 存在且 `enabled=false`，回 `404 model_not_found` 或 `403 model_disabled`。建議依 OpenAI-compatible catalog 語意用 `404 model_not_found`，但需在規格中固定。
  - alias 指向 disabled internal model 也必須被拒絕。
- Admin disable / delete model 的行為要有測試。

驗收測試：

- registry model disabled，backend 仍宣告該 model，直接呼叫應失敗。
- alias 指到 disabled model，呼叫 alias 應失敗。
- `/v1/models` 與實際 routing 結果一致。

### P0-4 缺少 client IP 稽核、統計與 API key IP 存取限制

位置：

- request path：`internal/handlers/handlers.go`
- auth middleware：`internal/auth/middleware.go`
- request log schema：`internal/logstore/logstore.go`
- SQLite schema：`internal/logstore/sqlite.go`
- metrics/stats：`internal/metrics/metrics.go`, `internal/admin/admin.go`, `internal/admin/admin_extra.go`
- config/API key schema：`internal/config/config.go`, `internal/store/store.go`

問題：

新增企業要求：

- 所有 logs 與統計必須記錄 client IP。
- 每個 API key 可以定義允許連線的 client IP。

目前程式只在一般 HTTP debug log 或 admin audit 中偶爾使用 `RemoteAddr` / `X-Forwarded-For`，核心 request log、API key stats、admin stats、dashboard analytics、rate/quota records 都沒有穩定的 `client_ip` 欄位。API key struct/config 也沒有 `allowed_client_ips` / `denied_client_ips` 或 CIDR 檢查，因此無法限制特定 key 只能從指定來源 IP 呼叫。

修改要求：

- 建立單一可信 client IP extractor，例如 `internal/netutil/clientip.go`：
  - 預設使用 `r.RemoteAddr`。
  - 只有當 `RemoteAddr` 屬於 `server.trusted_proxies` / `server.trusted_proxy_cidrs` 時，才信任 `X-Forwarded-For` / `X-Real-IP`。
  - 支援 IPv4、IPv6、CIDR、反向代理多層 XFF。
  - 不可盲目信任任意 client 傳入的 `X-Forwarded-For`。
- API key schema 增加：
  - `allowed_client_ips: []string`
  - `denied_client_ips: []string`
  - 支援 exact IP 與 CIDR，例如 `203.0.113.10`, `10.0.0.0/8`, `2001:db8::/32`。
  - deny 優先於 allow。
  - allow 空陣列代表不限來源；deny 仍生效。
- 在 API key authentication 後、model/routing 前執行 IP policy：
  - 不符合來源 IP 時回 `403 permission_error`，code 建議 `client_ip_not_allowed`。
  - 記錄 request log，包含 `client_ip` 與 rejected reason。
- 所有 request logs 必須新增 `client_ip`：
  - `logstore.RequestLog.ClientIP`
  - SQLite `request_logs.client_ip`
  - Admin `/admin/logs` filter 支援 `client_ip`。
  - Dashboard Logs 顯示 client IP 並可搜尋。
- 所有統計必須支援 client IP 維度：
  - Admin stats 增加 by_client_ip aggregation。
  - Dashboard Analytics 顯示 top client IPs。
  - API key usage drilldown 可依 client IP 分組。
- Prometheus 注意事項：
  - raw client IP 作為 Prometheus label 會有高 cardinality 風險。
  - 若產品要求 Prometheus 也必須帶 `client_ip`，請做成 config，例如 `metrics.client_ip_label_enabled=false` 預設關閉；但 persistent stats / admin analytics 必須記錄 client IP。
- 所有 Gateway 主動錯誤、backend error passthrough、streaming/non-streaming 成功請求都要帶 client IP。

驗收測試：

- `allowed_client_ips=["127.0.0.1/32"]` 時，非該 IP 呼叫回 403 `client_ip_not_allowed`。
- `denied_client_ips` 命中時，即使 allowed 也必須拒絕。
- IPv6/CIDR 匹配正確。
- 未設定 trusted proxy 時，偽造 `X-Forwarded-For` 不可繞過 IP 限制。
- trusted proxy 設定後，正確從 XFF 取最終 client IP。
- request log、admin stats、dashboard logs/analytics 都可看到 client IP。

## P1 必修：MVP 驗收缺口

### P1-1 Authorization header 接受非 Bearer 格式

位置：

- `internal/auth/middleware.go:78-87`

問題：

規格要求 client 使用：

```http
Authorization: Bearer sk-company-xxxx
```

但目前若 header 不符合 `Bearer ` prefix，`extractKey` 仍直接回傳整個 header：

```go
return strings.TrimSpace(raw)
```

這會接受 `Authorization: sk-xxx` 或其他非 Bearer 格式，與規格不符。

修改要求：

- 若 `api_key_prefix` 非空，header 必須以 prefix 開頭，否則回 `401 invalid_api_key`。
- 若要支援 legacy raw key，請增加顯式 config，例如 `auth.allow_raw_authorization=false`，預設 false。

驗收測試：

- `Authorization: sk-test` 必須 401。
- `Authorization: Bearer sk-test` 正常。
- 缺 header 仍 401。

### P1-2 token rate limit / token quota 不是嚴格限制

位置：

- admission check：`internal/handlers/handlers.go:497-537`
- usage accounting：`internal/handlers/handlers.go:458-482`
- limiter：`internal/ratelimit/ratelimit.go`
- quota：`internal/quota/quota.go`

問題：

目前 token limit 檢查只看之前累積的 tokens，實際 tokens 是 backend 回來後才透過 `usage` 加上去。這會導致單次大請求可大幅超過 `tokens_per_minute` / daily token quota / monthly token quota。

另外，如果 backend 沒有回 `usage`，token limit 和 quota 完全不會更新。

修改要求：

- MVP 至少要明確實作一種策略並文件化：
  1. preflight token estimation：從 messages/input/max_tokens 做估算並先 reserve；
  2. postpaid soft-limit：允許本次超出，但下一次阻擋，並在 dashboard/metrics 標示為 soft limit；
  3. strict backend usage required：若 key 有 token limit 但 backend 不回 usage，採保守估算或記錄警告。
- 規格要求 quota control，建議採「估算 reserve + 回應後 reconcile」。

驗收測試：

- 一次請求預估 token 超過 limit 時應 429。
- backend 無 usage 時不應讓 token quota 永久失效。
- streaming final usage 被正確計入。

### P1-3 API key usage stats 只在 backend 回 usage 時更新

位置：

- `internal/handlers/handlers.go:458-482`
- `internal/store/store.go:220-231`
- Admin key list：`internal/admin/admin.go:652-678`

問題：

`apiKey.Touch()` 只在 `recordUsage()` 內呼叫，而 `recordUsage()` 遇到 `u == nil` 直接 return。這代表：

- backend 不回 usage 的成功請求不會更新 API key `last_used_at` / `total_requests`。
- backend error 也不會更新 API key 使用統計。
- Dashboard/API key request count 會嚴重低估。

修改要求：

- 在 request admission 成功或 request completion 時，無論 backend 是否回 usage，都應更新 key 的 `last_used_at` 與 request count。
- token count 仍由 usage 或估算值更新。
- quota request counters 與 API key stats 要語意一致。

驗收測試：

- backend 回應沒有 `usage`，API key `total_requests` 仍 +1。
- backend 500 passthrough，API key `last_used_at` 仍更新。
- streaming 無 usage final chunk，request count 仍更新。

### P1-4 logging policy 實作不完整，且 raw request 記錄的是改寫後 body

位置：

- `internal/handlers/handlers.go:363-388`
- `internal/handlers/handlers.go:586-620`
- `internal/logstore/logstore.go`

問題：

規格要求每個 API key 可設定：

- log_metadata
- log_input
- log_output
- log_raw_request
- log_raw_response
- log_stream_chunks

目前問題：

1. `log_input` / `log_output` 有 config 與 `shouldLog`，但沒有任何實作。
2. `log_raw_request` 記錄的是 `forwardBody`，如果 alias mode 是 `use_internal`，raw log 會變成改寫後的 model，不是 client 傳入 Gateway 的原始 body。
3. 許多 Gateway 主動錯誤沒有寫 persistent request log，例如 invalid JSON、missing model、model_not_found、no_healthy_backend、backend_at_capacity。
4. backend error body 有 passthrough，但 request log 的 `error_code` 只存 HTTP status 字串，沒有嘗試保留 backend error code。

修改要求：

- `raw_request` 必須記錄 client 原始 `raw`，不是 alias rewrite 後的 `forwardBody`。
- 如需記錄 forwarded body，另加 `forwarded_request` 或 metadata 欄位。
- 實作 `log_input` / `log_output`：
  - log_input：可記錄 messages/input 相關欄位。
  - log_output：可記錄 assistant output 或合併後 stream output。
  - 保持預設 false。
- Gateway 所有主動錯誤都要進 request log metadata。
- backend error passthrough 時，若 body 是 OpenAI error envelope，解析 `error.code` 存進 request log，但不得改 response body。

驗收測試：

- alias use_internal + log_raw_request=true，request log 中 raw_request 仍是 client 原始 alias。
- invalid JSON 會產生 request log。
- log_input/log_output=false 時不記內容；true 時有內容。
- backend error `{error:{code:"invalid_thinking_budget"}}` passthrough 且 log error_code 保留。

### P1-5 streaming idle timeout 會注入額外 SSE comment

位置：

- `internal/proxy/stream.go:100-106`

問題：

規格要求 streaming chunk 原樣轉發，不改寫、不注入。現在 idle timeout 時會寫：

```text
: stream-idle-timeout
```

雖然這是 SSE comment，仍然是 Gateway 新增的 chunk，不是 backend 原樣輸出。

修改要求：

- idle timeout 時關閉 upstream/client stream 並記錄 log/metrics，不要寫入任何額外 SSE bytes。
- 若產品想讓 client 看到 timeout event，必須做成明確 opt-in config，預設關閉。

驗收測試：

- backend idle timeout 時，client 收到的 bytes 必須完全等於 backend 已輸出的 bytes，不包含 Gateway 自己的 comment。

### P1-6 Health check schema 與行為缺少規格欄位

位置：

- `internal/config/config.go:55-61`
- `internal/backend/health.go:47-88`

問題：

規格要求 health check 支援：

- `enabled`
- `type`: http / tcp / lightweight completion probe
- `path`
- `interval_ms`
- `timeout_ms`
- failure/success thresholds

目前 config 只有 interval/timeout/threshold/path，且：

- per-backend `IntervalMS` 在 `checkOne` 讀了，但實際 scheduler 只使用全域 ticker，per-backend interval 不生效。
- 無 `enabled`，無法對單一 backend 關閉 health check。
- 無 `type`，不支援 TCP 或 completion probe。
- 401/403 degraded 的 routing 行為如 P0-1。

修改要求：

- 擴充 `HealthCheckConfig` 欄位：`enabled`, `type`, `path`, `method`, optional probe body。
- 實作 per-backend scheduler 或至少明確不支援 per-backend interval，移除無效欄位。
- MVP 可只實作 http/tcp，但 config schema 要與規格一致，completion probe 預設 off。

驗收測試：

- per-backend health_check.enabled=false 不會被 probe，也不影響 status。
- per-backend interval 生效或 config 不再宣稱支援。
- TCP check 可檢測端口可連線。

### P1-7 Admin API 缺少規格要求的 endpoint

位置：

- routes：`internal/admin/admin.go:50-92`

規格要求但目前缺少或不完整：

- `GET /admin/models/{name}`
- `PATCH /admin/models/{name}`
- `PATCH /admin/model-aliases/{alias}`
- `GET /admin/metrics`
- `GET /admin/stats/models`
- `GET /admin/stats/backends`
- `GET /admin/stats/api-keys`

目前 model / alias 只有 list、POST upsert、DELETE。stats 只有 overview/range。

修改要求：

- 補齊規格列出的 Admin API。
- PATCH 應只更新明確傳入欄位，不應要求完整覆蓋。
- 所有 mutating endpoint 都要寫 audit log。
- 補 OpenAPI 或 README API 表格同步更新。

驗收測試：

- 每個規格 endpoint 都有 route test。
- RBAC 對每個 endpoint 生效。
- PATCH model/alias 只改指定欄位。

### P1-8 Dashboard 未達規格頁面資訊密度與操作要求

位置：

- `web/static/index.html`
- `web/static/app.js`
- `web/static/app.css`

主要缺口：

- Overview 缺 QPS/RPM、success rate、error rate、avg/p95/p99 latency、TTFT、queued requests、token throughput。
- Models 缺 backend count、healthy backend count、active/queued requests、avg/p95 latency、tokens/sec、error rate。
- Backends 缺 p95 latency、enable/disable 有但缺 maintenance、last health check detail 顯示不足。
- API Keys 缺 denied_models、quota usage、logging setting、last used time 詳細欄位；只能 create/delete，沒有 patch/disable/rotate UI。
- Logs filter 缺 time range、latency range、error code；不能檢視 raw request/response。
- Analytics 沒有趨勢圖、top errors、capacity planning。
- Settings 是 read-only，規格要求 Admin Settings 管理 backends/model/alias/API keys/rate/logging/dashboard users/RBAC/system config。

修改要求：

- 先補 Dashboard 對應 Admin API 的功能缺口，不必急著換 Next.js。
- 增加 charts 可以用 lightweight chart lib 或後續 Next.js + Recharts/ECharts。
- UI auth 不要長期把密碼放 sessionStorage；至少改成 token/session 模式或短期記憶體。

驗收測試：

- Dashboard 可新增/修改/停用 Backend。
- Dashboard 可新增/修改/停用/rotate API Key。
- Dashboard 可依規格條件查 logs。
- Overview/Models/Backends/API Keys/Analytics 欄位符合規格。

### P1-9 Metrics 不完整，labels 與統計維度不符合規格

位置：

- `internal/metrics/metrics.go:8-86`
- 使用位置：`internal/handlers/handlers.go:286-347`

目前有基本 counter/histogram/gauge，但規格要求的下列項目不足：

- qps/rpm 需由 Prometheus query 推導，目前 Admin/Dashboard 沒提供。
- queued_requests 只有 `QueueDepth`，且只在 acquire 後 set，沒有完整 queue state。
- latency_p50/p95/p99、ttft_p50/p95 需 histogram query，Admin API 未提供。
- tokens_per_second 沒有。
- timeout_count 沒有。
- quota_exceeded_count 沒有獨立 metric。
- labels 缺 `routing_policy`。
- 可選 labels `organization/user/tenant/tool_call/vision/thinking` 沒有任何判定或設計。

修改要求：

- 補齊 Prometheus metrics 與 Admin stats endpoint。
- Request metrics labels 至少包含：api_key_id、model、backend_id、endpoint、status_code、stream、routing_policy。
- timeout/rate/quota/backend error 分開 counter。
- Dashboard 從 Admin stats 或 Prometheus query 顯示 p95/p99/TTFT。

驗收測試：

- `/metrics` 包含規格要求核心 metrics。
- 同一請求能在 metrics 中帶 routing_policy label。
- timeout / quota / rate limit 分別增加不同 counter。

### P1-10 Storage/Redis/PostgreSQL 缺失，無法水平擴展

位置：

- config：`internal/config/config.go:99-103`
- main storage switch：`cmd/gateway/main.go:93-119`
- in-memory limiter/concurrency/queue/quota：`internal/ratelimit`, `internal/queue`, `internal/quota`
- Docker Compose：`docker-compose.yml`

問題：

目前 metadata/config 主要來自 YAML + in-memory store；request/audit log 只有 memory/sqlite。Admin 變更不會寫回 YAML 或 DB，restart 後消失。rate limit/concurrency/queue/quota 都是 process-local，Helm 預設 `replicaCount: 2` 時會直接失去全域限制語意。

規格要求：

- PostgreSQL 儲存 config/metadata。
- Redis 做 rate limit/concurrency/queue。
- Prometheus 做 metrics。
- 水平擴展共享狀態。

修改要求：

- MVP 若先不做 PostgreSQL/Redis，README 與 values.yaml 必須明確標註「不支援多 replica 嚴格限流」。
- 企業級驗收前必須新增：
  - Postgres-backed store：api_keys/backends/models/model_aliases/request_logs/audit_logs。
  - Redis-backed rate limiter/concurrency/quota/queue。
  - runtime admin mutations 持久化。
- Helm 在沒有 Redis/Postgres 前預設 replicaCount 應為 1，避免假 HA。

驗收測試：

- Admin 新增 API key 後 restart 仍存在。
- 兩個 gateway replica 共享 concurrent_requests 限制。
- 兩個 replica 共享 rate limit / quota。

### P1-11 Docker 建置與 compose 不符合規格

位置：

- `go.mod:3`
- `docker/Dockerfile:1`
- `docker-compose.yml:1-21`

問題：

- `go.mod` 宣告 `go 1.25.0`，但 Dockerfile 使用 `golang:1.24-alpine`。
- Compose 沒有 PostgreSQL / Redis，與規格範例不符。
- Compose healthcheck 執行 `/app/llmgateway --config /dev/null --help`，不是打 `/healthz` 或 `/readyz`，無法驗證服務真實健康狀態。

修改要求：

- Dockerfile build image 改成 Go 1.25+，或把 go.mod 降到實際可用版本。
- Compose 加 postgres/redis/prometheus optional services，或明確標記目前是 single-node demo。
- healthcheck 改為 HTTP 檢查 `/healthz`。

驗收測試：

- `docker compose up --build` 可成功啟動。
- `docker inspect` health status 能反映 gateway HTTP health。

### P1-12 declared/strict capability mode 沒有實作

位置：

- config fields：`internal/config/config.go:139-147`
- admin UI exposes modes：`web/static/app.js`
- forwarding path：`internal/handlers/handlers.go`

問題：

規格定義 `passthrough` / `declared` / `strict`。目前欄位存在，但 forwarding path 沒有任何 capability 檢查。Dashboard 甚至允許選 `declared` 或 `strict`，但系統行為仍是 passthrough。

修改要求：

- MVP 可保持預設 passthrough，但若 UI/API 接受 `declared` / `strict`，就必須實作。
- 或先限制只允許 `passthrough`，並在 README 註明 declared/strict 是後續功能。

驗收測試：

- declared mode 下，vision=false 的 model 收到 image_url request 應按規格拒絕。
- passthrough mode 下，同 request 應完整轉發。
- strict mode 未實作前不可在 UI/API 被設定。

### P1-13 Backend 異常缺少 Mail 通知

位置：

- health checker：`internal/backend/health.go`
- config：`internal/config/config.go`
- main wiring：`cmd/gateway/main.go`
- admin/dashboard：`internal/admin`, `web/static/app.js`

問題：

新增企業要求：Backend 異常時可以發送 Mail 通知。

目前 HealthChecker 只更新 backend status、Prometheus gauge 與 logger，沒有 notification subsystem。當 backend 從 healthy 變成 degraded/unhealthy、持續 flapping、或恢復 healthy 時，系統無法主動通知維運人員。

修改要求：

- 新增 notification config，例如：

```yaml
notifications:
  email:
    enabled: true
    smtp_host: smtp.example.com
    smtp_port: 587
    username: alerts@example.com
    password: ${SMTP_PASSWORD}
    from: llmgateway@example.com
    to:
      - ops@example.com
    use_tls: true
    start_tls: true
    cooldown_ms: 300000
    notify_on:
      - backend_degraded
      - backend_unhealthy
      - backend_recovered
```

- backend status transition 時觸發 Mail：
  - `healthy -> degraded`
  - `healthy/degraded/unknown -> unhealthy`
  - `unhealthy/degraded -> healthy` recovery
  - 可選：maintenance 不通知，或單獨配置。
- 通知要非阻塞：
  - 不可阻塞 health check loop。
  - 發送失敗要記 log/metric。
  - 要有 cooldown / dedupe，避免 backend flapping 時洗信。
- Mail 內容至少包含：
  - backend id/name/base_url
  - old status / new status
  - last error
  - latency
  - affected models
  - timestamp
  - gateway instance id / hostname
- Admin API / Dashboard 要可查看 notification 狀態與最近發送結果。
- Secret 不可明文出現在 dashboard 或 logs；SMTP password 應支援 env/secret 注入。

驗收測試：

- fake SMTP server 收到 backend unhealthy notification。
- 同一 backend 在 cooldown 內重複失敗只發一次。
- backend recovery 會發 recovery mail。
- SMTP 發送失敗不影響 health check 與 routing。
- Mail 內容不包含 API key 明文或 backend API key。

## P2 建議修正：企業級完整度與一致性

### P2-1 `/readyz` 把 unknown backend 視為 ready

位置：

- `cmd/gateway/main.go:149-161`

問題：

readiness 目前接受 `StatusUnknown`。這對冷啟動友善，但在 health check 尚未完成或長時間 unknown 時，LB 會提早導流。

建議：

- readiness 預設只接受 healthy。
- 可用 config 設定 startup grace，例如 `ready_allow_unknown_for_ms`。

### P2-2 multipart/audio 不是原樣 passthrough

位置：

- `internal/handlers/audio.go`

問題：

`ForwardMultipart` 會 parse multipart 並 rebuild body，會改 boundary、part ordering、部分 headers。這不符合最嚴格的 transparent passthrough。

建議：

- 若只需讀 model，可以 streaming parse 同時 tee 原始 body，或先 buffer raw bytes 再用 multipart reader 解析 model，最後 forwarding raw bytes。
- 保留原始 Content-Type boundary。

### P2-3 config bool 預設值容易因 YAML 省略變成 false

位置：

- `internal/config/config.go:124-170`
- `internal/store/store.go:335-420`

問題：

Backends/Models/Aliases/APIKeys 的 `enabled bool` 若 YAML 省略會是 false。這容易讓使用者新增項目後意外 disabled。

建議：

- 對 config struct 使用 `*bool` 或 load normalize：省略時 default true。
- sample YAML 可以保留 explicit true，但程式不應依賴使用者每次填。

### P2-4 Admin auth 與 secrets 管理仍偏 demo

位置：

- `internal/admin/admin.go:152-194`
- `web/static/app.js`
- `config/gateway.yaml`

問題：

- Dashboard 以 `prompt()` 收 token/username/password，並存 sessionStorage。
- sample config 有 plaintext admin passwords / API keys / backend API keys。
- API key create 要求使用者提供 key，沒有預設自動產生；rotate 才會產生。

建議：

- Admin 改 JWT session 或 secure cookie。
- Dashboard 避免儲存 password；token 也要有清楚過期語意。
- API key create 若 body 沒 `key`，自動產生並只回傳一次。
- Backend API key 若進 DB，必須 encrypted at rest。

### P2-5 OpenTelemetry tracer 已建立但未接入 request path

位置：

- `cmd/gateway/main.go:122-195`
- `internal/tracing/tracing.go`

問題：

main 建立 tracer 後只有 `_ = tr`，註解寫 future pass。README 卻描述可 emit spans。這是文件與實作不一致。

建議：

- 要嘛移除/降級 README 的 tracing 描述。
- 要嘛在 middleware/handler/proxy 內建立 spans，加入 model/backend/status/latency/ttft/error attrs。

### P2-6 README 宣稱內容比實作更完整

位置：

- `README.md`

問題：

README 說測試涵蓋 transparency、no fallback、permissions、load balancing 等；部分確實有測試，但企業級項目如 Redis/Postgres、tracing、完整 dashboard/admin stats 尚未實作。容易誤導驗收。

建議：

- README 分清楚「已實作」「部分實作」「roadmap」。
- 在企業級完成前，避免宣稱 production HA / Redis-backed / OTEL wired。

## Claude Code 修改順序建議

請 Claude Code 依以下順序處理，避免大改時把 passthrough 特性弄壞。

1. 先修 P0 routing/auth：
   - degraded backend 不可 route。
   - alias/internal model 權限檢查。
   - disabled model registry 實際阻擋 routing。
   - client IP extractor、API key client IP allow/deny、request log/stats client IP。
   - 補對應 regression tests。

2. 修核心 MVP 行為：
   - Bearer prefix 嚴格檢查。
   - API key request stats 不依賴 usage。
   - raw_request 記錄 client 原始 body。
   - Gateway 主動錯誤都寫 request log。
   - streaming idle timeout 不注入 chunk。

3. 補 Admin API 與 Dashboard 最低驗收：
   - 補 GET/PATCH model、PATCH alias、stats endpoints。
   - Dashboard 補 patch/disable/rotate key、patch backend/model/alias、logs filters。

4. 補 metrics/stats：
   - labels 加 routing_policy。
   - logs/stats 全面納入 client_ip，Admin/Dashboard 可依 client IP 查詢與分組。
   - timeout/quota/rate/backend error 分開 counter。
   - Admin stats 提供 p95/p99/TTFT/queued/token throughput。

5. 補 notification：
   - backend status transition 觸發 Email alert。
   - cooldown/dedupe、SMTP secret、安全 log。

6. 處理部署與測試：
   - Dockerfile Go 版本修正。
   - Compose healthcheck 改 HTTP。
   - 執行 `go test ./...`、`go test -race ./...`、Docker build。

7. 企業級 phase：
   - PostgreSQL store。
   - Redis limiter/concurrency/queue/quota。
   - runtime admin mutation 持久化。
   - 真正水平擴展驗收。
   - OTEL tracing 接入。

## 必補測試清單

請至少新增以下測試：

- `TestDegradedBackendIsNotRoutable`
- `TestAliasCannotBypassDeniedInternalModel`
- `TestDisabledRegistryModelCannotBeForwarded`
- `TestAPIKeyAllowedClientIPs`
- `TestAPIKeyDeniedClientIPsTakePrecedence`
- `TestClientIPExtractorDoesNotTrustSpoofedXForwardedFor`
- `TestRequestLogsIncludeClientIP`
- `TestStatsAggregateByClientIP`
- `TestAuthorizationRequiresBearerPrefix`
- `TestAPIKeyStatsIncrementWithoutUsage`
- `TestRawRequestLogKeepsClientOriginalBodyWhenAliasRewrites`
- `TestGatewayErrorsArePersistentlyLogged`
- `TestStreamIdleTimeoutDoesNotInjectSSEBytes`
- `TestAdminModelsGetPatchRoutes`
- `TestAdminAliasPatchRoute`
- `TestMetricsIncludeRoutingPolicyLabel`
- `TestBackendUnhealthySendsEmailNotification`
- `TestBackendNotificationCooldown`
- `TestBackendRecoverySendsEmailNotification`
- `TestDockerBuildUsesCompatibleGoVersion` 或 CI shell check

## 驗收 Checklist

Claude Code 修改後，請用下列 checklist 驗收：

- OpenAI SDK 可用 Gateway base_url 呼叫 `/v1/chat/completions`。
- `/v1/models` 只顯示 key 有權限且 enabled 的 model/alias。
- unknown request fields 完整轉發。
- `reasoning_effort` / `thinking_budget` / `enable_thinking` / `extra_body` 不被拒絕、不被移除。
- backend response 的 `reasoning_content` / `reasoning_tokens` / vendor fields 完整保留。
- streaming SSE chunk byte-level passthrough，不改寫 reasoning delta/tool call delta。
- client disconnect 會取消 backend request。
- no healthy backend 回 `503 no_healthy_backend`，不做 model fallback。
- 同 model 多 healthy backend 有 weighted load balancing。
- unhealthy/degraded/disabled/maintenance backend 不收流量。
- backend max_concurrent_requests 生效。
- API key missing/invalid/disabled/expired 都拒絕。
- API key allowed/denied models 對 alias/internal model 語意正確。
- API key 可設定 allowed_client_ips / denied_client_ips，deny 優先，支援 IPv4/IPv6/CIDR。
- 所有 request log、error log、admin stats、dashboard analytics 都記錄並可查詢 client IP。
- request rate limit、concurrent limit、delay_ms 生效。
- token quota/token rate limit 語意明確且可測。
- logging policy 預設不記 input/output；開啟後才記。
- raw request log 是 client 原始 body。
- backend error status/body 原樣 passthrough。
- Admin API 規格 endpoint 全存在且 RBAC/audit 生效。
- Dashboard 可完成規格要求的查看與管理操作。
- `/metrics` 與 Admin stats 能支援 dashboard 所需核心指標。
- Backend degraded/unhealthy/recovered 可依設定發送 Mail 通知，且有 cooldown/dedupe。
- Docker build、docker compose、Helm chart 都可啟動並通過 health/readiness。
- 多 replica 場景下，若宣稱企業級，rate/concurrency/quota/queue 必須共享狀態。
