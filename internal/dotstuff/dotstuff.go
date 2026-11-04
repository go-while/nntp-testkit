// Package dotstuff is the dot-stuffing of NNTP multi-line blocks (RFC 3977 section 3.1.1),
// shared by the fake server (replies) and the client (client.Stuff).
package dotstuff

import "strings"

// Append appends lines to out, dot-stuffing those that start with "." (one more "." in front).
// The lines carry no line ends; the terminator is not added.
func Append(out, lines []string) []string {
	for _, l := range lines {
		if strings.HasPrefix(l, ".") {
			l = "." + l
		}
		out = append(out, l)
	}
	return out
}
