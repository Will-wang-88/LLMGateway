package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/will-wang-88/llmgateway/internal/config"
)

// Conductor role names (also used as metric labels and in the route trace).
const (
	roleThinker     = "thinker"
	roleWorker      = "worker"
	roleVerifier    = "verifier"
	roleSynthesizer = "synthesizer"
)

// runConductor executes the Tier-B bounded DAG. It implements a TRINITY-style
// Thinker → Worker → Verifier → Synthesizer pipeline, trimmed to fit the
// configured step budget, with the verifier forced to a different model than
// the producer (heterogeneous cross-check) and per-step access lists gating
// which prior outputs each worker can see.
func (o *Orchestrator) runConductor(ctx context.Context, base []chatMessage, params genParams, eligible []*config.OrchestrationWorker, cls classification, escalated bool) (*Result, error) {
	budget := o.cfg.MaxSteps
	if budget <= 0 {
		budget = 5
	}

	// Decide which optional stages fit the budget. The solver (draft) is
	// mandatory; verifier and synthesizer are added as budget allows, and
	// the thinker is added last because it is the most optional.
	doVerifier := budget >= 2
	doSynth := budget >= 3
	doThinker := budget >= 4

	res := &Result{Tier: "conductor", Task: cls.Task, Confidence: cls.Confidence, Escalated: escalated}
	stepNo := 0
	addStep := func(role string, c *completion) {
		stepNo++
		o.incStep(role, c.WorkerID)
		res.Steps = append(res.Steps, StepInfo{
			Step: stepNo, Role: role, WorkerID: c.WorkerID, Model: c.Model,
			BackendID: c.BackendID, LatencyMS: c.LatencyMS,
		})
		res.Usage.PromptTokens += c.Usage.PromptTokens
		res.Usage.CompletionTokens += c.Usage.CompletionTokens
		res.Usage.TotalTokens += c.Usage.TotalTokens
		res.Usage.ReasoningTokens += c.Usage.ReasoningTokens
	}

	solver := o.selectWorker(cls.Task, eligible)
	if solver == nil {
		solver = strongestWorker(eligible)
	}

	// 1. Thinker: decompose the task. Access list: user only.
	var plan string
	if doThinker {
		thinker := o.selectWorker(TaskReasoning, eligible)
		if thinker == nil {
			thinker = solver
		}
		msgs := stepMessages(thinkerPrompt, base, nil)
		c, err := o.callWorker(ctx, thinker, msgs, plannerParams(params))
		if err != nil {
			o.incRoute("conductor", thinker.ID, cls.Task, "error")
			return nil, fmt.Errorf("conductor thinker step: %w", err)
		}
		plan = c.Text
		addStep(roleThinker, c)
	}

	// 2. Worker: produce a draft answer. Access list: user + thinker plan.
	var solverCtx []ctxBlock
	if plan != "" {
		solverCtx = append(solverCtx, ctxBlock{"DECOMPOSITION / PLAN", plan})
	}
	draftMsgs := stepMessages(workerPrompt, base, solverCtx)
	draftC, err := o.callWorker(ctx, solver, draftMsgs, params)
	if err != nil {
		o.incRoute("conductor", solver.ID, cls.Task, "error")
		return nil, fmt.Errorf("conductor worker step: %w", err)
	}
	draft := draftC.Text
	addStep(roleWorker, draftC)

	// 3. Verifier: heterogeneous cross-check. Access list: user + draft
	//    (deliberately NOT the plan, to keep the check independent).
	var critique string
	if doVerifier {
		verifier := o.selectVerifier(solver, eligible)
		if verifier.ID == solver.ID {
			// Only one eligible model — a same-model check is weaker but
			// still useful; note it in the trace via the role label.
			o.logWarn("conductor: verifier falls back to same model as worker (only one eligible worker)")
		}
		vMsgs := stepMessages(verifierPrompt, base, []ctxBlock{{"CANDIDATE ANSWER TO REVIEW", draft}})
		vC, err := o.callWorker(ctx, verifier, vMsgs, plannerParams(params))
		if err != nil {
			// A failed verification should not sink the whole request; we
			// degrade to the draft answer rather than erroring out.
			o.logWarn("conductor: verifier step failed, returning unverified draft: " + err.Error())
		} else {
			critique = vC.Text
			addStep(roleVerifier, vC)
		}
	}

	// 4. Synthesizer: fold the critique into a final answer. Access list:
	//    user + draft + critique.
	if doSynth && critique != "" {
		synthCtx := []ctxBlock{
			{"DRAFT ANSWER", draft},
			{"REVIEWER FEEDBACK", critique},
		}
		sMsgs := stepMessages(synthesizerPrompt, base, synthCtx)
		sC, err := o.callWorker(ctx, solver, sMsgs, params)
		if err != nil {
			o.logWarn("conductor: synthesizer step failed, returning draft: " + err.Error())
			res.Content = draft
		} else {
			res.Content = sC.Text
			addStep(roleSynthesizer, sC)
		}
	} else {
		res.Content = draft
	}

	outcome := "ok"
	if res.Content == "" {
		outcome = "empty"
	}
	o.incRoute("conductor", solver.ID, cls.Task, outcome)
	return res, nil
}

