# Validation for DoucheSync 0.1.0

Built on Linux x86-64 with Go 1.27.1 on 2026-10-07.

- All 20 automated Go tests pass, including the race detector.
- `go vet -buildvcs=false ./...` passes.
- Three separate client processes and one discovery server pass the process smoke test.
- Initial files, remote updates, and enabled deletions converge across all three clients.
- Discovery writes no files to its working directory.
- All processes shut down cleanly on SIGTERM.
- Linux x86-64 binary built with CGO_ENABLED=0 and exercised in the process test.
- Windows x86-64 binary built with CGO_ENABLED=0 by cross-compilation; Windows runtime behavior has not been tested.

Coverage includes simultaneous transfers, multiple-folder isolation, persistent
state after restart, concurrent edits, edit/delete conflicts, deletion disabled,
cached discovery peers, unsigned/replayed request rejection, certificate pinning,
announcement tampering, path traversal, symlinks, invalid transfers,
edits during download, ignore rules, folder locks, missing/failed state storage,
scan failure behavior, and TOML validation.

This is an initial release. See README.md for current limits and setup.
