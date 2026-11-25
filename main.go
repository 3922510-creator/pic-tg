package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/3922510-creator/pic-tg/config"
	"github.com/3922510-creator/pic-tg/server"
	"github.com/3922510-creator/pic-tg/storage"
)

func main() {
	configPath := flag.String("config", "./config.json", "path to config file")
	flag.Parse()

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config from %s: %v", *configPath, err)
	}
	if cfg.Initialized {
		log.Printf("initialized new config at %s with a generated auth_token and api_key (mode 0600); inspect the file to retrieve credentials", *configPath)
	}

	if cfg.StateDir == "" {
		cfg.StateDir = filepath.Join(filepath.Dir(*configPath), "data")
	}

	db, err := storage.OpenDB(cfg.StateDir)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	srv, err := server.NewServer(cfg, db)
	if err != nil {
		log.Fatalf("failed to initialize server: %v", err)
	}

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	httpServer := &http.Server{
		Addr:        addr,
		Handler:     srv.Handler(),
		ReadTimeout: 120 * time.Second,
		// Leave time for the bounded upload handler to return a JSON error.
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("Pic TG listening on %s (public: %s)", addr, cfg.PublicOrigin)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server listen failed: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("shutting down Pic TG gracefully...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
	log.Println("Pic TG stopped")
}
