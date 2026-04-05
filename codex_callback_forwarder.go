package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const codexCallbackPort = 1455

type codexCallbackForwarder struct {
	server *http.Server
	done   chan struct{}
}

var (
	codexForwarderMu sync.Mutex
	codexForwarder   *codexCallbackForwarder
)

func startCodexCallbackForwarder(targetBase string) error {
	codexForwarderMu.Lock()
	defer codexForwarderMu.Unlock()

	if codexForwarder != nil {
		return nil
	}

	addr := fmt.Sprintf("127.0.0.1:%d", codexCallbackPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen codex callback %s: %w", addr, err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := strings.TrimSuffix(targetBase, "/")
		if raw := r.URL.RawQuery; raw != "" {
			if strings.Contains(target, "?") {
				target += "&" + raw
			} else {
				target += "?" + raw
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, target, http.StatusFound)
	})

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Second,
	}
	done := make(chan struct{})
	go func() {
		if errServe := srv.Serve(ln); errServe != nil && !errors.Is(errServe, http.ErrServerClosed) {
			log.Printf("codex callback forwarder stopped unexpectedly: %v", errServe)
		}
		close(done)
	}()

	codexForwarder = &codexCallbackForwarder{server: srv, done: done}
	log.Printf("codex callback forwarder listening on %s forwarding to %s", addr, targetBase)
	return nil
}

func stopCodexCallbackForwarder() {
	codexForwarderMu.Lock()
	forwarder := codexForwarder
	codexForwarder = nil
	codexForwarderMu.Unlock()
	if forwarder == nil || forwarder.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = forwarder.server.Shutdown(ctx)
	select {
	case <-forwarder.done:
	case <-time.After(2 * time.Second):
	}
}
