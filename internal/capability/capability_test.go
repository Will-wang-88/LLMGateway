package capability

import "testing"

func TestPassthroughIgnoresEverything(t *testing.T) {
	body := []byte(`{"messages":[{"content":[{"type":"image_url","image_url":{"url":"x"}}]}],"tools":[],"reasoning_effort":"high"}`)
	if v := Check("passthrough", map[string]bool{"vision": false}, body); v != "" {
		t.Errorf("passthrough should not reject, got %q", v)
	}
}

func TestDeclaredRejectsImageWhenVisionFalse(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`)
	v := Check("declared", map[string]bool{"vision": false}, body)
	if v != "vision_not_supported" {
		t.Errorf("expected vision_not_supported, got %q", v)
	}
}

func TestDeclaredAllowsImageWhenVisionTrue(t *testing.T) {
	body := []byte(`{"messages":[{"content":[{"type":"image_url"}]}]}`)
	if v := Check("declared", map[string]bool{"vision": true}, body); v != "" {
		t.Errorf("expected pass, got %q", v)
	}
}

func TestDeclaredAllowsWhenCapabilityNotDeclared(t *testing.T) {
	body := []byte(`{"messages":[{"content":[{"type":"image_url"}]}]}`)
	if v := Check("declared", map[string]bool{}, body); v != "" {
		t.Errorf("declared with empty caps should pass, got %q", v)
	}
}

func TestStrictRejectsWhenCapabilityMissing(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function"}],"messages":[]}`)
	if v := Check("strict", map[string]bool{}, body); v != "tool_call_not_supported" {
		t.Errorf("strict with missing tool_call should reject, got %q", v)
	}
}

func TestDeclaredRejectsThinkingFields(t *testing.T) {
	body := []byte(`{"messages":[],"reasoning_effort":"high"}`)
	if v := Check("declared", map[string]bool{"thinking": false}, body); v != "thinking_not_supported" {
		t.Errorf("expected thinking_not_supported, got %q", v)
	}
}
