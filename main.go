// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	path := flag.String("config", "/etc/homelab-observer/config.json", "configuration JSON path")
	once := flag.Bool("once", false, "collect once and exit; does not serve MCP")
	flag.Parse()
	cfg, err := loadConfig(*path)
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}
	store, err := openStore(cfg.DatabasePath, cfg.RetentionDays)
	if err != nil {
		log.Fatalf("open state database: %v", err)
	}
	defer store.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *once {
		ready := make(chan struct{})
		go collectorLoop(ctx, cfg, store, ready)
		<-ready
		fmt.Println("Collection attempted. Check authenticated get_overview for per-source freshness and errors.")
		return
	}
	bearer, err := readSecret(cfg.MCPBearerFile)
	if err != nil {
		log.Fatalf("MCP bearer credential unavailable: %v", err)
	}
	ready := make(chan struct{})
	go collectorLoop(ctx, cfg, store, ready)
	// No credentials, nodes, internal URLs or keys are logged.
	srv := &http.Server{Addr: cfg.Listen, Handler: handler(store, bearer),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 12 * time.Second,
		WriteTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 32 << 10}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("HomelabObserver read-only MCP listening on %s", cfg.Listen)
	if err = srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("MCP server: %v", err)
	}
}
