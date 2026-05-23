// Package capability implements declared / strict request validation
// against a model's capability descriptor.
//
// Modes:
//   - passthrough (default): no validation; transparent forward.
//   - declared: reject requests that obviously violate a capability the
//     model explicitly declared as false (e.g. image_url content when
//     capabilities.vision=false, tools when capabilities.tool_call=false,
//     reasoning fields when capabilities.thinking=false). Unknown
//     capabilities are not enforced.
//   - strict: like declared, but unknown capabilities are also rejected
//     (i.e. anything not explicitly allowed is denied). Requires the
//     model's Capabilities map to be populated.
package capability

import (
	"bytes"
	"encoding/json"
)

// Check inspects the raw JSON request body and reports the first
// violation found, or empty string if the request is admissible under
// the given mode + capability map. mode is normalized to lower-case;
// unknown modes are treated as passthrough.
func Check(mode string, caps map[string]bool, body []byte) string {
	switch mode {
	case "", "passthrough":
		return ""
	case "declared", "strict":
		return checkDeclared(mode == "strict", caps, body)
	}
	return ""
}

func checkDeclared(strict bool, caps map[string]bool, body []byte) string {
	if !strict && len(caps) == 0 {
		// Declared with no capability map = effectively passthrough.
		return ""
	}
	usesTools := hasTopLevel(body, "tools") || hasTopLevel(body, "tool_choice")
	usesVision := referencesImage(body)
	usesThinking := hasTopLevel(body, "reasoning_effort") || hasTopLevel(body, "thinking_budget") || hasTopLevel(body, "enable_thinking")

	if usesTools && !cap_(caps, "tool_call", !strict) {
		return "tool_call_not_supported"
	}
	if usesVision && !cap_(caps, "vision", !strict) {
		return "vision_not_supported"
	}
	if usesThinking && !cap_(caps, "thinking", !strict) {
		return "thinking_not_supported"
	}
	return ""
}

// cap_ returns whether the capability is allowed. unsetMeansAllow controls
// the semantics when the capability key is absent from the map:
//   - declared: unset means "not declared either way" -> allow.
//   - strict: unset means "must be explicitly allowed" -> deny.
func cap_(caps map[string]bool, key string, unsetMeansAllow bool) bool {
	if v, ok := caps[key]; ok {
		return v
	}
	return unsetMeansAllow
}

// hasTopLevel reports whether the JSON object has the given top-level
// key with a non-null value.
func hasTopLevel(body []byte, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	v, ok := m[key]
	if !ok {
		return false
	}
	trimmed := bytes.TrimSpace(v)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// referencesImage looks for `"type":"image_url"` parts inside
// messages[].content. We use a string scan (not full JSON unmarshal)
// because content is a heterogeneous list of objects; the scan is
// scoped to messages[] to avoid false positives elsewhere.
func referencesImage(body []byte) bool {
	var env struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return false
	}
	needle := []byte(`"image_url"`)
	for _, m := range env.Messages {
		if bytes.Contains(m, needle) {
			return true
		}
	}
	return false
}
