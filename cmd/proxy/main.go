package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/gem/webdav-proxy/internal/config"
	"github.com/gem/webdav-proxy/internal/server"
)

func main() {
	cfg := config.Load()
	endpoint := os.Getenv("UPSTREAM_ENDPOINT")
	if endpoint == "" {
		log.Fatal("UPSTREAM_ENDPOINT is required")
	}
	srv, err := server.New(cfg, endpoint)
	if err != nil {
		log.Fatalf("server init: %v", err)
	}
	defer srv.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	srv.StartBackground(ctx) // 启缓存淘汰 worker

	hs := &http.Server{Addr: cfg.ListenAddr, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		_ = hs.Shutdown(context.Background())
	}()
	log.Printf("listening on %s (upstream %s)", cfg.ListenAddr, endpoint)
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}
