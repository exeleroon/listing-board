package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"listingboard/internal/env"
	"listingboard/internal/store"
	"listingboard/internal/worker"
)

const (
	defaultPause = 2 * time.Second
	pollEvery    = 500 * time.Millisecond
	connectWait  = 60 * time.Second
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}
	dataDir := env.String("DATA_DIR", "data")
	pause, err := env.Duration("STEP_PAUSE", defaultPause)
	if err != nil {
		log.Fatal(err)
	}
	st, err := store.OpenRetry(dsn, connectWait)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("worker started, data dir %s, step pause %s", dataDir, pause)
	if err := worker.New(st, dataDir, pause, pollEvery).Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
	log.Print("worker stopped")
}
