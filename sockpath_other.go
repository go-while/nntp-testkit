//go:build !linux

package testkit

// MaxSocketPath is the longest path, in bytes, a unix-domain socket address can have on this
// system (sun_path is 108 bytes on Linux and Solaris, 104 on macOS and the BSDs, each with a
// terminating NUL). Where it is not certain the smaller value is used.
const MaxSocketPath = 103
