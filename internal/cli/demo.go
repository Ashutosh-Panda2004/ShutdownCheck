package cli

import (
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/target"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/pkg/schema"
)

// demoServerCommand is hidden from help on purpose. It exists so that `demo`
// has something real to terminate, and is not a supported entry point: the
// underscores mark it as private, and it refuses to run without the exact
// arguments demo passes.
const demoServerCommand = "__demo-server"

// demoCommand runs the tool against a deliberately broken service that this
// binary spawns of itself.
//
// The point is that the report is real. The demo goes through the ordinary run
// pipeline against a real process receiving a real signal, so there is no code
// path in the tool that prints a report it did not measure.
func demoCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)

	profile := fs.String("profile", "kubernetes", "shutdown profile to judge the demo under")
	format := fs.String("format", "human", "output format: human, json, junit, markdown, ndjson, html")
	noColor := fs.Bool("no-color", false, "disable coloured output")
	fs.Usage = func() { printDemoUsage(stderr) }

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printDemoUsage(stderr)
			return schema.ExitPass
		}
		return schema.ExitUsage
	}

	if !target.ProcessTargetsSupported() {
		printDemoUnavailable(stderr)
		return schema.ExitTarget
	}

	self, err := os.Executable()
	if err != nil {
		writefBestEffort(stderr, "shutdowncheck: cannot locate this binary to run the demo: %v\n", err)
		return schema.ExitInternal
	}

	port, err := freeLocalPort()
	if err != nil {
		writefBestEffort(stderr, "shutdowncheck: cannot reserve a port for the demo: %v\n", err)
		return schema.ExitInternal
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	writeBestEffort(stderr, demoPreamble)

	// runCommand takes the arguments after the subcommand name, like the
	// dispatcher hands it; a leading "run" would stop flag parsing and the
	// demo would die with "--url is required".
	runArgs := []string{
		"--url", "http://" + addr + "/work",
		"--readiness-url", "http://" + addr + "/readyz",
		"--profile", *profile,
		"--format", *format,
		"--ensure-in-flight", "12",
		"--warmup", "1s",
		"--steady", "1s",
		"--grace-period", "10s",
		"--accept-window", "1s",
	}
	if *noColor {
		runArgs = append(runArgs, "--no-color")
	}
	runArgs = append(runArgs, "--", self, demoServerCommand, "--addr", addr)

	return runCommand(runArgs, stdout, stderr)
}

const demoPreamble = `Running a real check against a deliberately broken service.

The service below closes its listening socket the instant SIGTERM arrives, which
is what most "graceful shutdown" examples tell you to do. Under a load balancer
it produces 502s, because de-registration has not propagated yet.

`

func printDemoUsage(w io.Writer) {
	writeBestEffort(w, `Usage: shutdowncheck demo [flags]

Runs a real check against a deliberately broken service, so you can see what a
report looks like before pointing the tool at anything of your own.

Nothing is simulated: the service is a real process, it receives a real SIGTERM,
and the report is measured by the same code path as any other run.

Flags:
  --profile <name>   judge the demo under a different profile (default kubernetes)
  --format <name>    human, json, junit, markdown, ndjson, html
  --no-color         disable coloured output

Try the same evidence under another deployment model:
  shutdowncheck demo --profile strict
`)
}

func printDemoUnavailable(w io.Writer) {
	writeBestEffort(w, `shutdowncheck: the in-process demo needs POSIX signals, which this platform does not have.

You are getting this rather than a canned report because a demo that fabricated
its evidence would undermine the only thing this tool sells.

A container gets a real SIGTERM from the Docker daemon, so this works instead —
the same image serves as its own broken demo service:

  docker run -d --name shutdowncheck-demo -p 127.0.0.1:8080:8080 \
    ghcr.io/shutdowncheck/shutdowncheck:latest __demo-server --addr 0.0.0.0:8080
  shutdowncheck run --docker shutdowncheck-demo \
    --url /work --readiness-url /readyz --profile kubernetes
  docker rm -f shutdowncheck-demo

See docs/adr/0015-demo-without-recorded-fallback.md for why.
`)
}

func freeLocalPort() (int, error) {
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()

	return l.Addr().(*net.TCPAddr).Port, nil
}

// demoServer is the broken service the demo terminates.
//
// The defect is deliberate and is the one almost everyone ships: on SIGTERM it
// closes the listener immediately and never fails readiness. Behind any load
// balancer that is a burst of connection refused, which the ingress turns into
// 502s for real users.
func demoServer(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet(demoServerCommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "", "address to listen on")

	if err := fs.Parse(args); err != nil {
		return schema.ExitUsage
	}
	if *addr == "" {
		writeBestEffort(stderr, "shutdowncheck: __demo-server is internal to `shutdowncheck demo` and is not a supported command\n")
		return schema.ExitUsage
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/work", func(w http.ResponseWriter, _ *http.Request) {
		// Long enough that requests are reliably still in flight when the
		// signal lands, which is the whole point of the demonstration.
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})

	var ready atomic.Bool
	ready.Store(true)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		<-signals
		// The bug, in one line: the listener and every idle connection are torn
		// down at once, with no lame-duck window and no readiness flip.
		_ = srv.Close()
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		writefBestEffort(stderr, "demo server: %v\n", err)
		return schema.ExitInternal
	}
	return schema.ExitPass
}
