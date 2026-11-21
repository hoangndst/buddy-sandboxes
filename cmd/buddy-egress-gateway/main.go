package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hoangndst/buddy-sandboxes/internal/egressgateway"
)

func main() {
	addr := flag.String("addr", ":8080", "Address and port for the egress gateway to listen on")
	flag.Parse()

	hmacSecretStr := os.Getenv("EGRESS_GATEWAY_HMAC_SECRET")
	if strings.TrimSpace(hmacSecretStr) == "" || strings.Contains(hmacSecretStr, "REPLACE_WITH_HMAC_SECRET") {
		log.Fatal("EGRESS_GATEWAY_HMAC_SECRET environment variable is required")
	}

	server := egressgateway.NewServer([]byte(hmacSecretStr))
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           server,
		ReadHeaderTimeout: 5 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("Starting buddy-egress-gateway on %s", *addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Egress gateway listen error: %v", err)
		}
	}()

	<-stop
	log.Println("Shutting down buddy-egress-gateway...")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("Egress gateway graceful shutdown error: %v", err)
	}
	log.Println("buddy-egress-gateway stopped")
}
