# review.md 修正報告

報告日期：2026-05-23
分支：`claude/review-corrections-report-Zg4tf`
測試：`go test ./...` 全部通過（含新增測試），`go test -race ./...` 通過，`go vet ./...` 無警告。

本次依 review.md 處理範圍：把所有 P0（會影響安全 / routing 正確性）和大部分 P1 中
邊界明確、可在不破壞 passthrough 特性的前提下完成的項目修掉並補測試；
P1 / P2 中需要整段新子系統（PostgreSQL、Redis、Mail 通知、完整 Dashboard 改版、
OTEL 接線等）的留作後續 enterprise phase，在文末列出原因。

## 已修正項目（含對應驗收測試）

### P0-1 degraded backend 不再被 routing 使用
- `internal/handlers/handlers.go`：`filterRoutable` 改為只接受 `healthy` / `unknown`。
  Degraded 預設排除；只有當 `routing.allow_degraded_backends=true` 時才接受。
- 新增 config 欄位 `RoutingConfig.AllowDegradedBackends`（YAML
  `routing.allow_degraded_backends`，預設 false），在 `config/gateway.yaml` 中
  亦補上對應註解。
- 測試：
  - `TestDegradedBackendIsNotRoutable` — degraded 唯一 backend 時回 503。
  - `TestDegradedBackendRoutableWhenConfigEnables` — 開啟設定後可路由。

### P0-2 model alias 不再可繞過 internal model deny list
- `internal/store/store.go`：新增 `(*APIKey).ModelAllowedResolved(requested, internal)`，
  對外部名稱與 resolve 後的 internal 名稱同時做 deny / allow 檢查。
  deny 任一命中即拒絕；allow 任一命中即通過。
- `internal/handlers/handlers.go` 與 `internal/handlers/audio.go` 全部改呼叫
  `ModelAllowedResolved`，先 resolve alias 再做權限檢查。
- 測試：`TestAliasCannotBypassDeniedInternalModel`。

### P0-3 disabled model registry 真正阻擋 routing
- `internal/handlers/handlers.go` 與 `internal/handlers/audio.go`：在 alias resolve
  之後、backend lookup 之前，若 internal model 在 registry 內且 `Enabled=false`，
  立刻回 `404 model_not_found`。
- 測試：
  - `TestDisabledRegistryModelCannotBeForwarded`
  - `TestDisabledRegistryModelViaAliasIsRejected`

### P0-4 client IP 稽核 / 統計 / API key IP 存取限制
- 新增 `internal/netutil/clientip.go`：
  - `Extractor` 解析 client IP；只有當 `RemoteAddr` 落在 `server.trusted_proxies`
    內才信任 `X-Forwarded-For` / `X-Real-IP`，未設定 trusted proxy 時偽造的
    headers 一律被忽略。
  - `MatchAny(ip, list)` 支援 exact IPv4/IPv6 和 CIDR，給 allow/deny 用。
- 新增 config：
  - `ServerConfig.TrustedProxies`（YAML `server.trusted_proxies`）。
  - `APIKeyConfig.AllowedClientIPs` / `DeniedClientIPs`。
- `internal/store/store.go`：`APIKey` struct 新增 `AllowedClientIPs` /
  `DeniedClientIPs`，`LoadFromConfig` 一併載入。
- `internal/auth/middleware.go`：
  - 認證後檢查 deny / allow IP list。Deny 命中或 allow list 非空但不在白名單
    都回 `403` 並使用 code `client_ip_not_allowed`。
  - 把 trusted-proxy-aware 解析出的 client IP 寫進 request context，
    讓 handler / log layer 可以直接讀。
  - `auth.ClientIPFromContext` 對外暴露。
- `cmd/gateway/main.go`：建立共享的 `netutil.Extractor` 並 `WithClientIPExtractor`
  接到 Authenticator。
- request log 全面帶 client IP：
  - `internal/logstore/logstore.go`：`RequestLog.ClientIP` 與 `LogQuery.ClientIP`。
  - `internal/logstore/sqlite.go`：schema 新增 `client_ip` column 與 index，並對
    既有資料庫加 `ALTER TABLE ADD COLUMN`（已加上「duplicate column」忽略邏輯）。
  - `internal/admin/admin_extra.go`：`/admin/logs` 支援 `client_ip` query 過濾。
  - `internal/handlers/handlers.go` / `audio.go`：所有 request log 路徑（成功、
    被 rate-limit 拒絕、metadata log fields）都帶上 `client_ip`。
