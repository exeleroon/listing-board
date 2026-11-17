package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"listingboard/internal/brochure"
	"listingboard/internal/env"
	"listingboard/internal/httpapi"
	"listingboard/internal/store"
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}
	dataDir := env.String("DATA_DIR", "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatal(err)
	}
	st, err := store.OpenRetry(dsn, 30*time.Second)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	// Volumes written by the previous version keep brochures under the sheet id only.
	moved, err := brochure.MigrateLegacyLayout(context.Background(), st, dataDir)
	if err != nil {
		log.Printf("brochure layout migration: %v", err)
	}
	if moved > 0 {
		log.Printf("moved %d brochure(s) to the current layout", moved)
	}

	srv := &http.Server{
		Addr:              ":" + env.String("PORT", "8080"),
		Handler:           httpapi.New(st, dataDir, os.Getenv("WEB_DIR")).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	log.Printf("listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
