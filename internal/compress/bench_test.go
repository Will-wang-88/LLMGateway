package compress

import (
	"encoding/json"
	"testing"
)

func BenchmarkCompressUniformArray(b *testing.B) {
	rows := make([]any, 200)
	for i := 0; i < 200; i++ {
		rows[i] = map[string]any{"ts": "2024-01-01T00:00:00Z", "host": "web-1", "cpu": i, "ok": true, "region": "us-east"}
	}
	arr, _ := json.Marshal(rows)
	body, _ := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "tool", "content": string(arr)},
		},
	})
	cfg := DefaultConfig()
	cfg.ProtectRecentTurns = 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = Compress(body, cfg)
	}
}
