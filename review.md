# Transparent Enterprise LLM Gateway 二次 Review

Review 日期：2026-05-23

Review 範圍：

- 已先閱讀 `report.md`。
- 再 review `report.md` 宣稱修正後的目前程式碼。
- 本文件只列「目前修改後仍需要修正」的項目，避免重複處理已修項目。

重要限制：

- 本次只做 review 與驗收建議。
- 不修改 gateway 功能程式。
- Claude Code 請依本文件修正程式與補測試。

## 總結判定

`report.md` 宣稱多數前一版 review 已修完，且 `go test ./...`、`go test -race ./...`、`go vet ./...` 全部通過。靜態 review 後確認有不少項目已改善，例如 degraded backend routing、alias deny、disabled model、dashboard token login、Postgres logstore、Redis limiter、mail notifier 等。

但目前仍不能驗收通過，主要原因是：

1. token rate limit / daily token quota 的 limiter key 不一致，實際不會生效。
2. 多個 gateway error path 無視 logging policy，會記錄 raw request。
3. trusted proxy 後的 client IP 解析仍可被 `X-Forwarded-For` spoof。
4. client IP 的 log/stat 覆蓋不完整，auth middleware 直接拒絕的請求不進 persistent log。
5. Admin runtime 新增 backend 後沒有週期 health check，mail notification 也會漏。
6. SMTP STARTTLS 可默默降級成 plaintext。
7. Postgres 目前只存 logs/audit，不存 backends/models/api_keys metadata。

## 測試狀態

本機嘗試執行：

```bash
go test ./...
```

結果：

```text
zsh:1: command not found: go
```

目前環境沒有 Go toolchain，因此本次是靜態 review，無法在本機驗證 `report.md` 所述測試結果。Claude Code 修完後必須在有 Go 1.25+ 的環境執行：

```bash
go test ./...
go test -race ./...
go vet ./...
docker build -f docker/Dockerfile -t llmgateway:review .
```

另外已確認：

```bash
git diff --check f544eb8..HEAD
```

沒有 whitespace error。

## P0 必修：會影響安全、限流或核心驗收

### P0-1 token rate limit / daily token quota 實際不會生效

位置：

- `internal/handlers/handlers.go:634-635`
- `internal/handlers/handlers.go:682`
- `internal/ratelimit/ratelimit.go:102-137`
- `internal/ratelimit/redis.go:72-106`

問題：

`admit()` 檢查 limiter 時使用：

```go
h.limiter.CheckAndReserve("key:"+k.ID, rpm, tpm, dayReq, dayTok)
```

但 `recordUsage()` 累積 token 時使用：

```go
h.limiter.AddTokens("tok:"+apiKey.ID, u.TotalTokens)
```

兩邊 key 不一致，導致 `TokensPerMinute`、`TokensPerDay`、`Quota.DailyTokens` 經由 limiter 檢查時永遠看不到 token 累積。結果是 token rate limit / daily token quota 幾乎等於失效。

修改要求：

- `CheckAndReserve()` 與 `AddTokens()` 必須使用同一個 bucket key。
- 建議統一使用 `"key:"+apiKey.ID`。
- memory limiter 與 Redis limiter 都要一致。
- 補測試覆蓋 handler 層，不只測 limiter 單元。

驗收測試：

- 新增 `TestHandlerTokensPerMinuteBlocksAfterUsage`：
  - 設定 API key `tokens_per_minute` 很小。
  - 第一次 backend 回 usage 讓 token 累積超過限制。
  - 第二次請求必須回 `429 token_rate_limit_exceeded`。
- 新增 `TestHandlerDailyTokenLimitBlocksAfterUsage`：
  - 設定 `quota.daily_tokens` 或 `rate_limit.tokens_per_day`。
  - 使用 backend usage 或 fallback estimate 超過後，下一次請求必須被擋。
- Redis limiter 也需有等價測試或 integration test。

### P0-2 gateway error path 會無視 logging policy 記錄 raw request

位置：