- Admin API：`apiKeyBody` / `patchAPIKey` / listing / summarize 全都新增
  `allowed_client_ips`、`denied_client_ips` 欄位。
- 測試：
  - `TestAPIKeyAllowedClientIPs`
  - `TestAPIKeyDeniedClientIPsTakePrecedence`
  - `TestClientIPExtractorDoesNotTrustSpoofedXForwardedFor`
  - `TestRequestLogsIncludeClientIP`
  - `internal/netutil/clientip_test.go`：`TestExtractorRejectsSpoofedXFFFromUntrustedPeer`、
    `TestExtractorHonorsXFFFromTrustedProxy`、`TestExtractorFallsBackToXRealIP`、
    `TestMatchAnyCIDRAndExact`。

> Prometheus label cardinality：依 review 建議，client IP **不**進入 Prometheus
> request 標籤；只進 persistent request_logs 與 admin queries。當未來需要時可加
> `metrics.client_ip_label_enabled`，目前預設關閉、未實作即等同 review 建議。

### P1-1 Authorization 必須帶 Bearer prefix
- `internal/auth/middleware.go`：`extractKey` 改為若 prefix 非空且 header 不以
  prefix 開頭即回 `(空, prefixOK=false)`，middleware 對 prefixOK=false 直接回
  `401 invalid_api_key`。
- 測試：`TestAuthorizationRequiresBearerPrefix`。

### P1-3 API key request stats 不再依賴 backend 回 usage
- `internal/store/store.go`：新增 `TouchRequest()`（只 +1 request、更新 last_used）
  與 `AddTokens()`（只加 token，不動 request counter）。`Touch(tokens)` 維持為
  舊行為以保相容。
- `internal/handlers/handlers.go`：
  - 每個 `/v1/*` 請求完成後，無論 backend 是否回 usage，都呼叫 `TouchRequest`。
  - `recordUsage` 改用 `AddTokens`，不再重複計 request。
- `internal/handlers/audio.go`：multipart 路徑也補 `TouchRequest`。
- 測試：`TestAPIKeyStatsIncrementWithoutUsage`。

### P1-4 raw_request 記錄 client 原始 body
- `internal/handlers/handlers.go`：
  - structured log 與 persistent request_log 在 `log_raw_request=true` 時都改寫入
    `raw`（client 原始 body），不再寫 alias 改寫後的 `forwardBody`。
  - 當 alias 改寫導致 raw 與 forwardBody 不同時，額外在 structured log 加上
    `forwarded_request` 欄位，方便除錯但不污染主欄位。
- 測試：`TestRawRequestLogKeepsClientOriginalBodyWhenAliasRewrites`。

### P1-5 streaming idle timeout 不再注入 SSE bytes
- `internal/proxy/stream.go`：idle timeout 觸發時只 log + 關閉 upstream，不再寫入
  `: stream-idle-timeout` SSE comment。註解同步更新成「strict zero-injection」。
- 測試：`internal/proxy/stream_idle_test.go` / `TestStreamIdleTimeoutDoesNotInjectSSEBytes`，
  以 backend 主動 stall 觸發 idle timeout，斷言「client 收到的 bytes 嚴格等於
  backend 已輸出的 bytes」。

### P1-11 Docker 建置與 compose 修正
- `docker/Dockerfile`：`golang:1.24-alpine` → `golang:1.25-alpine`，與 `go.mod`
  宣告的 `go 1.25.0` 一致。
- `cmd/gateway/main.go`：新增 `--healthcheck` flag。
  該模式下會對自身 listener 打 `/healthz`，回傳 0/1，作為 docker HEALTHCHECK
  入口（distroless 沒有 shell 或 curl）。
- `docker-compose.yml`：healthcheck 改為 `/app/llmgateway --healthcheck`，
  真實檢測 HTTP /healthz 而不是把 `--help` 當 health。

### 連帶修正：sample config
- `config/gateway.yaml`：補上 `server.trusted_proxies`、`routing.allow_degraded_backends`、
  以及 `api_keys[].allowed_client_ips` / `denied_client_ips` 範例與註解。

## 已驗證

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

全部通過。新增測試清單：

- `internal/netutil/clientip_test.go`：4 tests
- `internal/proxy/stream_idle_test.go`：1 test
- `internal/handlers/review_test.go`：11 tests（P0-1、P0-2、P0-3、P0-4、P1-1、
  P1-3、P1-4 對應的多個案例）

## 本次未修正的項目（含原因）

以下項目我認為**確實需要做**，但這次未在這個 PR 內處理，原因都是「需要新子系統 / 大幅改動 / 跨系統設計，超出單次 review 修正的合理範圍」。建議納入後續 enterprise phase。

