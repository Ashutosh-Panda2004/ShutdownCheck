// Command fixture is a minimal HTTP server whose shutdown behaviour can be
// selected, so end-to-end tests can point shutdowncheck at a known defect and
// assert that it is found.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	mode := flag.String("mode", "correct", "correct | instant-close | ignore-signal | drop-inflight")
	work := flag.Duration("work", 60*time.Millisecond, "how long each request takes")
	lameDuck := flag.Duration("lame-duck", 400*time.Millisecond, "how long to keep serving after the signal")
	flag.Parse()

	var ready atomic.Bool
	ready.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/work", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(*work)
		fmt.Fprintln(w, "ok")
	})

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(listener) }()

	// Printed so a test can discover the port when one was not pinned.
	fmt.Printf("listening on %s\n", listener.Addr().String())

	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM, os.Interrupt)
	<-sigterm

	switch *mode {
	case "ignore-signal":
		// Never exits; the tool has to escalate to SIGKILL.
		select {}

	case "instant-close":
		// Correct by the naive reading, and wrong behind a load balancer:
		// readiness is not flipped and the listener shuts immediately.
		_ = srv.Shutdown(context.Background())

	case "drop-inflight":
		ready.Store(false)
		// Close destroys active connections instead of draining them.
		_ = srv.Close()

	default:
		ready.Store(false)
		time.Sleep(*lameDuck)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}
