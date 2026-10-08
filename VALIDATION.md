# Validation for DoucheSync 0.4.0

Built on Linux x86-64 with Go 1.27.1 on 2026-10-08.

- All 71 automated Go tests pass locally on Linux, including the race detector.
- `go vet -buildvcs=false ./...` passes.
- Three separate client processes and one discovery server pass the process smoke test.
- Initial files, remote updates, enabled deletions, and new nested folders converge across all three clients.
- Local edits, deletions, and new subtrees are indexed within 3 seconds with a 10-second polling interval.
- Discovery writes no files to its working directory.
- All processes shut down cleanly on SIGTERM.
- Linux x86-64 binary built with CGO_ENABLED=0 and exercised in the process test.
- Windows x86-64 binary built with CGO_ENABLED=0 by cross-compilation. Native Linux and Windows CI passed for 0.3.2; the updated suite runs on both platforms in GitHub Actions.

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

Deletion tests cover prioritization ahead of a stalled download, checkpoints
before downloads, replay after an interrupted deletion, retained recovery
contents, exact recovered clocks, periodic checkpoints across a 136-file batch,
stale records left after a completed checkpoint, malformed or wrong-identity
records, conflicting clocks, and unavailable history storage. Existing tests
continue to verify deletion disabled and concurrent edit/delete behavior.

`go test -buildvcs=false -run '^$' -bench BenchmarkDeleteHistory -benchtime=3x`
measured 1.461 s for 128 full snapshot writes versus 26.99 ms for 128 small
history records plus one checkpoint, in a 10,000-path history. This isolates
history persistence and excludes scanning, network latency and copying retained
file contents. Both modes fsync each mutation and produce the same final state;
the improvement is not a claim that total deletion time improves by that factor.

Actual parallel-deletion tests observe overlapping recovery temporary files,
verify one/three worker bounds, confirm the folder lock remains available during
copying, preserve edits made while copying, check cancellation cleanup and
unchanged sources, and validate `parallel_deletes` TOML defaults and bounds.
Copying and all complete content hashes for deletion now occur outside the
shared folder lock; the final identity/stat check, removal and history commit
hold the lock. Existing interrupted-batch and checkpoint replay tests still pass.

`go test -buildvcs=false -run '^$' -bench BenchmarkParallelDeleteBatch -benchtime=3x`
measured 149.8 ms with one worker and 45.90 ms with four, approximately 3.3 times
faster on this local filesystem. Each batch deletes sixteen 4 MiB files, retaining
and verifying recovery copies, fsyncing file/history writes and checkpointing the
batch. The TLS manifest fetch is included; fixture setup and initial scanning are
excluded. This measures the actual deletion phase, rather than history persistence
alone. Storage hardware and competing IO will affect the result.

Idle-work tests verify that repeated unchanged scans read zero content bytes and
keep the existing checkpoint file, and that settled peer cycles do not hash
unchanged files again. Cache tests cover same-size edits with restored modification
times, atomic replacements with matching size/time, deletions and cache pruning,
forced and aged verification, empty caches after restart, and interval validation.
Usable operating-system change timestamps are required for cache reuse; those
specific checks skip on unsupported filesystems while full hashing remains enabled.

Native fsnotify tests cover idle scan skipping, ignored/private history writes,
new nested directories, atomic replacements, deletions, unnotified writes recovered
by safety rescans, and polling fallback after queue overflow. The process test
exercises watcher scheduling and clean shutdown with three real clients.

`go test -buildvcs=false -run '^$' -bench BenchmarkIdleScan -benchtime=3x`
measured 44.12 ms for full verification of sixteen 4 MiB files, 44.82 microseconds
for an incremental safety scan, and 0.404 microseconds for a quiet watcher check.
The latter two read zero file content bytes; full verification reads 64 MiB.
Initial scanning, watcher setup and transfers are excluded from these timings.
The watcher check measures the local scan decision, not a complete peer cycle.

A separate real-process comparison ran two clients and one discovery server for
about 14.3 seconds per version, with 64 MiB of unchanged files on each client and
a deliberately short `scan_interval = "1s"`. Aggregate child-process CPU time
(measured using Python `resource.getrusage(RUSAGE_CHILDREN)` after shutdown) fell
from 1.968 seconds on 0.3.2 to 0.168 seconds on 0.4.0: about 13.73% versus 1.18%
of one CPU over the full run. This includes startup verification and discovery;
it is not a steady-state per-client measurement or a promise about a particular
laptop. No contents changed during either run. Normal default polling is 10 seconds.
