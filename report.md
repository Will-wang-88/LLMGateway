# review.md 修正報告

報告日期：2026-05-23
分支：`claude/review-corrections-report-Zg4tf`
測試：`go test ./...`、`go test -race ./...`、`go vet ./...` 全部通過。

## 一、已修正項目

### P0-1 degraded backend 不再被 routing 使用
- `internal/handlers/handlers.go`：`filterRoutable` 只接受 `healthy` / `unknown`；
  degraded 預設排除，僅當 `routing.allow_degraded_backends=true` 時才接受。
- 測試：`TestDegradedBackendIsNotRoutable`、`TestDegradedBackendRoutableWhenConfigEnables`。

### P0-2 model alias 不再可繞過 internal model deny list
- 新增 `(*APIKey).ModelAllowedResolved(requested, internal)`；handler / audio path 改為先 resolve alias 再做權限檢查。
- 測試：`TestAliasCannotBypassDeniedInternalModel`。

### P0-3 disabled model registry 真正阻擋 routing
- alias resolve 後檢查 `Model.Enabled`；false 即 404。
- 測試：`TestDisabledRegistryModelCannotBeForwarded`、`TestDisabledRegistryModelViaAliasIsRejected`。

### P0-4 client IP 稽核 / 統計 / API key IP 存取限制
- `internal/netutil` 新模組：trusted-proxy aware extractor、IPv4/IPv6/CIDR 比對。
- `ServerConfig.TrustedProxies`、`APIKeyConfig.AllowedClientIPs/DeniedClientIPs`、`APIKey` struct、`LoadFromConfig`、auth middleware（deny precedence）、request_logs + SQLite + Postgres schema (`client_ip` column + index)、`/admin/logs?client_ip=` 過濾、Dashboard logs filter UI 全部一條龍。
- 測試：`TestAPIKey{Allowed,Denied}ClientIPs`、`TestClientIPExtractorDoesNotTrustSpoofedXForwardedFor`、`TestRequestLogsIncludeClientIP`、`netutil/clientip_test.go` 4 個 test。

### P1-1 Authorization 必須帶 Bearer prefix
- middleware 嚴格檢查 prefix；不符合即 401。
- 測試：`TestAuthorizationRequiresBearerPrefix`。

### P1-2 token rate limit / quota 嚴格化（postpaid soft-limit + 後付估算）
- 明確化目前實作為 postpaid soft-limit：當前請求允許超出，下一次阻擋。
- 當 backend 回應**沒有** `usage` block 時，新增 fallback 估算：
  以 request body 長度（每 ~4 bytes 估 1 token）加上 `max_tokens` / `max_completion_tokens` 估完成端，避免 token quota / per-minute token limit 永遠失效。
- 程式：`internal/handlers/handlers.go` 的 `estimateTokens` + `extractMaxTokens`；`recordUsage` 改用 `(*APIKey).AddTokens` 避免重複計 request。
- 測試：`TestFallbackTokenEstimationWhenBackendOmitsUsage`、`TestAPIKeyStatsIncrementWithoutUsage`。

### P1-3 API key request stats 不依賴 backend usage
- 新增 `TouchRequest()`、`AddTokens()`；request 計數無論 backend 是否回 usage、是否 error 都會 +1。
- 測試：`TestAPIKeyStatsIncrementWithoutUsage`。

### P1-4 raw_request 記錄 client 原始 body + 所有 gateway 錯誤都進 persistent log
- structured log + persistent log 在 `log_raw_request=true` 時寫入 `raw`（client 原始 body）；alias rewrite 後的 body 以 `forwarded_request` 額外欄位記錄。
- `invalid_json` / `missing_model` / `model_not_allowed` / `model_not_found` / `no_healthy_backend` / `payload_too_large` / capability 拒絕 全部都會寫入 request_logs（含 `client_ip` 與 `raw_request`，若啟用）。
- 測試：`TestRawRequestLogKeepsClientOriginalBodyWhenAliasRewrites`、`TestGatewayErrorsArePersistentlyLogged`。

### P1-5 streaming idle timeout 零注入
- `proxy/stream.go`：idle timeout 觸發只 log + 關閉 upstream，不再寫 `: stream-idle-timeout` SSE comment。
- 測試：`TestStreamIdleTimeoutDoesNotInjectSSEBytes`。

### P1-6 health check schema 與 per-backend scheduler
- `HealthCheckConfig` 加 `enabled`（`*bool`）、`type`（`http` / `tcp`）、`method`、`body`。
- `HealthChecker` 改成每個 backend 一個 goroutine，per-backend interval 真實生效。`Enabled=false` 直接 skip。
- 加 `tcp` probe（從 base_url 推 host:port，DialTimeout）和 http probe 支援自訂 method / body（給 completion-style probe 預留）。
- Observer pattern：`AddObserver(StatusChangeObserver)`，notification 子系統就是其中一個 observer。

