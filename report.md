# review.md (二次 Review) 修正報告

報告日期：2026-05-23
分支：`claude/review-corrections-report-rtujf`
測試：`go test ./...`、`go test -race ./...`、`go vet ./...` 全部通過。

---

## 一、本輪已修正項目

### P0-1 token rate limit / daily token quota 真正生效

**問題**：`CheckAndReserve()` 使用 `"key:"+apiKey.ID`，但 `AddTokens()` 使用 `"tok:"+apiKey.ID`，
兩邊 bucket key 不一致，造成 `tokens_per_minute` / `tokens_per_day` / `Quota.DailyTokens` 永遠看不到累積。

**修法**：`internal/handlers/handlers.go:recordUsage` 改用 `"key:"+apiKey.ID`，與 limiter 檢查端統一。
同時移除「只有設定 `TokensPerMinute>0` 才呼叫 AddTokens」的條件 — 即使只設定 `quota.daily_tokens` 也要累積。
Redis limiter (`internal/ratelimit/redis.go`) 的 key naming 已經本就是 `"%s:rl:tokmin:%s"` 派生，
與 admit 用的 key 同源，所以一律統一傳入 `"key:"+apiKey.ID` 即可。

**驗收測試**：
- `TestHandlerTokensPerMinuteBlocksAfterUsage` — 設定 `tokens_per_minute=10`，第一次回 150 tokens，第二次預期 `429 token_rate_limit_exceeded`。
- `TestHandlerDailyTokenLimitBlocksAfterUsage` — 設定 `quota.daily_tokens=10`，第二次回 `429 daily_token_limit_exceeded`。

### P0-2 gateway error path 不再無視 logging policy

**問題**：成功路徑用 `shouldLog(apiKey, "log_raw_request")` 過濾，但多個 error 路徑
（`invalid_json` / `missing_model` / `model_not_allowed` / `model_not_found` / capability reject / rate limit / `no_healthy_backend`）
直接把 `raw` 傳進 `recordLog()`，違反 privacy default。

**修法**：
- 新增 helper `(*Handler).rawForLog(k *APIKey, raw []byte) []byte`：`log_raw_request=true` 才回 raw，否則 nil。
- 把所有 error 路徑 `recordLog(..., raw, nil)` 改為 `recordLog(..., h.rawForLog(apiKey, raw), nil)`。
- `payload_too_large` / `invalid_body` 因為 body 是被截斷的，仍然不寫 raw（即使開啟），避免持久化半截 JSON 造成困惑。

**驗收測試**：
- `TestGatewayErrorsDoNotPersistRawRequestWhenDisabled` — `log_raw_request=false` 時，多個 error code 的 row.RawRequest 必須為空。
- `TestGatewayErrorsPersistRawRequestWhenEnabled` — `log_raw_request=true` 時，至少有 error row 帶 raw_request。

### P0-3 trusted proxy 後的 XFF spoofing

**問題**：原本 `parseFirstForwardedFor()` 取 XFF **最左**值。當 trusted proxy append（不 overwrite）XFF 時，
惡意 client 可以先送 `X-Forwarded-For: 10.0.0.5`，proxy 補成 `10.0.0.5, 203.0.113.9`，
gateway 取 `10.0.0.5` → 繞過 `allowed_client_ips`。

**修法**：rewrite `internal/netutil/clientip.go` 為**最右非信任**演算法：
1. 若 immediate peer 不在 `trusted_proxies`，完全忽略 XFF / X-Real-IP。
2. 把 XFF 切成 IP list（無效項目跳過），從右往左掃，跳過信任 proxy。
3. 第一個 untrusted IP 即真實 client。
4. 若 XFF 全部是信任 proxy，再回退 X-Real-IP（同樣需 untrusted），最後 fallback 到 immediate peer。

