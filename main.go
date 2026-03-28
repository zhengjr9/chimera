package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	cfgPath := "config.yaml"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	p := NewProxy(cfg)

	mux := http.NewServeMux()
	auth := authMiddleware(cfg.Server.APIKeys)

	mux.Handle("/v1/messages", auth(http.HandlerFunc(p.handleMessages)))
	mux.Handle("/v1/chat/completions", auth(http.HandlerFunc(p.handleChatCompletions)))
	mux.Handle("/v1/responses", auth(http.HandlerFunc(p.handleResponses)))
	mux.Handle("/v1/responses/compact", auth(http.HandlerFunc(p.handleResponsesCompact)))
	mux.Handle("/v1/models", auth(http.HandlerFunc(p.handleModels)))

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	srv := &http.Server{Addr: addr, Handler: corsMiddleware(mux)}

	go func() {
		log.Printf("chimera listening on %s", addr)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

func authMiddleware(apiKeys []string) func(http.Handler) http.Handler {
	if len(apiKeys) == 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	set := make(map[string]bool, len(apiKeys))
	for _, k := range apiKeys {
		set[k] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := extractAPIKey(r)
			if set[key] {
				next.ServeHTTP(w, r)
				return
			}
			// Determine error format from Accept header or path
			if strings.HasSuffix(r.URL.Path, "/messages") {
				writeClaudeError(w, http.StatusUnauthorized, "authentication_error", "invalid api key")
			} else {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid api key")
			}
		})
	}
}

func extractAPIKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	if key := r.Header.Get("x-api-key"); key != "" {
		return key
	}
	if key := r.Header.Get("Anthropic-Version"); key != "" {
		// Some clients use Anthropic-Version header; not a key, but check api-key header
	}
	return r.Header.Get("api-key")
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-api-key, Anthropic-Version, anthropic-beta")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
