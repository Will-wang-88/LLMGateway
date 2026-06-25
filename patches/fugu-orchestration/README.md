# Fugu 編排層 Patch — 套用說明

本 patch 對應 PR #5 的兩個 commit，基底是 `a0ddc5a`(PR #4 合併後的狀態):

1. `439db2b` Add Fugu-style model orchestration layer
2. `930fc08` Address review findings in orchestration layer

## 檔案

| 檔案 | 用途 | 套用工具 |
|------|------|----------|
| `fugu-orchestration.patch` | 單一檔含兩個 commit(保留訊息/作者) | `git am` |
| `0001-*.patch` / `0002-*.patch` | 分段版,每個 commit 一檔 | `git am` |
| `fugu-orchestration.diff` | 純 diff(無 commit 訊息) | `git apply` 或 `patch -p1` |

擇一即可。建議用 `fugu-orchestration.patch`,可完整保留兩個 commit 歷史。

---

## 方法 A:`git am`(建議,保留 commit 歷史)

在內網 repo 中,先切到要併入的分支(基底需為 `a0ddc5a`,或內容等同的 main):

```bash
cd /path/to/internal/llmgateway
git checkout main            # 或你要併入的分支
git checkout -b fugu-orchestration   # 建議開新分支,驗證後再併回

git am /path/to/fugu-orchestration.patch
```

成功後會多出兩個 commit。確認:

```bash
git log --oneline -3
go build ./... && go test ./...
```

驗證無誤後併回主線:

```bash
git checkout main
git merge --no-ff fugu-orchestration
git push
```

### 若 `git am` 中途失敗(基底不完全一致時)
```bash
git am --show-current-patch=diff   # 看衝突內容
# 手動編輯衝突檔後:
git add -A
git am --continue
# 或放棄:
git am --abort
```
若衝突較多,改用方法 B 的三方合併。

---

## 方法 B:`git apply`(只要變更內容,不保留 commit 訊息)

```bash
cd /path/to/internal/llmgateway

# 先試套(不實際改檔),確認可乾淨套用:
git apply --check fugu-orchestration.diff

# 正式套用:
git apply fugu-orchestration.diff

# 衝突時改用三方合併(較能自動消解):
git apply --3way fugu-orchestration.diff
```

之後自行 commit:

```bash
git add -A
git commit -m "Add Fugu-style model orchestration layer (from PR #5)"
```

不使用 git 時,也可用標準 patch 工具:

```bash
patch -p1 < fugu-orchestration.diff
```

---

## 套用後檢查清單

```bash
go build ./...
go vet ./...
go test ./...
```

- 預設 `orchestration.enabled: false`,套用後行為不變;需在 `config/gateway.yaml`
  的 `orchestration:` 區塊開啟並設定 worker pool 才會啟用 `fugu-auto` / `fugu-ultra`
  兩個虛擬模型。
- 詳細設計與設定見 repo 內 `docs/orchestration.md` 與 README「Fugu-style model
  orchestration」章節(本 patch 一併帶入)。

## 備註
- commit 訊息含 `Co-Authored-By` / `Claude-Session` trailer。若內網不需要,
  套用後可用 `git rebase -i` 或 `git commit --amend` 移除,或改用方法 B 自行撰寫
  commit 訊息。
- 基底 commit:`a0ddc5a`。若內網 main 與此基底有差異,優先使用 `git am --3way`
  或 `git apply --3way` 以三方合併降低衝突。