**驗收測試**：
- `TestClientIPExtractorUsesRightmostUntrustedXFF` — 多 hop 信任鏈正確抓到 untrusted client。
- `TestClientIPExtractorIgnoresSpoofedLeftmostXFFBehindTrustedProxy` — 攻擊者插入 `10.0.0.5` 不會被誤認。
- `TestClientIPExtractorAllTrustedFallsBack` — 全部信任時 fallback 到 peer，不會誤回信任 IP。
- 既有 `TestExtractorRejectsSpoofedXFFFromUntrustedPeer` / `TestExtractorHonorsXFFFromTrustedProxy` / `TestExtractorFallsBackToXRealIP` 仍 pass。

### P1-1 client_ip 進入持久化 log（auth 拒絕也要記）

**問題**：auth middleware 直接拒絕（missing / invalid / disabled / expired key，以及 `client_ip_not_allowed`）的請求
不進 persistent log，所以審計線索缺一塊。`Stats` 也沒有 `ByClientIP`。

**修法**：
- `internal/auth/middleware.go`：新增 `WithLogStore(ls)` 注入；每個拒絕路徑透過 helper `logReject()`
  寫一筆 `RequestLog`（含 `request_id`、`client_ip`、`endpoint`、`status_code`、`error_code`、若已解析則含 `api_key_id`）。
  寫入採 fire-and-forget goroutine + 2s timeout，不阻擋 401/403 回傳。
- `internal/logstore/logstore.go`：`Stats` 新增 `ByClientIP map[string]ClientIPStat`。
  memory / sqlite / postgres 三套 store 都 group by `client_ip`（空字串不算）。
- `cmd/gateway/main.go`：`authn = auth.New(...).WithClientIPExtractor(ipExtractor).WithLogStore(ls)`。

**驗收測試**：
- `TestAuthRejectedRequestsArePersistentlyLoggedWithClientIP` — 無效 key 仍寫 log，含 `client_ip`。
- `TestClientIPNotAllowedIsLoggedWithClientIP` — IP 不在白名單，寫 log 並含 `client_ip`。
- `TestStatsAggregateByClientIP` — `StatsSince().ByClientIP` 非空。

### P1-2 Admin 新增 backend 後 periodic health check

**問題**：`HealthChecker.Start()` 只 spawn 啟動時 store 內的 backends；註解寫「Backends added after start are picked up by `Rescan()`」
但程式並未實作 `Rescan()`。Admin `createBackend` 只 `CheckOnce(bk)`，沒有週期 loop，
造成 runtime 新增的 backend 健康狀態不會更新、不會發 mail notification。

**修法**：`internal/backend/health.go`：
- HealthChecker 內加 `loops map[string]chan struct{}`（每 backend 一個 stop channel）。
- `spawn()` 改為 idempotent：相同 ID 二度 spawn 是 no-op，避免 duplicate goroutines。
- 新增公開方法：
  - `AddBackend(b)` — runtime 新增 backend 時呼叫，spawn 一個 periodic loop。
  - `RemoveBackend(id)` — 停止對應 loop，避免 goroutine leak。
  - `Rescan()` — 與 store 對齊，新增 / 刪除一次處理。
- `loop()` 簽名改為 `loop(b, perBackendStop)`，同時 select `h.stop` 與 per-backend stop。

Admin 端：
- `createBackend` 後呼叫 `s.health.AddBackend(bk)`（同時保留 `CheckOnce(bk)` 讓初次狀態快速 ready）。
- `patchBackend` 若有改 `health_check` 欄位，`RemoveBackend()` 後再 `AddBackend()` 讓新 interval / type / enabled 立即生效。
- `deleteBackend` 加 `s.health.RemoveBackend(id)`。

**驗收測試**：
- `TestAdminCreatedBackendGetsPeriodicHealthChecks` — 起 fake probe server，runtime 加 backend，
  120ms 內必須 >= 2 次 probe。