- `internal/handlers/handlers.go:220-229`
- `internal/handlers/handlers.go:244-267`
- `internal/handlers/handlers.go:278-294`
- `internal/handlers/handlers.go:301-303`
- `internal/handlers/handlers.go:522-560`

問題：

成功路徑有依 `log_raw_request` 決定是否寫 `raw_request`：

```go
if h.shouldLog(apiKey, "log_raw_request") {
    rawReqForLog = raw
}
```

但多個錯誤路徑直接呼叫：

```go
h.recordLog(..., raw, nil)
```

而 `recordLog()` 只要 `rawReq` 非空就寫入：

```go
if len(rawReq) > 0 {
    rec.RawRequest = string(rawReq)
}
```

這會讓以下錯誤在 `log_raw_request=false` 時仍記錄 client 原始 body：

- `invalid_json`
- `missing_model`
- `model_not_allowed`
- `model_not_found`
- capability reject
- rate limit / quota reject
- `no_healthy_backend`

這違反規格的 privacy default：預設不記 input/output，raw logging 必須明確開啟。

修改要求：

- 所有呼叫 `recordLog()` 的錯誤路徑都必須先通過 logging policy。
- 可新增 helper：

```go
func (h *Handler) rawRequestForLog(k *store.APIKey, raw []byte) []byte
```

只有 `log_raw_request=true` 才回傳 raw，否則回 nil。

- `recordLog()` 本身也可加防呆，避免呼叫端忘記 policy。
- 測試要覆蓋錯誤路徑，而不只成功 alias rewrite 路徑。

驗收測試：

- 新增 `TestGatewayErrorsDoNotPersistRawRequestWhenDisabled`。
- 新增 `TestGatewayErrorsPersistRawRequestWhenEnabled`。
- 分別覆蓋 `invalid_json`、`missing_model`、`model_not_allowed`、`no_healthy_backend` 至少其中幾種。

### P0-3 trusted proxy 後 client IP 仍可被 X-Forwarded-For spoof

位置：

- `internal/netutil/clientip.go:54-67`
- `internal/netutil/clientip.go:137-155`
- `internal/auth/middleware.go:97-112`

問題：

目前只要 immediate peer 是 trusted proxy，就直接取 `X-Forwarded-For` 第一個 IP：

```go
if ip := parseFirstForwardedFor(r.Header.Get("X-Forwarded-For")); ip != "" {
    return ip
}
```

在很多 proxy 預設行為中，proxy 會 append 而不是 overwrite `X-Forwarded-For`。惡意 client 可以先送：

```text
X-Forwarded-For: 10.0.0.5
```

trusted proxy append 後變成：

```text
X-Forwarded-For: 10.0.0.5, 203.0.113.9
```

Gateway 目前會取 `10.0.0.5`，造成 API key `allowed_client_ips` 可被繞過。

修改要求：

- client IP extraction 必須使用 trusted proxy chain algorithm。
- 建議邏輯：
  - immediate `RemoteAddr` 必須是 trusted proxy，否則完全忽略 XFF。
  - 把 XFF 切成 IP list。
  - 從右往左掃描，跳過 trusted proxies。
  - 遇到第一個 untrusted IP，即為真實 client IP。
  - 若全部都是 trusted，取最左邊或 remote peer，但要有明確測試。
- `X-Real-IP` 也只能在 trusted proxy 情況下使用，且優先級需明確。

驗收測試：

- 新增 `TestClientIPExtractorUsesRightmostUntrustedXFF`。
- 新增 `TestClientIPExtractorIgnoresSpoofedLeftmostXFFBehindTrustedProxy`。
- 新增 multi-hop trusted proxy 測試。
- API key allowed/denied IP 測試要用 trusted proxy + spoofed XFF 驗證不能繞過。

## P1 必修：企業級驗收缺口

### P1-1 client_ip log / stats 覆蓋不完整

位置：

