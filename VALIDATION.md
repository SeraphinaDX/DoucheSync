# Validation for DoucheSync 0.3.0

Built on Linux x86-64 with Go 1.27.1 on 2026-10-07.

- All 47 automated Go tests pass locally on Linux, including the race detector.
- `go vet -buildvcs=false ./...` passes.
- Three separate client processes and one discovery server pass the process smoke test.
- Initial files, remote updates, and enabled deletions converge across all three clients.
- Discovery writes no files to its working directory.
- All processes shut down cleanly on SIGTERM.
- Linux x86-64 binary built with CGO_ENABLED=0 and exercised in the process test.
- Windows x86-64 binary built with CGO_ENABLED=0 by cross-compilation; Native Linux and Windows CI also passed for the preceding 0.2.0 release; the updated suite runs on both platforms in GitHub Actions.

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

Diagnostic tests distinguish empty groups and bad discovery tokens, verify
authenticated peer reachability, retain certificate pin checks, and reject
untrusted announcements. A diagnostic alongside running clients leaves their
folder files, identities, and registration leases unchanged and prints no secrets.

Network tests use local UDP/HTTP router simulators. They exercise NAT-PMP public
address queries, TCP mapping packets, assigned ports, renewal, port-specific
deletion, private/CGNAT WAN rejection, SSDP, UPnP device descriptions, SOAP mapping
creation/renewal/deletion, permanent-only routers, occupied ports, ownership checks,
and unsafe/redirected router URL rejection. Manager tests cover retry intervals,
network changes, and cleanup. Other tests cover automatic addresses and allocated
ports, LAN sync with WAN advertisements, failed-WAN withdrawal, signed alternative
endpoint tampering, pinned endpoint fallback, and cached endpoint preference.

Actual consumer router hardware, CGNAT hole punching, and PCP have not been tested
or implemented. NAT traversal in this release means NAT-PMP/UPnP router mapping;
it cannot guarantee connectivity through every network. macOS gateway parsing is
tested with fixtures; macOS runtime behavior has not been tested.

See README.md for current limits, setup, and upgrade steps.

Parallel-transfer tests check actual overlapping requests with one and three
workers, enforce the configured bound, cancel in-flight downloads without
installing partial files or retaining temporary transfers, and isolate corrupt
downloads from successful files and edits made during a concurrent transfer.
A sequential eight-file batch reuses the manifest TLS connection for all eight
file requests. TOML tests cover defaults and worker bounds.

`go test -buildvcs=false -run '^$' -bench BenchmarkSmallFileBatch -benchtime=3x`
measured 819.4 ms/batch with one worker and 210.5 ms/batch with four, approximately
3.9 times faster. Each batch contains sixteen seven-byte files, with an artificial
50 ms wait before each file response, including hash verification and durable
file/history writes. Both modes use connection reuse. This demonstrates reduced
per-file latency, not an expected speedup for all networks or large files, and
is not a benchmark against another sync product.