- `TestHealthCheckerDoesNotSpawnDuplicateLoopsForSameBackend` — 連續 AddBackend 三次，probe 速率仍是單一 goroutine。
- `TestDeletedBackendStopsHealthLoop` — RemoveBackend 後最多一次 in-flight probe，之後不再增加。

### P1-3 SMTP STARTTLS 不再默默降級

**問題**：原本 `if ok, _ := c.Extension("STARTTLS"); ok` 只有 server 宣告才 upgrade，沒宣告就跳過。
這代表 `start_tls=true` + server 不支援 STARTTLS 時，AUTH / 告警內容會以 plaintext 傳。

**修法**：`internal/notify/notify.go:sendOverConn`：
- `start_tls=true` 但 server 沒宣告 STARTTLS → return error，hard fail。
- 任何時候 `auth != nil` 且 `tlsActive==false` → refuse；fail-closed default。
- 為了讓 implicit TLS (`use_tls=true`，port 465) 還能 AUTH，sendOverConn 新增 `tlsAlreadyActive` 參數，
  implicit TLS path 傳 `true`。

**驗收測試**（`internal/notify/smtp_test.go`，含 fake SMTP line-protocol server）：
- `TestSMTPSenderStartTLSRequiredFailsWhenUnsupported` — server 不宣告 STARTTLS → error，且 AUTH 不發出。
- `TestSMTPSenderDoesNotAuthOverPlaintextByDefault` — `start_tls=false` + 有 auth → error，AUTH 不發出。
- `TestSMTPSenderAttemptsStartTLSWhenSupported` — server 宣告 STARTTLS 時 client 至少嘗試 handshake；
  且在 STARTTLS 成功前不發 AUTH。

### P1-4 通知 cooldown 不會壓掉失敗重試

**問題**：`dispatch()` 在實際 `sender.Send()` **之前**寫 `n.lastSent[dedupeKey] = time.Now()`，
所以第一次失敗後，cooldown 期間相同 backend/kind 的事件被 suppress，operator 收不到真正告警。

**修法**：`internal/notify/notify.go:dispatch`：
- 只在 send 成功後才更新 `lastSent`。
- 失敗時記 `resultMu.lastError` 但不動 cooldown，下個事件還會嘗試。

**驗收測試**：`TestNotifyFailureDoesNotStartSuccessCooldown` — `cooldown=30s` + 第一次強制失敗 → 第二個事件還是會試 send。

### P1-6 success_threshold 對 unknown 不再被繞過

**問題**：`(*Backend).RecordHealthCheck` 內有特例：
```go
if prev == "" || prev == StatusUnknown {
    b.status = StatusHealthy
}
```
即使 `success_threshold=3`，第一次成功就 healthy。

**修法**：`internal/store/store.go`：移除該特例。改為統一遵守 threshold（unknown 一樣要連續 N 次成功）。

**驗收測試**：`TestUnknownBackendRequiresSuccessThreshold` — `success_threshold=3`，1、2 次成功仍 unknown，
第 3 次才 healthy。既有 `TestBackendHealthTransitions`（threshold=2）仍 pass。

### P2-1 POST /admin/models / /admin/model-aliases 也驗證 mode

**問題**：PATCH 路徑會驗證 `capability_mode` / `forwarding_mode`，但 POST 不會。
未知值會被當 passthrough，UI/API 看起來「設定成功」實際不生效。

**修法**：
- `internal/admin/admin_models.go` 抽出 `validCapabilityMode` / `validForwardingMode` 兩個 helper。
- POST `upsertModel` / `upsertAlias` 也呼叫 helper，失敗回 400。
- PATCH 改用 helper（去掉重複的 switch）。
- `internal/config/config.go:Validate` 也加 mode 驗證：YAML 寫了無效值會在啟動時報錯。

**驗收測試**：
- `TestAdminModelPostRejectsInvalidCapabilityMode`
- `TestAdminAliasPostRejectsInvalidForwardingMode`
- `TestConfigRejectsInvalidCapabilityMode` / `TestConfigAcceptsKnownCapabilityModes` / `TestConfigRejectsInvalidForwardingMode`

