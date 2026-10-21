# Maintainer notes

## Go versions

`go.mod` says `go 1.26.0`. CI (`.github/workflows/test.yml`) runs Go `oldstable` and `stable`
on Linux and macOS, so today it tests 1.26, the minimum, and 1.27.

- **Once Go 1.28 is out,** `oldstable` becomes 1.27 and CI stops testing 1.26, the minimum.
  Either raise the minimum then (`go 1.27.0`, and a new minor version, since that breaks
  anyone on an older Go), or add `1.26.x` to the `go:` list in the workflow.
- **macOS needs Go 1.24 or later.** Earlier Go writes binaries without an `LC_UUID` load
  command, and current macOS refuses to load them ("dyld: missing LC_UUID load command").
  Do not put a Go before 1.24 back into the macOS jobs.
- **Windows is not tested.** `proc` and its tests send SIGTERM and SIGHUP and read
  `syscall.WaitStatus`.