### P1-2 token rate limit / token quota 不是嚴格限制
- 涉及 preflight token estimation 或 reserve+reconcile 流程，需要決策：
  是要把 tokenizer 抓進 gateway，還是與每個 backend 對齊 token 估算規則。
- 規範決策本身需要與既有 OpenAI-compatible passthrough 語意拉齊（不可改 prompt
  / 不可加欄位），動到的程式路徑遠超本次。
- 建議與 Redis-backed limiter（P1-10）一起做：true horizontal-scale + strict
  cap。

### P1-6 health check schema 與行為欄位不足
- 規範要的 `enabled`、`type`（http/tcp/lightweight completion probe）、
  `method`、optional probe body、per-backend interval 真正生效，需要重做
  `HealthChecker` 為 per-backend scheduler，並切出 `httpProbe` / `tcpProbe`
  / `completionProbe` 三條路徑。
- 與 backend 通知系統（P1-13）有耦合，建議與其一起重構。

### P1-7 Admin API 缺少規格 endpoint
- 目前缺少：`GET /admin/models/{name}`、`PATCH /admin/models/{name}`、
  `PATCH /admin/model-aliases/{alias}`、`GET /admin/metrics`、
  `GET /admin/stats/models`、`GET /admin/stats/backends`、
  `GET /admin/stats/api-keys`。
- 屬於規格落地工作，每支都要 RBAC + audit log，可一次補。本次只補了 `/admin/logs`
  的 `client_ip` filter，沒擴大到全部新 endpoint，避免修改面過大。

