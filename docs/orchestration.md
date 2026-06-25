# Fugu-style Model Orchestration — Design & Roadmap

This document describes the behaviour-level model orchestration layer built
into the gateway, and the training roadmap for its learned variants. It is
the engineering counterpart to the v0.1 specification.

## 0. Why orchestration, not weight merging

The target pool is architecturally heterogeneous and **cannot** be
weight-merged:

| Model        | Architecture   | Approx. scale            | Weight merge |
|--------------|----------------|--------------------------|--------------|
| gpt-oss-120b | MoE (sparse)   | 120B total / ~5B active  | ❌ not dense-compatible |
| Gemma 4 26B  | Dense          | 26B                      | ❌ different vocab/depth |
| Qwen3.6 27B  | Dense          | 27B                      | ❌ different vocab/depth |

Even the tokenizers differ, so SLERP / TIES / DARE are out. The only viable
route is **behaviour-level fusion**: a small orchestrator decides, at
inference time, which black-box model(s) answer each request.

## 1. Goals

1. Expose a **single OpenAI-compatible endpoint**.
2. Internally route to one worker, or decompose and fan out to several then
   synthesize.
3. All workers run **on-prem / self-hosted** (vLLM / SGLang) — no data
   egress.
4. Two operating modes: low-latency single-pick vs. high-quality multi-step.

## 2. Two-tier architecture

### Tier-A — Router (low latency)

- **MVP (shipped):** a rule-based classifier (`internal/orchestrator/classify.go`)
  scores the request across task classes (`code`, `reasoning`, `zh`,
  `general`) from keyword/heuristic signals, plus a Traditional-Chinese Han
  ratio. The dominant class and its confidence (its share of total signal
  mass) drive a single-worker pick.
- **Learned (roadmap):** a bias-free linear *selection head* on a small
  backbone (e.g. Qwen3 0.6B). It reads an early-token hidden state, scores
  each worker, and dispatches — no full generation, so latency ≈ one model
  call. Trainable params are tiny (selection head + a few SVF offsets).
  Training: large-scale SFT warm-up → **sep-CMA-ES** (gradient-free)
  against end-to-end terminal reward.

### Tier-B — Conductor (high quality)

- **MVP (shipped):** a deterministic TRINITY DAG
  (`internal/orchestrator/conductor.go`): **Thinker → Worker → Verifier →
  Synthesizer**, trimmed to `max_steps` (≤5). The verifier is forced to a
  different model than the producer; access lists gate cross-step visibility.
- **Learned (roadmap):** a ~7B LM trained with RL (PPO/GRPO) to *author* the
  ≤5-step workflow in natural language and assign subtasks, reward = final
  task quality − (optional) cost penalty.

## 3. Routing priors (pool capability map)

Seeded by hand, then corrected by the orchestrator over time:

| Task type                    | Preferred worker            | Rationale |
|------------------------------|-----------------------------|-----------|
| Code gen / debug             | Qwen3.6 27B                 | strong coding, stable tool use |
| Long reasoning / math        | gpt-oss-120b                | MoE capacity, deep reasoning |
| Multilingual / chat / summary| Gemma 4 26B                 | general, low latency |
| Traditional Chinese (繁中)   | Qwen3.6 27B → Gemma cross   | better zh-TW corpus |
| Critical verification step   | a **different** model        | forced heterogeneous check |

These map onto each worker's `tasks`, `strength`, and `cost` in config.

## 4. Inference topology

```
Client ──> [Gateway / OpenAI-compatible API]
                 │
                 ▼
          [Orchestrator]
          ├ Tier-A Router (rule head)        ← default, low-latency path
          └ Tier-B Conductor (DAG, ≤5 step)  ← escalation / quality path
                 │  (HTTP, access-list controls visibility)
     ┌───────────┼───────────┐
     ▼           ▼           ▼
 gpt-oss-120b  Gemma 4 26B  Qwen3.6 27B
   (vLLM)       (SGLang)     (vLLM)
```

Suggested worker serving:

```
gpt-oss-120b → vLLM, TP=4 (H200), MoE expert-parallel tuning
Gemma 4 26B  → SGLang/vLLM, TP=2, latency-first
Qwen3.6 27B  → vLLM, TP=2, prefix caching on (repeated code prefixes)
```

## 5. Training pipeline (roadmap — outside this Go service)

1. **Data prep:** collect coding / reasoning / general prompts (incl. 繁中).
   Run all three workers per prompt, record outputs + scores → per-class
   "which model wins" ground truth.
2. **Tier-A:** SFT the selection head on the best-model labels →
   sep-CMA-ES against terminal reward (learns the latency/quality trade-off).
3. **Tier-B:** RL-train the conductor to emit ≤5-step workflows;
   reward = answer quality − cost penalty.

**Eval targets:** the orchestrator should beat any single pool member;
track task success rate, mean latency, mean token cost, and Tier-B
escalation ratio.

## 6. Access list & data governance

Each request carries a sensitivity level (`X-Secret-Level` header or
`secret_level` body field). A worker processes it only if its
`secret_max_level` admits the level (`0` = unlimited / on-prem). Because the
reference pool is fully self-hosted this is a no-op today; it becomes the
enforcement point if a cloud worker is ever added (give it a low
`secret_max_level` so high-sensitivity data never routes off-prem). Inside
Tier-B, each step additionally declares which prior outputs are visible
(e.g. the verifier sees the draft but not the planner's notes, to keep the
check independent).

## 7. Landing path

1. **MVP (done):** rule router + conductor over three vLLM endpoints behind
   one API. ✅
2. **Learned router:** collect data, train the Tier-A selection head, swap it
   in behind `router_model`.
3. **Learned conductor:** RL-trained Tier-B planner behind `conductor_model`.
4. **Cost optimisation:** tune `confidence_threshold` / `cost_penalty`,
   measure ROI.

## 8. Risks & trade-offs

- **Mis-routing cost:** a wrong single pick can be worse than always using
  the strongest model — hence the confidence threshold and Tier-B/strongest
  fallback.
- **Latency stacking:** multi-step workflows accumulate latency; ≤5 steps is
  a hard cap (enforced in config validation).
- **Eval bias:** a worker grading itself is untrustworthy — the verifier is
  always a heterogeneous model.
- **MoE tuning:** gpt-oss-120b expert routing is throughput-sensitive on
  multi-GPU; benchmark it separately.

## References

- Sakana AI Fugu (official): `github.com/SakanaAI/fugu`
- Open reimplementation: `github.com/trotsky1997/OpenFugu`
