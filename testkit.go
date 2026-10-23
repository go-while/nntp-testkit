// Package testkit is the root of nntp-testkit: shared NNTP test tools (a scripted or stateful
// fake server, a strict test client, an external-process runner) for projects that implement or
// consume NNTP. Only the standard library is used.
//
// The sub-packages are client, fakeserver, proc, yenc and conformance (stub). This package holds what
// they share: the hang guard.
package testkit

import (
	"os"
	"time"
)

// DefaultDeadline is the hang guard when NNTP_TEST_DEADLINE is not set.
const DefaultDeadline = 60 * time.Second

// EnvDeadline is the environment variable that overrides the hang guard: a Go duration such as
// "120s".
const EnvDeadline = "NNTP_TEST_DEADLINE"

// Deadline returns the hang guard of the kit: the outer bound of every blocking step (dial,
// read, write, waits). It only stops a hung test; a passing test never waits on it. It is
// NNTP_TEST_DEADLINE when that is a positive duration, else DefaultDeadline. Each package also
// accepts an explicit deadline per server or connection, which wins over this default.
func Deadline() time.Duration {
	return deadlineFromEnv(os.Getenv(EnvDeadline), DefaultDeadline)
}

// deadlineFromEnv parses v; an empty, invalid or non-positive value gives def.
func deadlineFromEnv(v string, def time.Duration) time.Duration {
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
