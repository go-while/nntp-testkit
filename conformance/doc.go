// Package conformance runs black-box protocol checks against any NNTP address. A server under
// test is given as "host:port" (plus optional credentials, TLS config and a group to post
// into), and every check speaks the wire protocol with a strict client (package client): a
// bare LF, a missing terminator, bad dot-stuffing or a stray byte after a reply fails the
// check that saw it.
//
// Entry points:
//
//	func Run(tb testing.TB, o Options) Report   // fails tb when a check fails, unless o.ReportOnly
//	func Check(o Options) (Report, error)       // no testing: err only when the server is unreachable
//
// Every check opens its own connection, so one failure cannot poison the next. A check whose
// capability the server does not advertise (CAPABILITIES) is reported as Skip, not Fail; the
// capability list itself is checked for format and for consistency with behaviour (READER →
// GROUP works, OVER → OVER works, POST/IHAVE/STREAMING per RFC 4644). Options.Skip names checks
// to leave out.
//
// Checks (Result.RFC names the section):
//
//   - RFC 3977: greeting code (200/201) and CRLF; CAPABILITIES format (101, VERSION line,
//     terminator); MODE READER; GROUP and LISTGROUP codes and ranges (211/411/412);
//     ARTICLE/HEAD/BODY/STAT by number, message-id and current article (220-223, 412, 420, 423,
//     430) with the dot-stuffing and the terminator checked by the strict client; NEXT/LAST
//     (421/422); LIST variants and OVERVIEW.FMT; OVER field count; DATE format; HELP; QUIT 205
//     then EOF with nothing pending; unknown command 500; bad arguments 501; CRLF-only line
//     ends and nothing pending after any reply.
//   - RFC 4643: AUTHINFO USER/PASS sequences (381/281/481/482, 480 before authentication on
//     restricted commands, 502 after success) when Options.User is set.
//   - RFC 4644: MODE STREAM (203), CHECK (238/431/438), TAKETHIS (239/439) and IHAVE
//     (335/235/435/436/437) when STREAMING (or IHAVE) is advertised and Options.PostGroup is
//     set; the articles posted carry a unique Message-ID under @conformance.invalid.
//
// cmd/nntp-conformance is the command-line front end: -addr -user -pass -group -tls -deadline
// -json; it prints the report and exits 1 on any Fail.
//
// Self-test: the kit's own fakeserver Store server passes every check (the handler's
// conformance test), and a scripted fake with deliberate mistakes produces the matching Fail
// results.
package conformance
