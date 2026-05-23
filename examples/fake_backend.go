//go:build ignore
// +build ignore

// fake_backend is a small OpenAI-compatible test backend.
// Run alongside the gateway for manual smoke testing:
//
//	go run examples/fake_backend.go --port 9001 --model llama-3.1-70b
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	port := flag.Int("port", 9001, "Listen port")
	model := flag.String("model", "fake-model", "Model name to advertise")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model","owned_by":"fake","created":1700000000}]}`, *model)
	})
	mux.HandleFunc("/v1/audio/transcriptions", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "parse multipart: "+err.Error(), http.StatusBadRequest)
			return
		}
		model := r.FormValue("model")
		log.Printf("audio transcription requested model=%s", model)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"text":"Hello from fake transcribed audio (model=%s)"}`, model)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		log.Printf("inbound body: %s", body)
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Stream {
			streamResponse(w, req.Model)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
			"id":"chatcmpl-fake",
			"object":"chat.completion",
			"model":%q,
			"choices":[{
				"index":0,
				"message":{
					"role":"assistant",
					"content":"Hello from fake backend!",
					"reasoning_content":"Let me think... the answer is hello."
				},
				"finish_reason":"stop"
			}],
			"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12,"reasoning_tokens":4},
			"vendor_extension":{"echo":true}
		}`, req.Model)
	})

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("fake OpenAI-compatible backend listening on %s (model=%s)", addr, *model)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func streamResponse(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher := w.(http.Flusher)

	send := func(payload string) {
		_, _ = io.WriteString(w, "data: "+payload+"\n\n")
		flusher.Flush()
		time.Sleep(20 * time.Millisecond)
	}
	send(fmt.Sprintf(`{"id":"chatcmpl-fake","model":%q,"choices":[{"index":0,"delta":{"reasoning_content":"thinking..."}}]}`, model))
	send(fmt.Sprintf(`{"id":"chatcmpl-fake","model":%q,"choices":[{"index":0,"delta":{"reasoning_content":"more thinking..."}}]}`, model))
	parts := []string{"Hello ", "from ", "fake ", "backend!"}
	for _, p := range parts {
		send(fmt.Sprintf(`{"id":"chatcmpl-fake","model":%q,"choices":[{"index":0,"delta":{"content":%q}}]}`, model, p))
	}
	send(fmt.Sprintf(`{"id":"chatcmpl-fake","model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, model))
	send(`{"usage":{"prompt_tokens":5,"completion_tokens":4,"total_tokens":9}}`)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
	_ = strings.Contains // avoid unused import warning if refactored
}
