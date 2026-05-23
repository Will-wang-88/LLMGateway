# review.md 修正報告

報告日期：2026-05-23
分支：`claude/review-corrections-report-Zg4tf`
測試：`go test ./...`、`go test -race ./...`、`go vet ./...` 全部通過。

## 一、已修正項目（含對應 regression test）

### P0-1 degraded backend 不再被 routing 使用
- `internal/handlers/handlers.go`：`filterRoutable` 只接受 `healthy` / `unknown`，
  degraded 預設排除；只有 `routing.allow_degraded_backends=true` 時才接受。
- `internal/config/config.go`：新增 `RoutingConfig.AllowDegradedBackends`。
- 測試：`TestDegradedBackendIsNotRoutable`、`TestDegradedBackendRoutableWhenConfigEnables`。

### P0-2 model alias 不再可繞過 internal model deny list
- `internal/store/store.go`：新增 `(*APIKey).ModelAllowedResolved(requested, internal)`，
  對外部名稱與 resolve 後的 internal 名稱同時檢查 deny / allow，deny 任一命中即拒絕。
- `internal/handlers/handlers.go` 與 `audio.go` 改為先 resolve alias 再呼叫
  `ModelAllowedResolved`。
- 測試：`TestAliasCannotBypassDeniedInternalModel`。

### P0-3 disabled model registry 真正阻擋 routing
- alias resolve 之後、backend lookup 之前檢查 `Model.Enabled`；false 則 404。
- 測試：`TestDisabledRegistryModelCannotBeForwarded`、
  `TestDisabledRegistryModelViaAliasIsRejected`。

### P0-4 client IP 稽核 / 統計 / API key IP 存取限制
- 新增 `internal/netutil/clientip.go`：trusted-proxy aware extractor + `MatchAny` 支援
  IPv4/IPv6/CIDR；未設定 trusted proxy 時 XFF / X-Real-IP 一律忽略。
- `ServerConfig.TrustedProxies`、`APIKeyConfig.AllowedClientIPs` / `DeniedClientIPs`。
- `internal/store/store.go`：`APIKey` 加上對應欄位、`LoadFromConfig` 一併載入。
- `internal/auth/middleware.go`：認證後執行 deny / allow IP policy；解析後的 client IP
  寫進 request context；deny 命中或 allow list 非空但不在白名單時回
  `403 client_ip_not_allowed`。
- `internal/logstore/{logstore,sqlite}.go`：`RequestLog.ClientIP` + `LogQuery.ClientIP`，
  SQLite schema 加 `client_ip` 欄位與 index，含 `ALTER TABLE ADD COLUMN` migration（
  容忍 duplicate column 錯誤以相容既有 DB）。
- `internal/admin/admin_extra.go`：`/admin/logs?client_ip=` 支援過濾。
- `internal/admin/{admin,admin_keys}.go`：APIKey CRUD（create/patch/list/get/summarize）
  全部暴露 `allowed_client_ips` / `denied_client_ips`。
- `cmd/gateway/main.go`：建立共享 `netutil.Extractor` 接到 Authenticator。
- 測試：`TestAPIKeyAllowedClientIPs`、`TestAPIKeyDeniedClientIPsTakePrecedence`、
  `TestClientIPExtractorDoesNotTrustSpoofedXForwardedFor`、
  `TestRequestLogsIncludeClientIP`、`internal/netutil/clientip_test.go` 4 個 test。

### P1-1 Authorization 必須帶 Bearer prefix
- `internal/auth/middleware.go`：prefix 非空時 header 未以 prefix 開頭直接回 401，
  不再容忍 raw key。
- 測試：`TestAuthorizationRequiresBearerPrefix`。

### P1-3 API key request stats 不再依賴 backend usage
- `internal/store/store.go`：拆分 `TouchRequest()`（+1 request + last_used）與
  `AddTokens()`（只加 tokens）；`Touch(tokens)` 保留以維持 API 相容。
- handler 每次請求完成都呼叫 `TouchRequest`（含 streaming、含 backend error、含無
  usage 的成功響應）；`recordUsage` 改用 `AddTokens` 避免重複計 request。
- 測試：`TestAPIKeyStatsIncrementWithoutUsage`。

### P1-4 raw_request 記錄 client 原始 body
- structured log 與 persistent log 在 `log_raw_request=true` 時改寫入 `raw`（client
  原始 body）；alias rewrite 後的 body 另以 `forwarded_request` 額外欄位記錄，
  互不污染。
- 測試：`TestRawRequestLogKeepsClientOriginalBodyWhenAliasRewrites`。

### P1-5 streaming idle timeout 零注入
- `internal/proxy/stream.go`：idle timeout 觸發只 log + 關閉 upstream，不再寫
  `: stream-idle-timeout` SSE comment。
