//go:build !windows

package main

import (
	"net"
	"os"
	"os/exec"
	"syscall"
)

// inheritedListenerFD is where ExtraFiles places the first passed descriptor:
// stdin, stdout and stderr occupy 0 to 2.
const inheritedListenerFD = 3

// spawnChild starts a copy of this process holding the same listening socket,
// which is what an orphaned worker looks like from the outside.
//
// The socket is passed as a file descriptor rather than re-bound, because that
// is how the defect actually occurs: a pre-forked worker shares the parent's
// listener, so the port stays busy after the process the orchestrator was
// watching has exited. Re-binding would simply fail with "address already in
// use" and reproduce nothing.
//
// The child gets its own process group so that killing the parent's group does
// not take it along, which is the entire point of SC012.
func spawnChild(listener net.Listener) {
	tcp, ok := listener.(*net.TCPListener)
	if !ok {
		return
	}

	file, err := tcp.File()
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()

	self, err := os.Executable()
	if err != nil {
		return
	}

	cmd := exec.Command(self, "-child") // #nosec G204 -- re-executes this same binary
	cmd.ExtraFiles = []*os.File{file}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	_ = cmd.Start()
}
