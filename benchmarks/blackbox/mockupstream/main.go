// Command mockupstream provides deterministic OpenAI-compatible unary and SSE
// responses for black-box proxy measurements. It deliberately performs no
// inference work so configured delays define the upstream cost.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

type server struct {
	unaryDelay time.Duration
	chunkDelay time.Duration
	chunks     int
	requests   atomic.Uint64
	streams    atomic.Uint64
	failures   atomic.Uint64
}

func main() {
	var address string
	var unaryDelay, chunkDelay time.Duration
	var chunks int
	flag.StringVar(&address, "address", "127.0.0.1:0", "listen address")
	flag.DurationVar(&unaryDelay, "unary-delay", 0, "delay before a unary response")
	flag.DurationVar(&chunkDelay, "stream-chunk-delay", 0, "delay before each SSE chunk")
	flag.IntVar(&chunks, "stream-chunks", 4, "number of SSE content chunks")
	flag.Parse()
	if chunks <= 0 {
		fmt.Fprintln(os.Stderr, "mockupstream: -stream-chunks must be positive")
		os.Exit(2)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mockupstream:", err)
		os.Exit(1)
	}
	backend := &server{unaryDelay: unaryDelay, chunkDelay: chunkDelay, chunks: chunks}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/stats", backend.stats)
	mux.HandleFunc("/v1/chat/completions", backend.chat)

	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	ready := map[string]any{
		"address": listener.Addr().String(),
		"url":     "http://" + listener.Addr().String(),
	}
	if err := json.NewEncoder(os.Stdout).Encode(ready); err != nil {
		fmt.Fprintln(os.Stderr, "mockupstream:", err)
		os.Exit(1)
	}
	if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "mockupstream:", err)
		os.Exit(1)
	}
}

func (s *server) chat(writer http.ResponseWriter, request *http.Request) {
	s.requests.Add(1)
	body, err := io.ReadAll(io.LimitReader(request.Body, 4<<20))
	if err != nil {
		s.failures.Add(1)
		http.Error(writer, "read request", http.StatusBadRequest)
		return
	}
	var envelope struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || strings.TrimSpace(envelope.Model) == "" {
		s.failures.Add(1)
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	if envelope.Stream {
		s.stream(writer, request, envelope.Model)
		return
	}
	if !wait(request.Context(), s.unaryDelay) {
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"id":      "chatcmpl-benchmark",
		"object":  "chat.completion",
		"created": 0,
		"model":   envelope.Model,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role": "assistant", "content": "ok",
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{
			"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2,
		},
	})
}

func (s *server) stream(writer http.ResponseWriter, request *http.Request, model string) {
	s.streams.Add(1)
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.WriteHeader(http.StatusOK)
	flusher, ok := writer.(http.Flusher)
	if !ok {
		s.failures.Add(1)
		return
	}
	for index := 0; index < s.chunks; index++ {
		if !wait(request.Context(), s.chunkDelay) {
			return
		}
		chunk := map[string]any{
			"id": "chatcmpl-benchmark", "object": "chat.completion.chunk",
			"created": 0, "model": model,
			"choices": []map[string]any{{
				"index": index, "delta": map[string]string{"content": "x"},
				"finish_reason": nil,
			}},
		}
		raw, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(writer, "data: %s\n\n", raw)
		flusher.Flush()
	}
	_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *server) stats(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]uint64{
		"requests": s.requests.Load(),
		"streams":  s.streams.Load(),
		"failures": s.failures.Load(),
	})
}

func wait(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