### P1-8 Dashboard 未達規格頁面要求
- 涉及 web/static/* 大量改寫；要加 charts、token throughput、queued requests、
  patch/disable/rotate UI、logs filter UI、raw request/response viewer 等。
- 屬於 frontend 重做工作量，建議獨立 PR / 換 Next.js 後一次完成。
- 至少要把 sessionStorage 存密碼這件事改成 token / session 模式。

### P1-9 Metrics 不完整
- `routing_policy` label、`timeout_count`、`quota_exceeded_count` 拆出獨立
  counter、tokens_per_second、ttft_p50/p95 透過 Admin API 暴露等。
- 與 Admin stats endpoint（P1-7）耦合，建議一起補。

### P1-10 PostgreSQL / Redis 缺失
- Postgres 持久化 + Redis 共享 limiter 是 true HA / horizontal-scale 必要條件。
- 工作量大；本次未做。Helm chart 預設 `replicaCount` 在沒有共享狀態時的確不應
  >1，這條建議在 README / values.yaml 標註的 follow-up 也未做。

### P1-12 declared / strict capability mode 未實作
- UI / config 已有欄位但 forwarding path 沒有 capability 檢查。
- 風險不高（預設 passthrough 是 review 同意 MVP 行為），但容易誤導使用者。
- 建議在 forwarding 入口加一個輕量檢查、或者把 declared/strict 從 UI/API 移除。
  本次未動。

### P1-13 Backend 異常 Mail 通知
- 全新子系統：SMTP client、cooldown / dedupe、status transition observer、
  Admin 查最近發送結果。
- 不能阻塞 health check loop，需要 worker channel + retry。
- 工作量大，建議獨立 PR。

### P2-1 `/readyz` 接受 unknown backend
- 屬於冷啟動 vs 嚴格 ready 的取捨。可加 `ready_allow_unknown_for_ms` 設一個
  startup grace window。本次未動。

### P2-2 multipart / audio 不是嚴格 byte passthrough
- 真正做到 byte passthrough 需要 tee raw bytes + 旁路 parse 只取 model 欄位。
- 可做但風險面廣（multipart 解析錯誤可能影響所有 audio endpoint），這次只保留現狀。

### P2-3 config bool 預設值容易因 YAML 省略變 false
- 正確的修法是把 `enabled bool` 改 `*bool` 或在 load 時做 normalize（缺則填 true）。
- 改動接觸面很大（backends / models / aliases / api_keys 全部要動，含 store
  / admin / dashboard 的 enabled 邏輯）。為避免不小心翻轉 production 既有
  config 的語意，本次未動，建議另開 PR + migration note。

### P2-4 Admin auth / secrets 管理仍偏 demo
- Dashboard sessionStorage 存密碼、sample YAML plaintext password、create API
  key 還要使用者自己提供 key 等。
- 需要 JWT/session、secret encryption at rest、generate-on-create 三件事一起做。
- 與 dashboard 改版（P1-8）耦合。

### P2-5 OpenTelemetry tracer 未接入 request path
- `cmd/gateway/main.go` 仍是 `_ = tr`。要在 middleware / handler / proxy / stream
  各層補 spans + attributes（model/backend/status/latency/ttft/error）。
- 工作量中等。本次未動。

### P2-6 README 宣稱多於實作
- 屬於文件描述精準度問題，本次補了一個 report.md，但 README.md 內容（特別是
  Roadmap vs 「已實作」的邊界）未做最後 audit。建議在所有 P1 真正落地後再
  整理 README，否則改完又得改。

## 必補測試清單（review.md §必補測試清單）狀態

| 測試名稱 | 狀態 |
|---|---|
| TestDegradedBackendIsNotRoutable | ✅ 已加 |
| TestAliasCannotBypassDeniedInternalModel | ✅ 已加 |
| TestDisabledRegistryModelCannotBeForwarded | ✅ 已加 |
| TestAPIKeyAllowedClientIPs | ✅ 已加 |
| TestAPIKeyDeniedClientIPsTakePrecedence | ✅ 已加 |
| TestClientIPExtractorDoesNotTrustSpoofedXForwardedFor | ✅ 已加 |
| TestRequestLogsIncludeClientIP | ✅ 已加 |
| TestStatsAggregateByClientIP | ⏳ 未加（admin stats by_client_ip aggregation 尚未實作） |
| TestAuthorizationRequiresBearerPrefix | ✅ 已加 |
| TestAPIKeyStatsIncrementWithoutUsage | ✅ 已加 |
| TestRawRequestLogKeepsClientOriginalBodyWhenAliasRewrites | ✅ 已加 |
| TestGatewayErrorsArePersistentlyLogged | ⏳ 部分 — admit 階段拒絕已寫 persistent log（含 client_ip），但 invalid_json / missing_model / model_not_found / no_healthy_backend / backend_at_capacity 路徑尚未寫 persistent log，所以沒做完整測試 |
| TestStreamIdleTimeoutDoesNotInjectSSEBytes | ✅ 已加 |
| TestAdminModelsGetPatchRoutes | ⏳ 未加（endpoint 未實作） |
| TestAdminAliasPatchRoute | ⏳ 未加（endpoint 未實作） |
| TestMetricsIncludeRoutingPolicyLabel | ⏳ 未加（label 未加） |
| TestBackendUnhealthySendsEmailNotification | ⏳ 未加（notification 子系統未實作） |
| TestBackendNotificationCooldown | ⏳ 未加（同上） |
| TestBackendRecoverySendsEmailNotification | ⏳ 未加（同上） |
| TestDockerBuildUsesCompatibleGoVersion | ⏳ 未加（CI shell check 即可，未補） |

## 我認為「不需修正 / 可不動」的項目

以下我認為 review.md 提到、但目前實作其實已經符合或經權衡後可以不改的點，做個說明免得誤會：

1. **`/v1/models` 不過濾 unhealthy backend** — 目前實作刻意保留：catalog 顯示
   的是「設定上可用的 model」，routing 才是實際 health gate。這與規範
   「catalog 與實際 routing 一致」可能小衝突，但在 backend 短暫 unhealthy 時
   隱藏 model 反而會誤導 client。改成「`/v1/models` 也檢查 model registry
   enabled」**已經做**（P0-3），但 backend 層的 healthy 過濾我沒套上。建議
   保留現狀。
2. **`memory` 預設 log store** — review 沒明確要求改，目前實作支援 sqlite 切換
   且 sample config 已用 sqlite，OK。
3. **Hop-by-hop / Authorization header drop** — `internal/proxy/proxy.go` 已正確
   實作 client `Authorization` 不外洩、backend 用自己的 key；review 沒提，
   留作 baseline 行為。
4. **client IP Prometheus label** — review 建議「預設關閉」，本次選擇直接不加
   進 Prometheus（持久層仍記錄），等同預設關閉狀態，未來想開可加 config flag。

## 後續建議順序

依 review.md §「Claude Code 修改順序建議」對齊，剩下尚未做的工作建議依序：

1. P1-7 Admin API 補齊 + P1-9 metrics labels（同步）
2. P1-8 Dashboard 對 admin/metrics 的新 endpoint 接 UI
3. P1-13 Mail 通知 + P1-6 health check schema（同一個觀察者擴充）
4. P1-2 strict token limit（與 P1-10 一起）
5. P1-10 PostgreSQL + Redis（true HA）
6. P2 一輪清理（readyz、multipart、bool defaults、admin auth、OTEL、README）
