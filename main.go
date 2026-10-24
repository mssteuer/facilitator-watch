package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	address := os.Getenv("FACILITATOR_WATCH_ADDR")
	if address == "" {
		address = "127.0.0.1:8501"
	}
	listen := flag.String("listen", address, "listen address (default loopback)")
	dbPath := flag.String("db", envOr("FACILITATOR_WATCH_DB", "/var/lib/facilitator-watch/facilitators.sqlite"), "SQLite database path")
	once := flag.Bool("once", false, "collect each fixed target once, without retries, then keep serving")
	collection := flag.Bool("collect", true, "enable collection (false serves retained evidence without provider requests)")
	cdpKeyFile := flag.String("cdp-key-file", os.Getenv("FACILITATOR_WATCH_CDP_KEY_FILE"), "optional Coinbase CDP key file; fixed read-only endpoint only")
	flag.Parse()
	key, err := collectionKey(*collection, *cdpKeyFile)
	if err != nil {
		log.Fatal("invalid Coinbase CDP key configuration")
	}
	syscall.Umask(0077)
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0750); err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := openLiveStore(ctx, *dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	store.cdpConfigured = key != nil
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	client := newPollClient()
	collectorDone := make(chan struct{})
	go func() {
		defer close(collectorDone)
		if *collection {
			runCollector(ctx, store, client, *once, key)
		}
	}()
	defer func() { stop(); <-collectorDone; client.CloseIdleConnections() }()
	server := &http.Server{Addr: *listen, Handler: NewLiveServer(store), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			log.Printf("shutdown: %v", err)
			_ = server.Close()
		}
	}()
	if !*collection {
		log.Printf("facilitator-watch listening on %s; collection disabled; serving retained evidence", *listen)
	} else if *once {
		log.Printf("facilitator-watch listening on %s; one initial /supported pass only, no retries; targets=%d", *listen, len(targets()))
	} else {
		log.Printf("facilitator-watch listening on %s; passive /supported polling every %s; targets=%d", *listen, pollInterval, len(targets()))
	}
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	<-shutdownDone
}

func collectionKey(enabled bool, path string) (*cdpKey, error) {
	if !enabled || path == "" {
		return nil, nil
	}
	return loadCDPKey(path)
}
func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
