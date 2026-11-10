// Package conformance runs black-box protocol checks against any NNTP address. A server under
// test is given as "host:port" (plus optional credentials, TLS config and a group to post
// into), and every check speaks the wire protocol with a strict client (package client): a
// bare LF, a missing terminator, bad dot-stuffing or a stray byte after a reply fails the
// check that saw it.
//
// Entry points:
//
//	func Run(tb testing.TB, o Options) Report   // fails tb when a check fails, unless o.ReportOnly
//	func RunVerbose(tb testing.TB, o Options) Report // Run, logging every result that is not a Skip first
//	func Check(o Options) (Report, error)       // no testing: err only when the server is unreachable
//	func Names() []string                       // the check names in report order (Options.Skip)
//	func (r Report) ByName() map[string]Result  // the results by check name
//	func (r Report) Require(tb testing.TB, names ...string) // fails tb per named check that did not Pass (a Skip and an unknown name count)
//	func (r Report) RequireNoFail(tb testing.TB)            // fails tb per Fail (and for an empty report)
//
// Every check opens its own connection, so one failure cannot poison the next. A check whose
// capability the server does not advertise (CAPABILITIES) is reported as Skip, not Fail, with
// Result.Missing naming the capability and Detail "<Missing> not advertised"; the
// capability list itself is checked for format and for consistency with behaviour (READER →
// GROUP works, OVER → OVER works, POST/IHAVE/STREAMING per RFC 4644). Options.Skip names checks
// to leave out.
// Options.Allow tolerates a known server bug: a Fail of a check whose Detail contains the
// given substring becomes a Skip "allowed: <Detail>", the passing part of the check is kept.
// Options.StartTLS configures the STARTTLS handshake (nil: not verified, said in the Detail).
// Options.StrictOverviewFmt additionally requires the RFC-preferred :bytes/:lines
// metadata names; by default the RFC-permitted Bytes:/Lines: aliases remain accepted.
//
// Checks (Result.RFC names the section):
//
//   - RFC 3977: greeting code (200/201) and CRLF; CAPABILITIES format (101, VERSION line,
//     terminator); MODE READER (a 480 from a server that lists AUTHINFO is accepted: with
//     Options.User the check logs in and asks again); GROUP and LISTGROUP codes and ranges (211/411/412), the range
//     forms "n", "n-m" (also m < n: empty) and "n-", and the current article LISTGROUP leaves;
//     ARTICLE/HEAD/BODY/STAT by number, message-id and current article (220-223, 412, 420, 423,
//     430) with the dot-stuffing and the terminator checked by the strict client; NEXT/LAST
//     (421/422); LIST variants, ACTIVE.TIMES (when the LIST capability names it), HEADERS
//     (mandatory with HDR) and OVERVIEW.FMT; OVER field count; HDR by current article, range
//     and message-id against the article's header (225, 412, 420, 423, 430); NEWGROUPS (both
//     date forms, nothing for a future date); NEWNEWS (message-ids, nothing for a future
//     date); POST into Options.PostGroup (340/240, then the article is found by its
//     message-id with the dot-stuffed body line intact); DATE format; HELP; QUIT 205 then EOF
//     with nothing pending; unknown command 500; bad arguments 501; CRLF-only line ends and
//     nothing pending after any reply.
//   - RFC 4643: AUTHINFO USER/PASS sequences (381/281/481/482, 480 before authentication on
//     restricted commands, 502 after success) when Options.User is set.
//   - RFC 4644: MODE STREAM (203), CHECK (238/431/438), TAKETHIS (239/439) and IHAVE
//     (335/235/435/436/437) when STREAMING (or IHAVE) is advertised and Options.PostGroup is
//     set; the articles posted carry a unique Message-ID under @conformance.invalid.
//   - RFC 4642 (STARTTLS advertised on a plain connection; Options.TLS skips them; no login is
//     done, STARTTLS is not valid after AUTHINFO): starttls (382, no plaintext behind it, the
//     handshake, no new greeting, CAPABILITIES afterwards lists neither STARTTLS nor
//     MODE-READER; a 580 passes with a Detail, the server may lack a certificate),
//     starttls-twice (502 over TLS) and starttls-pipelined (a DATE sent in the same write as
//     STARTTLS must neither be answered in plaintext before the handshake nor run after it,
//     RFC 8143 §4, CVE-2011-0411; the check does the handshake and listens), and
//     tls-capabilities (with Options.TLS, implicit TLS: CAPABILITIES lists no STARTTLS; a
//     SASL EXTERNAL without a client certificate is only noted). The certificate is verified
//     only when Options.StartTLS asks for it; without it the handshake accepts TLS 1.0 and up.
//   - RFC 8054 (COMPRESS DEFLATE advertised, also after MODE READER or AUTHINFO; the checks log
//     in only when the capability needs it): compress (206 and both directions deflated,
//     CAPABILITIES afterwards lists no COMPRESS, STARTTLS or MODE-READER and AUTHINFO without
//     arguments, DATE, HELP and LIST OVERVIEW.FMT, commands pipelined in one write and in
//     back-to-back writes, QUIT), compress-unknown (503 for an unknown algorithm, 501 for none,
//     the connection stays plain), compress-twice (502 for a second COMPRESS DEFLATE (502 or
//     503 for an unknown algorithm), and 502 for STARTTLS and AUTHINFO when advertised before; a 403 to COMPRESS skips
//     the check), compress-upload and compress-upload-lines (with Options.PostGroup: a POST of
//     3000 dot-stuffed lines in one write and line by line is answered within the hang guard
//     (the INN issue #269 hang) and reads back intact).
//
// The checks run concurrently (Options.Workers at a time, default 4, each on its own
// connection), the ones that post after all the others; Report.Results keeps the order of
// Names, and Report.Capabilities is the list one extra connection read right after the
// greeting. A panic inside a check is that check's Fail. A server that greets with 400 or 502
// fails the greeting check ("service unavailable: <line>") and every other check is skipped.
//
// cmd/nntp-conformance is the command-line front end: -addr -user -pass -group -tls
// -tls-insecure -ca -servername -starttls-verify -allow -deadline -workers -skip -json
// -strict-overview-fmt; it prints the report and exits 1 on any Fail,
// 2 on a usage error or an unreachable server.
//
// Self-test: the kit's own fakeserver Store server passes every check (the handler's
// conformance test), and a scripted fake with deliberate mistakes produces the matching Fail
// results.
package conformance
