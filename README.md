# nntp-testkit

Shared NNTP test tools, standard library only, GPLv2 (see LICENSE).

| package | what |
|---|---|
| `client` | strict test client: `Dial`, `Cmd`, `ReadDotLines`, `ExpectNoPending`, `ParseCode`, `ExpectCode`, `ExpectLines`, `Unstuff` |
| `fakeserver` | fake NNTP server: scripted steps, a handler func, or a stateful `Store` served by the built-in RFC 3977 handler; command counters and fault hooks; `TLSConfig`; a streaming `PeerHandler` |
| `proc` | runs an external server binary for a test (stdio or TCP), stops it, collects its logs |
| `conformance` | stub: planned black-box RFC 3977/4643/4644 checks |

The hang guard of every blocking step is `NNTP_TEST_DEADLINE` (a Go duration, default 60s); a passing test
never waits on it.

Each package's `doc.go` holds its API. A worked example (fake upstream plus strict client) follows once the
packages land.

```bash
go vet ./... && go test -race -count=1 ./...
```