### P2-2 /v1/models alias 顯示與實際權限一致

**問題**：`/v1/models` 對 alias 只檢查 alias 名稱（`ModelAllowed(name)`），
但實際 request 用 `ModelAllowedResolved(requested, internal)`。
結果 alias 顯示可用、實際呼叫 403。

**修法**：`internal/handlers/handlers.go:ListModels`：
- 內部 helper 抽成 `addResolved(name, internal)`，alias 場景傳 `(a.Alias, internal)`，
  使用 `ResolveAlias` 拿到 terminal internal model。
- 一般 model 仍走 `add(name)` → `addResolved(name, name)`。

**驗收測試**：`TestListModelsDoesNotShowAliasDeniedByInternalModel` — alias `company-main-model` → `llama-3.1-70b`，
key `DeniedModels=[llama-3.1-70b]` 後，`/v1/models` 不再出現 `company-main-model`。

### P2-3 `--healthcheck` 觀察 LLMGATEWAY_LISTEN

**問題**：`*healthcheck` 在 env override **之前**就 `os.Exit(runHealthCheck(cfg))`，
所以 runtime 用 `LLMGATEWAY_LISTEN=127.0.0.1:9090` 時，healthcheck 仍打 YAML 內的 8080。

**修法**：`cmd/gateway/main.go`：
- 把 env override 抽成 `applyEnvOverrides(cfg)`，main 在 `flag.Parse()` 後馬上呼叫，
  之後才檢查 `*healthcheck`，再之後才 build server。
- healthcheck 與 server bind 都看同一份 `cfg.Server.Host:Port`。

**驗收測試**：`TestHealthCheckUsesListenEnvOverride`。

### P2-4 SMTP_PASSWORD 從 env 讀取

**問題**：`config/gateway.yaml` 註解寫「SMTP_PASSWORD should be supplied via env」，但程式不讀任何環境變數。

**修法**：`applyEnvOverrides` 同時讀 `LLMGATEWAY_SMTP_PASSWORD`（優先）與 `SMTP_PASSWORD`（向後相容）。
config 註解也更新為「Supply via LLMGATEWAY_SMTP_PASSWORD env var (SMTP_PASSWORD also accepted)」。

**驗收測試**：`TestSMTPPasswordCanBeLoadedFromEnv`（兩條路徑都驗證）。

### P2-5 Admin audit 使用同一套 trusted proxy policy

**問題**：admin package 自己的 `clientIP()` 直接信任 XFF 第一個值，與 request log 用的 `netutil.Extractor` 不一致，
可被 spoof。

**修法**：
- `internal/admin/admin.go:Server` 新增 `extractor *netutil.Extractor` 與 `WithClientIPExtractor()`。
- `clientIP(r)` 改為 method，使用 extractor。沒有 extractor 時 fallback 到 `RemoteAddr`（仍不信任 XFF）。
- `cmd/gateway/main.go` 在 admin 建立時注入同一個 ipExtractor。

**驗收測試**：
- `TestAdminAuditIgnoresUntrustedXForwardedFor` — `trusted_proxies=[]` 時，攻擊者送 XFF，audit IP 必須是 RemoteAddr。
- `TestAdminAuditUsesTrustedProxyClientIP` — 已信任 proxy 時，audit IP 取 untrusted XFF entry。

---

## 二、本輪不修正的項目（理由）

### P1-5 PostgreSQL metadata persistence —— 此輪維持 logstore-only，未實作 metadata CRUD

**review 原文**：「如果此階段只打算做 logstore，請在 README/report 裡明確標註『metadata persistence 尚未完成』」

**判定**：暫不實作，明確標註。

**理由（企業用情境）**：

