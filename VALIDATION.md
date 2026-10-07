# Validation for DoucheSync 0.1.1

Built on Linux x86-64 with Go 1.27.1 on 2026-10-07.

- All 24 automated Go tests pass locally on Linux, including the race detector.
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

The restart regression test reproduced `remote error: tls: bad certificate`
and a peer fingerprint mismatch on 0.1.0. With saved identities, a restarted
client keeps its fingerprint, accepts requests from a peer with the existing
cached announcement, transfers files, and renews its old discovery lease.
Additional tests cover identity locks, corrupt identities, and private-key
file permissions. GitHub Actions also runs the suite on Windows.

See README.md for current limits, setup, and the one-time 0.1.0 upgrade steps.
