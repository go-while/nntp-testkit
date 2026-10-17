// Package conformance will hold black-box protocol checks that run against any NNTP address:
// a server under test is given as "host:port" (plus optional credentials) and every check
// speaks the wire protocol with a strict client (package client).
//
// Planned (not implemented yet; this file is the stub the plan asked for):
//
//   - RFC 3977: greeting codes (200/201/400/502), CAPABILITIES format, MODE READER, GROUP and
//     LISTGROUP ranges and codes (211/411), article retrieval by number/message-id/current
//     (220-223, 412, 420, 423, 430), NEXT/LAST (421/422), LIST variants, OVER/XOVER field
//     count and ordering, DATE format, HELP, QUIT (205), CRLF-only line ends, dot-stuffing of
//     every multi-line reply and the ".\r\n" terminator, nothing pending after a reply, and
//     behaviour on an unknown command (500) or bad arguments (501).
//   - RFC 4643: AUTHINFO USER/PASS codes (381/281/481/482), 480 before authentication on
//     restricted commands, no AUTHINFO after a successful one (502).
//   - RFC 4644: MODE STREAM (203), CHECK (238/431/438), TAKETHIS (239/439), and the IHAVE
//     fallback (335/235/435/436/437).
//
// Shape: `func Run(tb testing.TB, addr string, o Options) Report` with per-check results
// (name, pass/fail/skip, detail), skipping checks whose capability the server does not
// advertise, and a `cmd/nntp-conformance` CLI that prints the report. Checks never modify the
// server beyond posting into a group named in Options.
package conformance