// selectVerifier picks a worker for the verification step, strongly
// preferring a model that is DIFFERENT from the producer (the spec requires
// a heterogeneous cross-check). Workers tagged "verify" are preferred among
// the alternatives.
func (o *Orchestrator) selectVerifier(producer *config.OrchestrationWorker, eligible []*config.OrchestrationWorker) *config.OrchestrationWorker {
	var bestDiff *config.OrchestrationWorker
	bestScore := -1e18
	for _, w := range eligible {
		if w.ID == producer.ID {
			continue
		}
		score := workerStrength(w)
		if containsFold(w.Tasks, "verify") {
			score += 1.0
		}
		if score > bestScore {
			bestScore = score
			bestDiff = w
		}
	}
	if bestDiff != nil {
		return bestDiff
	}
	return producer
}

func (o *Orchestrator) logWarn(msg string) {
	if o.logger != nil {
		o.logger.Warn(msg, nil)
	}
}

// ctxBlock is a labeled piece of context injected into a step's system
// prompt under the access-list rules.
type ctxBlock struct {
	Label string
	Text  string
}

// stepMessages builds the message list for one worker call: a system message
// carrying the role instructions plus any access-listed context blocks,
// followed by the original conversation.
func stepMessages(rolePrompt string, base []chatMessage, ctx []ctxBlock) []chatMessage {
	var sys strings.Builder
	sys.WriteString(rolePrompt)
	for _, b := range ctx {
		sys.WriteString("\n\n--- ")
		sys.WriteString(b.Label)
		sys.WriteString(" ---\n")
		sys.WriteString(b.Text)
	}
	out := make([]chatMessage, 0, len(base)+1)
	out = append(out, chatMessage{Role: "system", Content: sys.String()})
	out = append(out, base...)
	return out
}

// plannerParams returns params suited to planning/critique steps: it leaves
// the caller's settings intact but drops any client max_tokens cap that is
// too small to express a useful intermediate artifact.
func plannerParams(p genParams) genParams {
	out := p
	if out.MaxTokens != nil && *out.MaxTokens < 512 {
		out.MaxTokens = nil
	}
	return out
}

const (
	thinkerPrompt = "You are the THINKER in a multi-model workflow. Do NOT answer the user's request directly. " +
		"Instead, decompose it into a short, concrete plan (at most five steps) that another model will execute. " +
		"Identify the key sub-problems, edge cases, and what a correct answer must contain. Be concise."

	workerPrompt = "You are the WORKER in a multi-model workflow. Produce the best, complete answer to the user's request. " +
		"If a plan is provided, follow it. Be correct and concrete."

	verifierPrompt = "You are the VERIFIER in a multi-model workflow, and you are a DIFFERENT model from the one that wrote the candidate answer. " +
		"Critically review the candidate answer for correctness, missing cases, and mistakes. " +
		"List concrete problems and corrections. If the answer is fully correct, say so explicitly. Do NOT rewrite the whole answer."

	synthesizerPrompt = "You are the SYNTHESIZER in a multi-model workflow. Using the draft answer and the reviewer feedback, " +
		"produce the final, corrected answer for the user. Incorporate valid corrections and ignore incorrect feedback. " +
		"Output only the final answer, with no meta-commentary about the process."
)
