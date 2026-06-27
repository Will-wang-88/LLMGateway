package compress

import (
	"bytes"
	"encoding/json"
	"strings"
)

// movable references a single tool-result text payload that is eligible for
// compression. For OpenAI-style messages (role=="tool") the whole message
// content is the payload and blockIdx is -1. For Anthropic-style messages the
// payload is one tool_result block inside the message's content array.
type movable struct {
	msgIdx   int
	blockIdx int // -1 => OpenAI whole-message string content
	content  string
}

// envelope is a partially-parsed request body. Unmodified messages are kept as
// raw bytes (json.RawMessage) and never round-tripped, so compression never
// perturbs the bytes of blocks it did not touch (preserving provider prefix
// cache and guaranteeing a true no-op when nothing is compressed).
type envelope struct {
	root     map[string]json.RawMessage
	keyOrder []string // original top-level key order, preserved on render
	msgsRaw  []json.RawMessage

	// Anthropic content arrays, parsed lazily per message that has movables.
	contentBlocks map[int][]json.RawMessage

	movables []movable

	// Pending replacements, keyed by message / block index.
	newOpenAI    map[int]string         // msgIdx -> new content (blockIdx == -1)
	newAnthropic map[int]map[int]string // msgIdx -> blockIdx -> new content
}

// parseEnvelope parses the request body and selects movable tool-result blocks
// (spec Stages 0–1). It returns (nil,false) on any structural failure, on
// which the caller must pass the original bytes through unchanged.
func parseEnvelope(reqBody []byte, cfg Config) (*envelope, bool) {
	dec := json.NewDecoder(bytes.NewReader(reqBody))
	dec.UseNumber()
	var root map[string]json.RawMessage
	if err := dec.Decode(&root); err != nil {
		return nil, false
	}
	msgsRaw, ok := root["messages"]
	if !ok {
		return nil, false
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(msgsRaw, &msgs); err != nil {
		return nil, false
	}

	env := &envelope{
		root:          root,
		keyOrder:      topLevelKeyOrder(reqBody, root),
		msgsRaw:       msgs,
		contentBlocks: make(map[int][]json.RawMessage),
		newOpenAI:     make(map[int]string),
		newAnthropic:  make(map[int]map[int]string),
	}

	boundary := protectedBoundary(msgs, cfg.ProtectRecentTurns)

	for i, mr := range msgs {
		if i >= boundary {
			continue // within the protected recent turns
		}
		var head struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(mr, &head); err != nil {
			continue
		}
		switch head.Role {
		case "tool":
			// OpenAI tool result: content is a JSON string.
			var s string
			if err := json.Unmarshal(head.Content, &s); err != nil {
				continue // non-string content => not movable
			}
			if !eligible(s, cfg) {
				continue
			}
			env.movables = append(env.movables, movable{msgIdx: i, blockIdx: -1, content: s})
		case "user":
			// Anthropic: tool_result blocks live in user messages as an array.
			var blocks []json.RawMessage
			if err := json.Unmarshal(head.Content, &blocks); err != nil {
				continue // string content (plain user prose) => never touch
			}
			env.contentBlocks[i] = blocks
			for bi, br := range blocks {
				var bh struct {
					Type    string          `json:"type"`
					Content json.RawMessage `json:"content"`
				}
				if err := json.Unmarshal(br, &bh); err != nil {
					continue
				}
				if bh.Type != "tool_result" {
					continue
				}
				var s string
				if err := json.Unmarshal(bh.Content, &s); err != nil {
					continue // tool_result with array content => skip
				}
				if !eligible(s, cfg) {
					continue
				}
				env.movables = append(env.movables, movable{msgIdx: i, blockIdx: bi, content: s})
			}
		}
	}
	return env, true
}

// eligible reports whether a tool-result string is worth compressing: large
// enough, and not already a prior retrieval result (which would otherwise be
// re-compressed into an unredeemable marker).
func eligible(s string, cfg Config) bool {
	if isCCRMarker(s) {
		return false
	}
	return estimateString(s) > cfg.MinTokensToCrush
}

// protectedBoundary returns the message index at and after which messages are
// protected (the most recent N turns). A turn boundary is a "user" message.
func protectedBoundary(msgs []json.RawMessage, protectRecentTurns int) int {
	if protectRecentTurns <= 0 {
		return len(msgs)
	}
	var userIdx []int
	for i, mr := range msgs {
		var head struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(mr, &head); err != nil {
			continue
		}
		if head.Role == "user" {
			userIdx = append(userIdx, i)
		}
	}
	if len(userIdx) <= protectRecentTurns {
		return 0 // fewer turns than we protect => protect everything
	}
	return userIdx[len(userIdx)-protectRecentTurns]
}

// queryText returns the text of the most recent user message, used as the
// relevance query for the lossy stage. For Anthropic-style array content it
// concatenates the text blocks.
func (e *envelope) queryText() string {
	last := ""
	for _, mr := range e.msgsRaw {
		var head struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(mr, &head); err != nil {
			continue
		}
		if head.Role != "user" {
			continue
		}
		var s string
		if err := json.Unmarshal(head.Content, &s); err == nil {
			if strings.TrimSpace(s) != "" {
				last = s
			}
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(head.Content, &blocks); err == nil {
			var sb strings.Builder
			for _, b := range blocks {
				if b.Type == "text" && b.Text != "" {
					if sb.Len() > 0 {
						sb.WriteByte(' ')
					}
					sb.WriteString(b.Text)
				}
			}
			if strings.TrimSpace(sb.String()) != "" {
				last = sb.String()
			}
		}
	}
	return last
}

// setContent records a replacement payload for a movable block.
func (e *envelope) setContent(m movable, newContent string) {
	if m.blockIdx < 0 {
		e.newOpenAI[m.msgIdx] = newContent
		return
	}
	if e.newAnthropic[m.msgIdx] == nil {
		e.newAnthropic[m.msgIdx] = make(map[int]string)
	}
	e.newAnthropic[m.msgIdx][m.blockIdx] = newContent
}

// changed reports whether any block was replaced.
func (e *envelope) changed() bool {
	return len(e.newOpenAI) > 0 || len(e.newAnthropic) > 0
}

// render rebuilds the request body, re-marshaling only messages that changed.
// Untouched messages keep their exact original bytes.
func (e *envelope) render() ([]byte, error) {
	newMsgs := make([]json.RawMessage, len(e.msgsRaw))
	for i, mr := range e.msgsRaw {
		s, openAIChanged := e.newOpenAI[i]
		blockChanges, anthChanged := e.newAnthropic[i]
		if !openAIChanged && !anthChanged {
			newMsgs[i] = mr
			continue
		}
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(mr, &msg); err != nil {
			newMsgs[i] = mr // give up on this message; keep original
			continue
		}
		if openAIChanged {
			enc, err := json.Marshal(s)
			if err != nil {
				newMsgs[i] = mr
				continue
			}
			msg["content"] = enc
		}
		if anthChanged {
			blocks := append([]json.RawMessage(nil), e.contentBlocks[i]...)
			for bi, nc := range blockChanges {
				if bi < 0 || bi >= len(blocks) {
					continue
				}
				var blk map[string]json.RawMessage
				if err := json.Unmarshal(blocks[bi], &blk); err != nil {
					continue
				}
				enc, err := json.Marshal(nc)
				if err != nil {
					continue
				}
				blk["content"] = enc
				nb, err := json.Marshal(blk)
				if err != nil {
					continue
				}
				blocks[bi] = nb
			}
			cb, err := json.Marshal(blocks)
			if err != nil {
				newMsgs[i] = mr
				continue
			}
			msg["content"] = cb
		}
		nm, err := json.Marshal(msg)
		if err != nil {
			newMsgs[i] = mr
			continue
		}
		newMsgs[i] = nm
	}
	encMsgs, err := json.Marshal(newMsgs)
	if err != nil {
		return nil, err
	}
	e.root["messages"] = encMsgs
	return orderedMarshal(e.root, e.keyOrder)
}

// topLevelKeyOrder returns the keys of the top-level object in their original
// source order so render can preserve byte layout ahead of the messages array.
// Any key present in root but not seen during scanning is appended sorted.
func topLevelKeyOrder(b []byte, root map[string]json.RawMessage) []string {
	dec := json.NewDecoder(bytes.NewReader(b))
	order := make([]string, 0, len(root))
	seen := make(map[string]bool, len(root))
	if _, err := dec.Token(); err != nil { // consume '{'
		return sortedKeys(root)
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			break
		}
		key, ok := kt.(string)
		if !ok {
			break
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil { // consume value
			break
		}
		if !seen[key] {
			order = append(order, key)
			seen[key] = true
		}
	}
	for _, k := range sortedKeys(root) {
		if !seen[k] {
			order = append(order, k)
		}
	}
	return order
}

// orderedMarshal renders a JSON object with keys in the given order.
func orderedMarshal(m map[string]json.RawMessage, order []string) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	write := func(k string, v json.RawMessage) error {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		kb, err := json.Marshal(k)
		if err != nil {
			return err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(v)
		return nil
	}
	emitted := make(map[string]bool, len(m))
	for _, k := range order {
		v, ok := m[k]
		if !ok {
			continue
		}
		if err := write(k, v); err != nil {
			return nil, err
		}
		emitted[k] = true
	}
	for _, k := range sortedKeys(m) {
		if emitted[k] {
			continue
		}
		if err := write(k, m[k]); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// trimmedEmpty is a small helper to detect blank payloads.
func trimmedEmpty(s string) bool { return strings.TrimSpace(s) == "" }