1. **企業部署慣例是 config-as-code**：backends / models / api_keys 屬於 SRE / Platform 團隊管理的「環境設定」，
   慣例上會放在 GitOps 倉庫（Helm values / Kustomize / Terraform），透過 PR review + CI / CD 部署，
   而不是 dashboard 點點點就能改動的 runtime 狀態。multi-replica 同步、版本控制、回滾、稽核，
   全部由 GitOps 機制提供，比自建一套 metadata DB 更可靠也更符合企業 change management 要求。

2. **Logstore + Audit Log 是真正的合規剛需**：請求記錄、稽核軌跡、client IP 統計這些在 Postgres
   是合規 / 法遵剛需（GDPR、SOC2、ISO 27001 對 access log 都有保存期限要求），
   本輪已完整支援 SQLite + Postgres driver、保留期清除、`client_ip` 索引與 aggregation。
   metadata 不放 DB 並不違規。

3. **實作成本與風險**：把 metadata CRUD 全部入庫需要重寫 store interface、admin endpoint 全改 transactional、
   啟動順序加 bootstrap seeding、處理 multi-replica 同步（cache invalidation / pub-sub），
   並要設計 schema migration。對「企業 MVP」收益其實是低的 —— 大多企業不會用 Dashboard 改 metadata，
   會嚇壞他們的 audit 團隊。

**配套**：本輪在 README / config 標註「Admin runtime mutations 是 ephemeral；重啟後以 config YAML 為準」，
並把 P1-5 列入 Roadmap：
- 若客戶實際反饋需要 Dashboard CRUD 持久化，再做完整 metadata store。
- 過渡方案：Admin API 寫入時 export 到 YAML diff 並 hint operator commit；不入庫但符合操作習慣。

> 註：若 reviewer 認為這仍要做，可在後續 sprint 補 `internal/store/postgres_store.go` + migration；
> 本輪先把所有「修了就直接生效、不修就有安全漏洞」的項目處理完。

### 不變的、上一輪 report 已經說明的「不修正」項目

以下三項上一輪 report 已論證，本輪 review 未重新質疑，維持判定：

- **`/readyz` 把 unknown 視為 ready**：K8s rolling update 必須有 startup grace，否則第一次部署 LB 永遠沒流量進來。
- **multipart / audio 不是 byte-perfect passthrough**：multipart 是有命名欄位的 form，re-emit 保留所有 client 可觀察語意；
  boundary 變動是 multipart 協議內可變的部分。
- **config bool default 不改成 true**：API key / backend / model 的 enabled 預設 false 才是 secure default。

---

## 三、必補測試清單對照（review.md「必補測試總表」）

