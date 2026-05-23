package proxy

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestReadLineHandlesLineLargerThanBuffer(t *testing.T) {
	// Build a single "data: ...\n" line that is bigger than the default
	// bufio buffer (256 KiB) to verify the reassembly path.
	huge := strings.Repeat("x", 600*1024)
	input := "data: " + huge + "\n" + "data: small\n"

	br := bufio.NewReaderSize(strings.NewReader(input), 4096)

	first, err := readLine(br)
	if err != nil {
		t.Fatalf("first read err=%v", err)
	}
	if !bytes.HasSuffix(first, []byte{'\n'}) {
		t.Error("first line should end in newline")
	}
	if len(first) != len("data: ")+len(huge)+1 {
		t.Errorf("first line length %d != expected %d", len(first), len("data: ")+len(huge)+1)
	}
	if !bytes.HasPrefix(first, []byte("data: ")) {
		t.Error("first line should start with 'data: '")
	}

	second, err := readLine(br)
	if err != nil {
		t.Fatalf("second read err=%v", err)
	}
	if string(second) != "data: small\n" {
		t.Errorf("second line=%q", second)
	}

	_, err = readLine(br)
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
}

func TestReadLineHandlesTrailingNonNewline(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader("data: incomplete"), 4096)
	line, err := readLine(br)
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
	if string(line) != "data: incomplete" {
		t.Errorf("line=%q", line)
	}
}

func TestParseChunkUsage(t *testing.T) {
	u := parseChunkUsage([]byte(`data: {"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12,"reasoning_tokens":3}}` + "\n"))
	if u == nil {
		t.Fatal("expected non-nil usage")
	}
	if u.PromptTokens != 5 || u.CompletionTokens != 7 || u.TotalTokens != 12 || u.ReasoningTokens != 3 {
		t.Errorf("unexpected usage: %+v", u)
	}

	if u := parseChunkUsage([]byte("data: [DONE]\n")); u != nil {
		t.Errorf("[DONE] should not yield usage")
	}
	if u := parseChunkUsage([]byte(": comment\n")); u != nil {
		t.Errorf("comment should not yield usage")
	}
}