- `internal/auth/middleware.go:60-112`
- `internal/handlers/handlers.go:162-179`
- `internal/logstore/logstore.go:78-89`
- `internal/logstore/sqlite.go:214-265`
- `internal/logstore/postgres.go:193-255`

問題：

使用者新增需求是：

> 所有的 log 與統計要記錄 CLIENT IP，API KEY 可以定義限制可以連線的 CLIENT IP。

目前成功請求與部分 handler 內 gateway error 會記 `client_ip`，但 auth middleware 直接拒絕的請求不會進 handler，也不會進 persistent request log，包括：

- missing Authorization
- Authorization prefix 錯誤
- invalid API key
- disabled API key
- expired API key
- `client_ip_not_allowed`

另外 `Stats` 目前只有：

```go
ByModel
ByBackend
ByAPIKey
```

沒有 `ByClientIP`。`report.md` 說用 `/admin/logs?client_ip=` filter 代替 aggregation，但這不等於統計記錄 client IP。

修改要求：

- auth middleware 直接拒絕的 API request 也要能寫 persistent request log，至少要包含：
  - request_id
  - client_ip
  - endpoint
  - status_code
  - error_code
  - api_key_id 若已解析
- `logstore.Stats` 新增 `ByClientIP`。
- memory/sqlite/postgres `StatsSince()` 都要 group by `client_ip`。
- Admin `/admin/stats/range` 回傳 `by_client_ip`。
- Dashboard analytics 顯示 top client IPs 或至少能查詢。

驗收測試：

- 新增 `TestAuthRejectedRequestsArePersistentlyLoggedWithClientIP`。
- 新增 `TestClientIPNotAllowedIsLoggedWithClientIP`。
- 新增 `TestStatsAggregateByClientIP`，要求 `StatsSince().ByClientIP` 有資料。
- sqlite / postgres schema migration 都要包含 client_ip index 與 aggregation。

### P1-2 Admin 新增 backend 後不會被週期 health check 接管

位置：

- `internal/backend/health.go:72-82`
- `internal/backend/health.go:275-278`
- `internal/admin/admin.go:405-407`
- `internal/admin/admin.go:493-500`

問題：

`HealthChecker.Start()` 啟動時只對當下 store 裡的 backends spawn goroutine：

```go
for _, b := range h.store.Backends() {
    h.spawn(b)
}
```

註解寫「Backends added after start are picked up by Rescan()」，但程式沒有 `Rescan()`。Admin create backend 只呼叫一次：

```go
s.health.CheckOnce(bk)
```

因此 runtime 新增的 backend 只會被檢查一次，之後不會持續 health check。這會造成：

- dashboard health 狀態不更新。
- unhealthy backend 可能長期維持舊狀態。
- backend 異常 mail notification 可能漏發。

修改要求：

- 實作 `HealthChecker.Rescan()` 或 `WatchStore()`。
- Admin `createBackend` 後要啟動該 backend 的週期 health loop。
- Admin `deleteBackend` 後要停止對該 backend 的 loop，避免 goroutine leak。
- Admin patch health_check interval/enabled/type 時，scheduler 要能更新，至少重啟該 backend loop。
- 避免同一 backend 被 spawn 多個 goroutine。

驗收測試：

- 新增 `TestAdminCreatedBackendGetsPeriodicHealthChecks`。
- 新增 `TestHealthCheckerDoesNotSpawnDuplicateLoopsForSameBackend`。
- 新增 `TestDeletedBackendStopsHealthLoop` 或用 fake probe counter 驗證。

### P1-3 SMTP STARTTLS 不可默默降級 plaintext

位置：

- `internal/notify/notify.go:237-263`
- `internal/notify/notify.go:266-285`

問題：

當 config 設定：

```yaml
start_tls: true
```

但 SMTP server 沒有 advertise `STARTTLS` 時，目前程式會略過 TLS upgrade，繼續做 AUTH / MAIL / DATA。這會讓 SMTP 帳密與告警內容可能以 plaintext 傳送。

修改要求：