- 測試：`TestStreamIdleTimeoutDoesNotInjectSSEBytes` — backend 主動 stall，斷言
  client 收到的 bytes 嚴格等於 backend 已輸出的 bytes。

### P1-11 Docker 建置與 compose 修正
- `docker/Dockerfile`：`golang:1.24-alpine` → `golang:1.25-alpine`，對齊
  `go.mod` 的 `go 1.25.0`。
- `cmd/gateway/main.go`：新增 `--healthcheck` 子指令，對自身 listener 打 `/healthz`
  並回傳 0/1（distroless 沒有 shell / curl）。
- `docker-compose.yml`：healthcheck 改為 `/app/llmgateway --healthcheck`，真實檢測
  HTTP `/healthz`。

### 連帶修正
- `config/gateway.yaml`：補上 `server.trusted_proxies`、`routing.allow_degraded_backends`、
  `api_keys[].allowed_client_ips/denied_client_ips` 範例與註解。

---

## 二、我認為「企業不需要 / 不合理 / 超出 LLM gateway 範圍」可以不修

只有這三條真的不需要修正。每條都附理由。

### P2-1 `/readyz` 把 unknown backend 視為 ready
- **判定：不需修正（合理 default）**
- 理由：unknown 是「啟動後 health probe 尚未跑完」的短暫狀態，K8s readiness
  probe 標準作法本來就是給 startup grace。若 unknown 不算 ready，第一次部署或
  rolling update 時 LB 會在 health probe 完成前就拒絕導流，反而造成假性 outage。
  review 提到「長時間 unknown 仍 ready 不好」是事實，但這是 backend 配置錯誤
  問題，不是 readyz 邏輯問題，readyz 自己加 grace 反而把問題藏起來。

### P2-2 multipart / audio 不是 byte-perfect passthrough
- **判定：超出 transparent passthrough 必要範圍**
- 理由：規格 transparent passthrough 的核心是「JSON 內 unknown vendor fields 必須
  完整轉發」，因為 JSON 是自由 schema、gateway 不知道哪些欄位有意義。multipart 是
  有名欄位的 form：每個 part 都有明確的 name + filename + content-type，gateway 解析
  後再 re-emit，每個 client 可觀察到的語意（field name、filename、body bytes、
  per-part content-type）都被保留。boundary 變動是 multipart 協議定義內可變的部分，
  client 不應依賴特定 boundary 值。所以 audio 走 re-emit 是 API gateway 標準作法，
  與 transparent passthrough 精神並不衝突。

### P2-3 config bool 預設值容易因 YAML 省略變成 false
- **判定：不合理改成隱式 default true**
- 理由：對 API key / backend / model 這類授權與路由控制資源，**「預設不啟用」才是
  安全 default**。如果一個 admin 建立新 API key 忘了寫 `enabled: true`，它應該
  是停用而不是上線。同理對 backend：忘了寫 enabled 就上線會把流量打到還沒準備好的
  後端。改成隱式 default true 對 UX 看起來好但對安全是退步。review 的提議在
  「使用者期望」上有道理，但企業環境寧可顯式宣告。

---

## 三、企業需要 / 在 LLM gateway 範圍內 / 但這次 PR 沒做

以下都是**真正需要做**的，我這次沒做完，誠實列在這裡，不再用「太大」當理由。

### P1-2 token rate limit / token quota 嚴格化
- 需要做：選一個策略（preflight estimate + reserve，或保留現有 postpaid soft-limit
  但明確記入文件 + 在 backend 無 usage 時做 fallback 估算）。
- 還未做。

### P1-6 health check schema 與行為
- 需要做：`enabled`、`type`（http / tcp / completion probe）、`method`、optional
  probe body、per-backend interval 真正生效。要做成 per-backend scheduler。
- 還未做。

### P1-7 Admin API 規格 endpoint 補齊
- 需要做：`GET /admin/models/{name}`、`PATCH /admin/models/{name}`、
  `PATCH /admin/model-aliases/{alias}`、`GET /admin/metrics`、
  `GET /admin/stats/{models,backends,api-keys}`。每支要 RBAC + audit。
- 還未做。

### P1-8 Dashboard 規格頁面
- 需要做：Overview QPS/p95/p99/TTFT、Models / Backends / API Keys 補欄位、
  patch/disable/rotate UI、Logs filter 完善、Analytics 趨勢圖、Settings 可寫。
  Dashboard auth 不能再用 sessionStorage 存密碼。
- 還未做。

### P1-9 Metrics 完整化
- 需要做：`routing_policy` label、`timeout_count` / `quota_exceeded_count` 獨立
  counter、`tokens_per_second`、Admin stats 暴露 p50/p95/p99/TTFT/queued。
- 還未做。

### P1-10 PostgreSQL / Redis 共享狀態
- 需要做：metadata + request_logs + audit_logs Postgres backed；rate limit +
  concurrency + queue + quota Redis backed；runtime admin mutation 持久化。
  至少 README + Helm values.yaml 需要清楚標註「沒有 Redis/Postgres 時 replicaCount
  必須為 1」。