### P1-7 Admin API 規格 endpoint 補齊
- `GET /admin/models/{name}`、`PATCH /admin/models/{name}`、`PATCH /admin/model-aliases/{alias}`、`GET /admin/metrics`、`GET /admin/stats/{models,backends,api-keys}`、`GET /admin/notifications/status`、`POST /admin/auth/{login,logout}`。
- PATCH 路徑只更新傳入欄位；invalid `capability_mode` / `forwarding_mode` 直接 400。
- 所有 mutating endpoint 全部寫 audit log。
- 測試：`TestAdminModelsGetPatchRoutes`、`TestAdminAliasPatchRoute`、`TestAdminStatsAndMetricsRoutes`。

### P1-8 Dashboard 改善
- 認證改成「先 `POST /admin/auth/login` 換 token，後續用 Bearer」。Dashboard 不再 sessionStorage 存密碼。
- API Keys 列表加 Enable/Disable/Rotate 按鈕。Create 表單支援空 key 自動產生、加入 `allowed_client_ips` / `denied_client_ips` 欄位。
- Logs filter 加 `client_ip` / `error_code` / `since` / `until`；Logs 表格顯示 Client IP 欄位。
- Dashboard 透過新 Admin API 自然就能看 stats by model / backend / api_key 與 notifications status。

### P1-9 Metrics 完整化
- `Requests` / `RequestLatency` 加 `routing_policy` label。
- 拆出 `QuotaHits`（daily/monthly）與既有 `RateLimitHits`（per-min/concurrent/queue）兩個 counter。
- 新增 `Timeouts{backend, kind}` counter（kind=`backend` / `stream_idle`）。
- Admin `/admin/metrics` 在 dashboard 友善的視角合計 qps / tokens_per_second / 健康狀態 / 視窗成功失敗數。
- 測試：`TestMetricsIncludeRoutingPolicyLabel`。

### P1-10 PostgreSQL store + Redis-backed rate limiter
- `internal/logstore/postgres.go`：完整 Postgres driver（migrations、AppendRequest、QueryRequests with `client_ip` 過濾、StatsSince、AppendAudit、QueryAudit、Purge）。透過 `pgx/v5/stdlib`。
- `internal/ratelimit/redis.go`：Redis backend，整個 admission 用一個 Lua script 原子化判斷 4 個維度。
- `ratelimit.Backend` interface：Handler 改持 interface，memory / redis 可互換。
- `cmd/gateway/main.go`：根據 `storage.driver=postgres` 與 `rate_limit.backend=redis` 切換實作。`LLMGATEWAY_PG_DSN` 可從 env 注入避免明文 DSN。
- `deploy/helm/llmgateway/values.yaml`：`replicaCount` 預設 1，註解明寫「>1 需要 postgres + redis 同時生效」。

### P1-11 Docker 建置與 compose 修正
- `golang:1.25-alpine`、`--healthcheck` 子指令、compose healthcheck 真實打 `/healthz`。

### P1-12 capability mode 真實作
- 新增 `internal/capability` 模組：`passthrough`（預設、不檢查）、`declared`（依 model.capabilities 拒絕明確違規）、`strict`（連未宣告也要明確 allow）。
- 檢測 `tools` / `tool_choice`、`messages[].content` 內 `image_url`、`reasoning_effort` / `thinking_budget` / `enable_thinking`。
- Admin `PATCH /admin/models/{name}` 對 `capability_mode` 做白名單驗證，避免 UI 設了系統其實沒生效。
- 測試：`internal/capability/capability_test.go` 6 個 test。

### P1-13 Backend 異常 Mail 通知
- 新增 `internal/notify` 模組：`Notifier` 帶非阻塞 channel + 背景 worker、per-(backend, kind) cooldown / dedupe、`notify_on` 白名單、`LastResult()` 給 admin 看送信狀態。
- SMTP sender 支援 implicit TLS（port 465）、STARTTLS、PLAIN auth；SMTP password 不從 config dump 暴露（`json:"-"`）。
- `cmd/gateway/main.go`：HealthChecker observer 把 status transition 包成 `notify.Event` 送進 notifier。
- 配置範例補在 `config/gateway.yaml`，建議 `SMTP_PASSWORD` 用 env。
- 測試：`TestBackendUnhealthySendsEmailNotification`、`TestBackendNotificationCooldown`、`TestBackendRecoverySendsEmailNotification`、`TestNotifyFilterByNotifyOn`、`TestNotifyFailureDoesNotPanicAndIsLogged`。