- `start_tls=true` 時，若 server 不支援 STARTTLS，必須 return error。
- 若 `auth != nil` 且沒有 TLS，應拒絕或至少要有明確 config 允許 insecure auth，預設拒絕。
- error 應進 notifier `LastResult()` 與 structured log。

驗收測試：

- 新增 `TestSMTPSenderStartTLSRequiredFailsWhenUnsupported`。
- 新增 `TestSMTPSenderDoesNotAuthOverPlaintextByDefault`。

### P1-4 notification cooldown 目前會壓掉失敗重試

位置：

- `internal/notify/notify.go:157-183`

問題：

`dispatch()` 在真正 send 之前就寫入 `lastSent`：

```go
n.lastSent[dedupeKey] = time.Now()
```

如果 SMTP send 失敗，接下來 cooldown 期間相同 backend/kind 的事件都會被 suppress。也就是第一次告警發送失敗後，真正的 backend 異常可能在 cooldown 內完全不再通知。

修改要求：

- 只有 send 成功後才更新 `lastSent`。
- 或拆成 `lastAttempt` / `lastSuccess`，失敗重試用較短 cooldown。
- `LastResult()` 要能區分 last attempt 與 last success。

驗收測試：

- 新增 `TestNotifyFailureDoesNotStartSuccessCooldown`。
- 新增失敗後下一次事件會再嘗試送信的測試。

### P1-5 PostgreSQL 目前只存 logs/audit，不存 config metadata

位置：

- `cmd/gateway/main.go:120-172`
- `internal/store/store.go:381-470`
- `internal/admin/admin.go:374-410`
- `internal/admin/admin.go:440-500`
- `internal/admin/admin.go:741-788`

問題：

規格要求 PostgreSQL 儲存 config / metadata，例如：

- api_keys
- backends
- models
- model_aliases
- audit_logs
- request_logs

目前 `storage.driver=postgres` 只切換 request_logs / audit_logs 的 logstore。核心 config metadata 仍然是：

```go
s := store.New(...)
s.LoadFromConfig(cfg)
```

Admin API 對 backend/model/alias/api_key 的修改只更新 in-memory store，Gateway 重啟後全部消失。

如果此階段只打算做 logstore，請在 README/report 裡明確標註「metadata persistence 尚未完成」。如果要符合 enterprise MVP，則必須補 metadata store。

修改要求：

- 定義 metadata Store interface，至少支援：
  - backends CRUD
  - models CRUD
  - model_aliases CRUD
  - api_keys CRUD
- Postgres driver 實作對應 schema/migration。
- 啟動時從 Postgres 載入 metadata，config yaml 可作 bootstrap seed。
- Admin API mutation 必須 persist 到 Postgres。
- 多 replica 時 config mutation 要有同步策略，最少要靠 DB 查詢或 cache invalidation。

驗收測試：

- 新增 `TestPostgresMetadataPersistsAPIKeyAcrossRestart`。
- 新增 `TestPostgresMetadataPersistsBackendAcrossRestart`。
- 新增 Admin API create/patch/delete 後重建 store 仍能讀到資料的 integration test。

### P1-6 health check `success_threshold` 對 unknown 狀態被繞過

位置：

- `internal/store/store.go:98-123`
- `internal/store/store_test.go:96-117`

問題：

`RecordHealthCheck()` 成功時先依 `success_threshold` 判斷，但接著對 `unknown` 直接設為 healthy：

```go
if prev == "" || prev == StatusUnknown {
    b.status = StatusHealthy
}
```

因此從 unknown 開始時，第一次成功就 healthy，`success_threshold` 沒有被遵守。測試註解說「3 successes」，但實際 expected 是 2 successes，且程式第一下就會變 healthy。

修改要求：

- 明確定義 unknown -> healthy 是否要遵守 success_threshold。
- 若規格要求 threshold，就移除 unknown 特例。
- 若想保留 startup fast-ready，應新增 config，並在 report/README 說明。

驗收測試：

- 新增 `TestUnknownBackendRequiresSuccessThreshold` 或相反語意的測試，避免目前隱性繞過。

