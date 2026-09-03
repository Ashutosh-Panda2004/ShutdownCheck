//go:build windows

package main

import "net"

// inheritedListenerFD is unused on Windows; the orphaned-listener mode needs
// POSIX descriptor passing and process groups, and the conformance suite only
// runs where signals exist. These stubs keep the package building so `go vet`
// still covers the rest of the file here.
const inheritedListenerFD = 3

func spawnChild(net.Listener) {}