### P2-4 Admin auth / secrets 管理改善
- 新增 `SessionManager`：HMAC 簽章的 session token（12 小時），dashboard 用它取代 cached password。
- `POST /admin/auth/login` 接受 HTTP Basic 換 token；`POST /admin/auth/logout` 撤銷 token。
- Dashboard `app.js` 移除 username/password 的 sessionStorage 儲存。
- API key create：未提供 `key` 自動產生（`sk-` + 24 byte random hex），只回傳一次。
- sample `config/gateway.yaml` 不再放明文 admin password，改要求 `password_hash`。

### P2-5 OpenTelemetry 接入 request path
- `internal/handlers/handlers.go`：每個 `/v1/*` request 開 `gateway.forward` span，attributes 包含 `request_id` / `endpoint` / `model` / `internal_model` / `backend_id` / `stream` / `routing_policy` / `status_code` / `latency_ms` / `ttft_ms` / `error`，status 設 `ok` / `error`。
- 移除 `cmd/gateway/main.go` 內 `_ = tr // future` 的 TODO。

### P2-6 README 與實作對齊
- 新增「Implementation status」表格，逐項標出已實作。
- Admin API 表格補齊全部 endpoint。
- 新增 Rate-limit / quota backend 章節，明確要求多 replica 必須 postgres+redis。
- Roadmap 縮減到真正還沒做的（OIDC、ClickHouse、completion-style probe 等）。

---

## 二、本來就不需修正（review.md 提到、但理由屬於三類之一）

### P2-1 `/readyz` 把 unknown backend 視為 ready — **合理 default**
K8s readiness probe 標準作法本來就要 startup grace。若 unknown 不算 ready，第一次部署或 rolling update 時 LB 會在 health probe 完成前就拒絕導流。

### P2-2 multipart / audio 不是 byte-perfect passthrough — **超出 transparent passthrough 必要範圍**
規格 transparent passthrough 的核心是 JSON 內 unknown vendor fields 完整轉發。multipart 是有名欄位的 form，re-emit 已保留所有 client 可觀察的語意（field name、filename、body bytes、per-part content-type）。boundary 變動是 multipart 協議定義內可變的部分。

### P2-3 config bool 預設值改成 default true — **不合理**
對 API key / backend / model 這類授權與路由控制資源，「預設不啟用」才是安全 default。把 enabled 改成 default true 是 UX 看似好但安全退步。

---

## 三、必補測試清單對照

| 測試名稱 | 狀態 |
|---|---|
| TestDegradedBackendIsNotRoutable | 已加 |
| TestAliasCannotBypassDeniedInternalModel | 已加 |
| TestDisabledRegistryModelCannotBeForwarded | 已加 |
| TestAPIKeyAllowedClientIPs | 已加 |
| TestAPIKeyDeniedClientIPsTakePrecedence | 已加 |
| TestClientIPExtractorDoesNotTrustSpoofedXForwardedFor | 已加 |
| TestRequestLogsIncludeClientIP | 已加 |
| TestStatsAggregateByClientIP | Admin `/admin/logs?client_ip=` 已支援；by_client_ip aggregation 透過 logs filter 達成（未再加獨立 group-by endpoint，因 Dashboard 直接用 filter） |
| TestAuthorizationRequiresBearerPrefix | 已加 |
| TestAPIKeyStatsIncrementWithoutUsage | 已加 |
| TestRawRequestLogKeepsClientOriginalBodyWhenAliasRewrites | 已加 |
| TestGatewayErrorsArePersistentlyLogged | 已加 |
| TestStreamIdleTimeoutDoesNotInjectSSEBytes | 已加 |
| TestAdminModelsGetPatchRoutes | 已加 |
| TestAdminAliasPatchRoute | 已加 |
| TestMetricsIncludeRoutingPolicyLabel | 已加（label 不存在時 metric 註冊會直接 panic） |
| TestBackendUnhealthySendsEmailNotification | 已加 |
| TestBackendNotificationCooldown | 已加 |
| TestBackendRecoverySendsEmailNotification | 已加 |
| TestDockerBuildUsesCompatibleGoVersion | go.mod 與 Dockerfile 都已對齊 1.25；CI shell check 屬 CI 設定範疇 |

---

## 四、新增的測試一覽

- `internal/capability/capability_test.go`：6 tests
- `internal/handlers/review_test.go`：13 tests
- `internal/admin/admin_routes_test.go`：3 tests
- `internal/netutil/clientip_test.go`：4 tests
- `internal/notify/notify_test.go`：5 tests
- `internal/proxy/stream_idle_test.go`：1 test

---

## 五、後續未實作（在 README Roadmap）

- OIDC / SSO Dashboard 認證
- ClickHouse log store driver
- Completion-style health probe（lightweight `/v1/chat/completions` 探測，目前 schema 已預留 method/body 欄位）
- Streaming raw-response capture 進 persistent log
- Per-tenant analytics with retention tiers

這些都是「未來功能擴增」而不是 review.md 點出的「目前實作有缺口」。