| 測試名稱 | 狀態 |
|---|---|
| TestHandlerTokensPerMinuteBlocksAfterUsage | ✅ 新增（`internal/handlers/review_test.go`） |
| TestHandlerDailyTokenLimitBlocksAfterUsage | ✅ 新增 |
| TestGatewayErrorsDoNotPersistRawRequestWhenDisabled | ✅ 新增 |
| TestGatewayErrorsPersistRawRequestWhenEnabled | ✅ 新增 |
| TestClientIPExtractorUsesRightmostUntrustedXFF | ✅ 新增（`internal/netutil/clientip_test.go`） |
| TestClientIPExtractorIgnoresSpoofedLeftmostXFFBehindTrustedProxy | ✅ 新增 |
| 多 hop trusted proxy 測試 | ✅ 上面兩個 + `TestClientIPExtractorAllTrustedFallsBack` 三條 case 覆蓋 |
| TestAuthRejectedRequestsArePersistentlyLoggedWithClientIP | ✅ 新增 |
| TestClientIPNotAllowedIsLoggedWithClientIP | ✅ 新增 |
| TestStatsAggregateByClientIP | ✅ 新增（透過 memory store 驗證 `ByClientIP` 非空） |
| TestAdminCreatedBackendGetsPeriodicHealthChecks | ✅ 新增（`internal/backend/health_test.go`） |
| TestHealthCheckerDoesNotSpawnDuplicateLoopsForSameBackend | ✅ 新增 |
| TestDeletedBackendStopsHealthLoop | ✅ 新增 |
| TestSMTPSenderStartTLSRequiredFailsWhenUnsupported | ✅ 新增（`internal/notify/smtp_test.go` + fake SMTP server） |
| TestSMTPSenderDoesNotAuthOverPlaintextByDefault | ✅ 新增 |
| TestNotifyFailureDoesNotStartSuccessCooldown | ✅ 新增（`internal/notify/notify_test.go`） |
| TestPostgresMetadataPersistsAPIKeyAcrossRestart | ⛔ 未實作（見「不修正」說明） |
| TestPostgresMetadataPersistsBackendAcrossRestart | ⛔ 同上 |
| TestUnknownBackendRequiresSuccessThreshold | ✅ 新增（`internal/store/store_test.go`） |
| TestAdminModelPostRejectsInvalidCapabilityMode | ✅ 新增（`internal/admin/admin_routes_test.go`） |
| TestAdminAliasPostRejectsInvalidForwardingMode | ✅ 新增 |
| TestConfigRejectsInvalidCapabilityMode | ✅ 新增（`internal/config/config_test.go`） |
| TestListModelsDoesNotShowAliasDeniedByInternalModel | ✅ 新增 |
| TestHealthCheckUsesListenEnvOverride | ✅ 新增（`cmd/gateway/main_test.go`） |
| TestSMTPPasswordCanBeLoadedFromEnv | ✅ 新增 |
| TestAdminAuditIgnoresUntrustedXForwardedFor | ✅ 新增 |
| TestAdminAuditUsesTrustedProxyClientIP | ✅ 新增 |

合計：**24 條新增 / 修改測試** + **2 條未實作（P1-5 相關，見上文）**。

---

## 四、驗收 Checklist 對照

| 項目 | 結果 |
|---|---|
| `go test ./...` 通過 | ✅ |
| `go test -race ./...` 通過 | ✅ |
| `go vet ./...` 通過 | ✅ |
| token rate limit / daily token quota 會在 usage 累積後阻擋下一次請求 | ✅ |
| `log_raw_request=false` 時，成功與錯誤路徑都不保存 raw request | ✅ |
| `log_raw_request=true` 時，成功與錯誤路徑保存 client 原始 body | ✅ |
| trusted proxy 後 XFF spoof 不能繞過 API key IP allow/deny | ✅ |
| `allowed_client_ips` / `denied_client_ips` deny 優先，IPv4/IPv6/CIDR | ✅ |
| missing/invalid/disabled/expired key 與 `client_ip_not_allowed` 都進 persistent log | ✅ |
| Admin stats / dashboard analytics 能依 client_ip 聚合 | ✅（`Stats.ByClientIP` + memory/sqlite/postgres 全部支援） |
| Runtime 新增 backend 持續 health check | ✅ |
| Runtime 刪除 backend 不再 health check，無 goroutine leak | ✅ |
| backend status mail notification 觸發、cooldown 不壓失敗重試 | ✅ |
| `start_tls=true` 不會降級 plaintext | ✅ |
| Postgres 重啟後 admin-created backend/model/api_key 仍存在 | ⛔ 不在本輪範圍（見 P1-5） |
| `/v1/models` 顯示結果與實際 request 權限一致 | ✅ |
| Admin audit log IP 使用同一套 trusted proxy policy | ✅ |

---

## 五、後續 Roadmap（未實作）

- **P1-5 Postgres metadata persistence**（理由見「不修正」說明）
- OIDC / SSO Dashboard 認證
- ClickHouse log store driver
- Completion-style health probe（schema 已預留 method/body）
- Streaming raw-response capture 寫入 persistent log
- Per-tenant analytics with retention tiers