## P2 建議修正：一致性、可維運性與測試完整度

### P2-1 Admin POST model/alias 沒有驗證 mode

位置：

- `internal/admin/admin.go:596-624`
- `internal/admin/admin.go:657-681`
- `internal/admin/admin_models.go:58-70`
- `internal/admin/admin_models.go:107-117`
- `internal/capability/capability.go:25-32`

問題：

PATCH 已驗證：

- `capability_mode` must be `passthrough|declared|strict`
- `forwarding_mode` must be `use_internal|keep_external`

但 POST / upsert path 仍可寫入任意值。未知 `capability_mode` 進到 `capability.Check()` 會被當 passthrough，造成 UI/API 看起來設定成功但實際不生效。

修改要求：

- POST 與 PATCH 共用 validation helper。
- invalid mode 回 400。
- 既有 config load 也建議 validate mode。

驗收測試：

- `TestAdminModelPostRejectsInvalidCapabilityMode`
- `TestAdminAliasPostRejectsInvalidForwardingMode`
- `TestConfigRejectsInvalidCapabilityMode`

### P2-2 `/v1/models` alias permission 顯示與實際 request 權限可能不一致

位置：

- `internal/handlers/handlers.go:82-149`
- `internal/store/store.go:560-585`

問題：

`/v1/models` 對 alias 顯示只檢查 alias 名稱：

```go
if apiKey == nil || apiKey.ModelAllowed(name) {
    ids[name] = true
}
```

但實際 request path 使用 `ModelAllowedResolved(requested, internal)`。如果 alias allowed 但 internal denied，`/v1/models` 可能顯示可用，實際呼叫卻 403。

修改要求：

- `/v1/models` 顯示 alias 時也使用 alias resolved 後的權限語意。
- Denied internal model 必須讓 alias 從 `/v1/models` 中消失。

驗收測試：

- `TestListModelsDoesNotShowAliasDeniedByInternalModel`

### P2-3 healthcheck subcommand 沒吃 `LLMGATEWAY_LISTEN`

位置：

- `cmd/gateway/main.go:54-75`
- `cmd/gateway/main.go:298-315`

問題：

`--healthcheck` 在 env override 前就執行：

```go
cfg, err := config.Load(...)
if *healthcheck {
    os.Exit(runHealthCheck(cfg))
}
```

如果 runtime 用 `LLMGATEWAY_LISTEN` 改 port，healthcheck 仍會打 yaml 裡的 port。

修改要求：

- env override 必須在 `runHealthCheck(cfg)` 前套用。
- 建議把 env override 抽成 `applyEnvOverrides(cfg)`，main 與 healthcheck 共用。

驗收測試：

- `TestHealthCheckUsesListenEnvOverride`

### P2-4 SMTP config 說建議用 SMTP_PASSWORD，但程式沒有讀

位置：

- `config/gateway.yaml:108-117`
- `internal/config/config.go:307-322`
- `cmd/gateway/main.go:64-75`

問題：

sample config 註解寫：

```yaml
# SMTP_PASSWORD should be supplied via env, not inline.
```

但程式沒有讀 `SMTP_PASSWORD` 或 `LLMGATEWAY_SMTP_PASSWORD`，也沒有 `os.ExpandEnv()`。使用者若照註解設定 env，實際 password 仍是空字串。

修改要求：

- 支援 `LLMGATEWAY_SMTP_PASSWORD` override。
- 或支援 config value `${SMTP_PASSWORD}` 展開。
- README / config 註解要與實作一致。

驗收測試：

- `TestSMTPPasswordCanBeLoadedFromEnv`

### P2-5 Admin audit client IP 沒有使用同一套 trusted proxy policy

位置：

- `internal/admin/admin.go:267-306`
- `internal/netutil/clientip.go`

問題：

request log / auth 使用 `netutil.Extractor`，但 audit log 使用 admin package 自己的 `clientIP()`，而且直接信任 XFF 第一個值：

