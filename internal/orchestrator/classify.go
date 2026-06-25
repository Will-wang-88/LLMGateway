package orchestrator

import (
	"strings"
	"unicode"
)

// Task labels produced by the Tier-A classifier. These are matched against
// each worker's configured `tasks` list to choose a preferred worker.
const (
	TaskCode      = "code"
	TaskReasoning = "reasoning"
	TaskZh        = "zh"      // Traditional-Chinese-heavy input
	TaskGeneral   = "general" // default: chat / summarization / multilingual
)

// classification is the output of the rule-based Tier-A router. Confidence
// is the dominant task's share of total signal; a low value means the
// request is ambiguous and should be escalated.
type classification struct {
	Task       string
	Confidence float64
	Scores     map[string]float64
	ZhRatio    float64
}

// signal keyword sets. Matching is case-insensitive substring matching over
// the request text (English) plus literal CJK substrings (Chinese).
var (
	codeSignals = []string{
		"```", "def ", "func ", "class ", "import ", "function", "return ",
		"const ", "let ", "var ", "#include", "public static", "println",
		"console.log", "stack trace", "traceback", "exception", "segfault",
		"compile", "compiler", "debug", "refactor", "unit test", "npm ",
		"pip install", "go build", "cargo ", "kubectl", "dockerfile",
		"bug", "code", "python", "javascript", "typescript", "golang",
		"rust", "regex", "sql", "syntax", "api endpoint",
		"程式", "程式碼", "代碼", "除錯", "函式", "函數", "編譯", "報錯", "重構", "單元測試",
	}
	reasoningSignals = []string{
		"prove", "proof", "theorem", "solve for", "calculate", "compute the",
		"step by step", "step-by-step", "derive", "derivative", "integral",
		"equation", "probability", "how many", "optimal", "algorithm complexity",
		"reason through", "logic puzzle", "chain of thought",
		"證明", "計算", "推理", "求解", "方程", "演算法", "邏輯", "逐步",
	}
)

// classify runs the rule-based Tier-A classifier over the latest user text.
func classify(text string) classification {
	lower := strings.ToLower(text)
	scores := map[string]float64{
		TaskCode:      countSignals(lower, codeSignals),
		TaskReasoning: countSignals(lower, reasoningSignals),
		TaskGeneral:   1.0, // base mass so a signal-free prompt is confidently general
	}

	zhRatio := traditionalChineseRatio(text)
	if zhRatio > 0.2 {
		// Strong Han presence promotes the zh task; the more Han, the
		// stronger. Capped so it can't single-handedly dominate a clear
		// code/reasoning prompt.
		scores[TaskZh] = 1.0 + zhRatio*3.0
	}

	// Dominant task = argmax. Confidence = its share of total mass.
	var total, top float64
	task := TaskGeneral
	// Deterministic iteration order for stable tie-breaking.
	for _, k := range []string{TaskCode, TaskReasoning, TaskZh, TaskGeneral} {
		v := scores[k]
		total += v
		if v > top {
			top = v
			task = k
		}
	}
	conf := 0.0
	if total > 0 {
		conf = top / total
	}
	return classification{Task: task, Confidence: conf, Scores: scores, ZhRatio: zhRatio}
}

func countSignals(lowerText string, signals []string) float64 {
	var n float64
	for _, s := range signals {
		if strings.Contains(lowerText, s) {
			n++
		}
	}
	return n
}

// traditionalChineseRatio returns the fraction of Han (CJK) runes among all
// non-space runes. It does not distinguish Simplified from Traditional (the
// pool routing prior handles 繁中 via the worker's tasks list), but a high
// Han ratio is a reliable signal that a Chinese-strong worker is preferred.
func traditionalChineseRatio(text string) float64 {
	var han, total float64
	for _, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		if unicode.Is(unicode.Han, r) {
			han++
		}
	}
	if total == 0 {
		return 0
	}
	return han / total
}