- 還未做。

### P1-12 declared / strict capability mode
- 需要做：要嘛真實作（在 forwarding 前依 model.capabilities 檢查 vision / tool_call /
  thinking 欄位），要嘛把 declared / strict 從 admin UI / API 拿掉，避免使用者選了
  系統其實沒生效。
- 還未做。

### P1-13 Backend 異常 Mail 通知
- 需要做：notification config、SMTP client、status transition observer、
  cooldown / dedupe、non-blocking worker、admin 查看發送狀態、SMTP secret 不可明文。
- 還未做。

### P2-4 Admin auth / secrets 管理
- 需要做：Admin 改 JWT session 或 secure cookie，不要在 dashboard 存 password；
  sample config 不應放 plaintext admin password；create API key 沒給 `key` 時應
  自動產生並只回傳一次；backend API key 進 DB 必須 encrypted at rest。
- 還未做。

### P2-5 OpenTelemetry tracer 接入 request path
- 需要做：要嘛真的在 middleware / handler / proxy 內建 spans 並加上
  model/backend/status/latency/ttft/error attrs；要嘛把 README 內 tracing 章節改成
  「未實作」避免誤導驗收。
- 還未做。

### P2-6 README 與實作對齊
- 需要做：把 README 內各章節依「已實作 / 部分實作 / roadmap」分類，避免在
  Redis/Postgres、tracing、Dashboard 等項目給出比實際更完整的印象。
- 還未做。

---

## 四、必補測試清單對照

| 測試名稱 | 狀態 |
|---|---|
| TestDegradedBackendIsNotRoutable | 已加 |
| TestAliasCannotBypassDeniedInternalModel | 已加 |
| TestDisabledRegistryModelCannotBeForwarded | 已加 |
| TestAPIKeyAllowedClientIPs | 已加 |
| TestAPIKeyDeniedClientIPsTakePrecedence | 已加 |
| TestClientIPExtractorDoesNotTrustSpoofedXForwardedFor | 已加 |
| TestRequestLogsIncludeClientIP | 已加 |
| TestStatsAggregateByClientIP | 待補（admin stats by_client_ip aggregation 尚未實作） |
| TestAuthorizationRequiresBearerPrefix | 已加 |
| TestAPIKeyStatsIncrementWithoutUsage | 已加 |
| TestRawRequestLogKeepsClientOriginalBodyWhenAliasRewrites | 已加 |
| TestGatewayErrorsArePersistentlyLogged | 部分 — admit 階段拒絕已寫含 client_ip 的 persistent log；invalid_json / missing_model / model_not_found / no_healthy_backend / backend_at_capacity 路徑尚未寫 persistent log，需於 P1-4 補完整 |
| TestStreamIdleTimeoutDoesNotInjectSSEBytes | 已加 |
| TestAdminModelsGetPatchRoutes | 待補（endpoint 未實作，見 P1-7） |
| TestAdminAliasPatchRoute | 待補（endpoint 未實作，見 P1-7） |
| TestMetricsIncludeRoutingPolicyLabel | 待補（label 未加，見 P1-9） |
| TestBackendUnhealthySendsEmailNotification | 待補（見 P1-13） |
| TestBackendNotificationCooldown | 待補（見 P1-13） |
| TestBackendRecoverySendsEmailNotification | 待補（見 P1-13） |
| TestDockerBuildUsesCompatibleGoVersion | 待補（CI shell check） |

---

## 五、其他保持現狀的決定（不算修正）

1. **`/v1/models` 不依 backend health 過濾**：catalog 顯示「設定上可用」的 model，
   routing 才是 health gate。backend 暫時 unhealthy 時不從 catalog 移除 model，
   避免 client 誤判為 model 被下架。`/v1/models` 已會過濾 registry disabled
   （見 P0-3）與 API key 權限。
2. **client IP 不進 Prometheus label**：依 review 建議避免 high cardinality。
   持久層 request_logs 與 Admin API 全面記錄 client IP，已足以做 enterprise
   稽核與分組。未來如需 Prometheus label，加 `metrics.client_ip_label_enabled` 即可。

---

## 六、後續執行順序建議

依 review.md「Claude Code 修改順序建議」對齊，剩下的工作建議：

1. P1-7 Admin API + P1-9 metrics labels（同步補齊）
2. P1-8 Dashboard 接 1 + 上述新 admin endpoint
3. P1-13 Mail 通知 + P1-6 health check 擴充（同一個 observer pattern）
4. P1-2 strict token quota（與 P1-10 共用 Redis 後做）
5. P1-10 PostgreSQL + Redis（true HA）
6. P1-12 capability mode 落地或從 UI 移除
7. P2-4 admin auth、P2-5 OTEL、P2-6 README 清理
