package compress

// EstimateTokens is a cheap, deterministic, monotone token estimate used both
// for the per-model threshold gate and for the lossy budget. It deliberately
// avoids a real tokenizer (which would be slow and provider-specific): the
// classic bytes/4 heuristic is enough to decide whether compression is worth
// running and to size the row-drop budget. Same bytes in => same number out.
func EstimateTokens(b []byte) int {
	return len(b) / 4
}

// estimateString is the string convenience form of EstimateTokens.
func estimateString(s string) int {
	return len(s) / 4
}
