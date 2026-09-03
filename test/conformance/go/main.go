// Command conformance is a minimal HTTP server whose shutdown behaviour is
// selected with -mode.
//
// It is the reference implementation of the conformance suite: every other
// language mirrors these modes, and the end-to-end harness asserts that
// ShutdownCheck reaches the same verdict for each of them regardless of stack.
//
// It doubles as documentation. Reading `correct` next to the broken modes shows
// what the seven stages of termination actually look like in Go, and what each
// individual mistake does to real traffic.
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

// Modes are the shutdown behaviours this server can exhibit. Each broken mode
// is a real mistake seen in production code, not a synthetic one.
const (
	// ModeCorrect implements all seven stages properly.
	ModeCorrect = "correct"
	// ModeIgnoreSignal never handles SIGTERM, so the orchestrator must kill it.
	ModeIgnoreSignal = "ignore-signal"
	// ModeInstantClose closes the listener immediately and never flips
	// readiness: correct by the naive reading, and a source of 502s behind any
	// load balancer.
	ModeInstantClose = "instant-close"
	// ModeNoReadinessFlip drains correctly but never tells anyone it is going.
	ModeNoReadinessFlip = "no-readiness-flip"
	// ModeAbruptReset destroys live connections instead of draining them.
	ModeAbruptReset = "abrupt-reset"
	// ModeSlowDrain drains, but takes longer than the grace period allows.
	ModeSlowDrain = "slow-drain"
	// ModeEarlyExit walks out while requests are still being served.
	ModeEarlyExit = "early-exit"
	// ModeOrphanChild exits while a child keeps the port bound.
	ModeOrphanChild = "orphan-child"
	// ModeListenerNeverCloses keeps accepting new work forever.
	ModeListenerNeverCloses = "listener-never-closes"
	// ModeSlowReadiness eventually flips readiness, far too late.
	ModeSlowReadiness = "slow-readiness"
	// ModeReadinessFlap starts failing readiness and then recovers.
	ModeReadinessFlap = "readiness-flap"
	// ModeNonZeroExit shuts down cleanly but reports failure.
	ModeNonZeroExit = "nonzero-exit"
	// ModeAcceptNoResponse accepts connections after the signal and never
	// answers them, so callers hang for their full timeout.
	ModeAcceptNoResponse = "accept-no-response"
)

var (
	ready    atomic.Bool
	stalling atomic.Bool
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	mode := flag.String("mode", ModeCorrect, "shutdown behaviour to exhibit")
	work := flag.Duration("work", 60*time.Millisecond, "how long each request takes")
	lameDuck := flag.Duration("lame-duck", 400*time.Millisecond, "how long to keep serving after the signal")
	child := flag.Bool("child", false, "internal: run as the orphaned child")
	flag.Parse()

	ready.Store(true)

	listener, err := openListener(*addr, *child)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}

	srv := &http.Server{Handler: handler(*work), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(listener) }()

	fmt.Printf("listening on %s\n", listener.Addr().String())

	// A child holding the same port is how an orphaned worker outlives its
	// parent; it must not itself react to the signal.
	if *child {
		select {}
	}

	if *mode == ModeIgnoreSignal {
		// No handler at all beyond blocking: nothing can stop this but SIGKILL.
		signal.Ignore(syscall.SIGTERM)
		select {}
	}

	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM, os.Interrupt)
	<-sigterm

	shutdown(srv, listener, *mode, *lameDuck)
}

// openListener binds the address, or adopts the socket inherited from the
// parent.
//
// An orphaned worker does not rebind the port — it already holds the same
// listening socket, which is exactly why the port stays busy after the process
// the orchestrator was watching has gone. Rebinding would simply fail with
// "address already in use" and reproduce nothing.
func openListener(addr string, child bool) (net.Listener, error) {
	if !child {
		return net.Listen("tcp", addr)
	}
	return net.FileListener(os.NewFile(inheritedListenerFD, "inherited-listener"))
}

func handler(work time.Duration) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/work", func(w http.ResponseWriter, r *http.Request) {
		if stalling.Load() {
			// Accepted, never answered: the caller waits out its whole timeout.
			<-r.Context().Done()
			return
		}
		time.Sleep(work)
		fmt.Fprintln(w, "ok")
	})

	return mux
}

func shutdown(srv *http.Server, listener net.Listener, mode string, lameDuck time.Duration) {
	switch mode {
	case ModeInstantClose:
		// Stage 2 skipped and stage 3 skipped: readiness never flips and the
		// listener shuts at once, so traffic still being routed here is refused.
		_ = srv.Shutdown(context.Background())

	case ModeNoReadinessFlip:
		time.Sleep(lameDuck)
		_ = srv.Shutdown(context.Background())

	case ModeAbruptReset:
		ready.Store(false)
		time.Sleep(lameDuck)
		// Close destroys live connections rather than draining them.
		_ = srv.Close()

	case ModeSlowDrain:
		ready.Store(false)
		time.Sleep(lameDuck)
		// Longer than any sensible grace period, so the kill lands first.
		time.Sleep(2 * time.Minute)
		_ = srv.Shutdown(context.Background())

	case ModeEarlyExit:
		ready.Store(false)
		time.Sleep(lameDuck)
		// Leaves without waiting for anything still in progress.
		os.Exit(0)

	case ModeOrphanChild:
		ready.Store(false)
		spawnChild(listener)
		time.Sleep(lameDuck)
		os.Exit(0)

	case ModeListenerNeverCloses:
		ready.Store(false)
		// Keeps taking new work indefinitely; drain can never finish.
		select {}

	case ModeSlowReadiness:
		time.Sleep(3 * time.Second)
		ready.Store(false)
		_ = srv.Shutdown(context.Background())

	case ModeReadinessFlap:
		ready.Store(false)
		time.Sleep(300 * time.Millisecond)
		// Recovering re-registers a terminating instance with the balancer.
		ready.Store(true)
		time.Sleep(lameDuck)
		_ = drainGracefully(srv)

	case ModeNonZeroExit:
		ready.Store(false)
		time.Sleep(lameDuck)
		_ = drainGracefully(srv)
		os.Exit(3)

	case ModeAcceptNoResponse:
		ready.Store(false)
		stalling.Store(true)
		select {}

	default:
		// The correct sequence, in order:
		//   1. the signal arrived
		//   2. stop advertising readiness, so de-registration begins
		//   3. keep serving while that propagates
		//   4. stop accepting, 5. signal close, 6. drain
		//   7. exit cleanly, inside the budget
		ready.Store(false)
		time.Sleep(lameDuck)
		_ = drainGracefully(srv)
	}
}

func drainGracefully(srv *http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}
