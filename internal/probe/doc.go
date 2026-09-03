// Package probe performs the black-box observations: the HTTP prober (with
// httptrace-based connection forensics), the readiness prober, and the raw TCP
// listener-acceptance prober.
//
// These are deliberately three independent observers. A server can accept TCP
// without ever responding, or exit while an orphaned child still holds the port;
// neither is detectable by looking at HTTP responses alone.
//
// Sensitive headers are redacted at the recording boundary, and response bodies
// are read under a hard byte cap and discarded.
//
// Implemented in Phase 2.
package probe