```go
if v := r.Header.Get("X-Forwarded-For"); v != "" {
    return strings.TrimSpace(v[:idx])
}
```

這會讓 audit log 的 admin IP 與 request log policy 不一致，也可被 spoof。

修改要求：

- Admin server 也注入同一個 `netutil.Extractor`。
- audit log IP 必須使用 trusted proxy policy。
- 未信任 proxy 時忽略 XFF。

驗收測試：

- `TestAdminAuditIgnoresUntrustedXForwardedFor`
- `TestAdminAuditUsesTrustedProxyClientIP`

## 必補測試總表

Claude Code 請至少新增或修正以下測試：

- `TestHandlerTokensPerMinuteBlocksAfterUsage`
- `TestHandlerDailyTokenLimitBlocksAfterUsage`
- `TestGatewayErrorsDoNotPersistRawRequestWhenDisabled`
- `TestGatewayErrorsPersistRawRequestWhenEnabled`
- `TestClientIPExtractorUsesRightmostUntrustedXFF`
- `TestClientIPExtractorIgnoresSpoofedLeftmostXFFBehindTrustedProxy`
- `TestAuthRejectedRequestsArePersistentlyLoggedWithClientIP`
- `TestClientIPNotAllowedIsLoggedWithClientIP`
- `TestStatsAggregateByClientIP`
- `TestAdminCreatedBackendGetsPeriodicHealthChecks`
- `TestHealthCheckerDoesNotSpawnDuplicateLoopsForSameBackend`
- `TestDeletedBackendStopsHealthLoop`
- `TestSMTPSenderStartTLSRequiredFailsWhenUnsupported`
- `TestSMTPSenderDoesNotAuthOverPlaintextByDefault`
- `TestNotifyFailureDoesNotStartSuccessCooldown`
- `TestPostgresMetadataPersistsAPIKeyAcrossRestart`
- `TestPostgresMetadataPersistsBackendAcrossRestart`
- `TestUnknownBackendRequiresSuccessThreshold` 或明確相反語意測試
- `TestAdminModelPostRejectsInvalidCapabilityMode`
- `TestAdminAliasPostRejectsInvalidForwardingMode`
- `TestConfigRejectsInvalidCapabilityMode`
- `TestListModelsDoesNotShowAliasDeniedByInternalModel`
- `TestHealthCheckUsesListenEnvOverride`
- `TestSMTPPasswordCanBeLoadedFromEnv`
- `TestAdminAuditIgnoresUntrustedXForwardedFor`
- `TestAdminAuditUsesTrustedProxyClientIP`

## 驗收 Checklist

修正後請逐項驗收：

- `go test ./...` 通過。
- `go test -race ./...` 通過。
- `go vet ./...` 通過。
- Docker build 通過。
- token rate limit / daily token quota 會在 usage 累積後阻擋下一次請求。
- `log_raw_request=false` 時，成功與錯誤路徑都不保存 raw request。
- `log_raw_request=true` 時，成功與錯誤路徑保存 client 原始 body。
- trusted proxy 後 XFF spoof 不能繞過 API key IP allow/deny。
- API key `allowed_client_ips` / `denied_client_ips` deny 優先，支援 IPv4/IPv6/CIDR。
- missing/invalid/disabled/expired API key 與 `client_ip_not_allowed` 都會記 persistent request log，且包含 client_ip。
- Admin stats / dashboard analytics 能依 client_ip 聚合。
- Runtime 新增 backend 後會持續 health check。
- Runtime 刪除 backend 後不再 health check，沒有 goroutine leak。
- backend unhealthy/degraded/recovered mail notification 能觸發，且 cooldown 不會壓掉失敗重試。
- `start_tls=true` 不會降級 plaintext。
- Postgres 若宣稱 enterprise metadata storage，重啟後 admin-created backend/model/api_key 仍存在。
- `/v1/models` 顯示結果與實際 request 權限一致。
- Admin audit log 的 IP 使用與 API request 相同的 trusted proxy policy。
