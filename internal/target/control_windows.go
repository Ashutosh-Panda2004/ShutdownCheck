//go:build windows

package target

import (
	"fmt"
	"io"
)

// Windows has no SIGTERM. Graceful termination there is a different mechanism
// entirely — console control events, job objects, service stop requests — and
// each has its own applicability rules.
//
// The one thing this package must never do is map SIGTERM onto
// TerminateProcess. That is the equivalent of SIGKILL, so every Windows verdict
// would describe a hard kill while claiming to describe a graceful shutdown.
// Failing loudly is the only honest option until the model is implemented
// properly. See docs/adr/0008-windows-support-strategy.md.
const windowsGuidance = "process and command targets need POSIX signals, which Windows does not have. " +
	"Use --docker to test the container you actually deploy, or run shutdowncheck on Linux or macOS. " +
	"Native Windows termination is planned for v1.2"

// ProcessTargetsSupported reports whether this platform can deliver the signals
// a process or command target needs.
func ProcessTargetsSupported() bool { return false }

func newCommandControl(CommandOptions, io.Writer, io.Writer) (processControl, error) {
	return nil, fmt.Errorf("%w: %s", ErrUnsupportedPlatform, windowsGuidance)
}

func newAttachControl(int) (processControl, error) {
	return nil, fmt.Errorf("%w: %s", ErrUnsupportedPlatform, windowsGuidance)
}
