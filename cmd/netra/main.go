// Command netra is a self-hosted IP & location intelligence toolkit for
// authorized security assessments. See README.md for the usage disclaimer.
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

	"github.com/spignelon/netra/internal/app"
	"github.com/spignelon/netra/internal/auth"
	"github.com/spignelon/netra/internal/config"
	"github.com/spignelon/netra/internal/db"
	"github.com/spignelon/netra/internal/geoip"
	"github.com/spignelon/netra/internal/handlers"
)

func main() {
	cfg := config.Load()

	database, err := db.Open(cfg.DBPath())
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer database.Close()
	_ = database.PurgeExpiredSessions()

	am := auth.NewManager(database, cfg.CookieSecure, cfg.TrustProxy)
	geo := geoip.New()

	h, err := handlers.New(database, cfg, am, geo)
	if err != nil {
		log.Fatalf("handlers: %v", err)
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           app.NewMux(h, am),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	// Graceful shutdown.
	go func() {
		log.Printf("Netra listening on :%s (base URL %s)", cfg.Port, cfg.BaseURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
