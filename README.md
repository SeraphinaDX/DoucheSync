# DoucheSync

DoucheSync 0.3.1 synchronizes files in one or more folders directly between
machines. It is written in Go and configured with TOML.

One executable has two modes:

- **Server:** an authenticated, in-memory discovery directory. It receives only
  opaque folder identifiers, device IDs, client endpoints, certificate
  fingerprints, expiry times, and signatures. It has no file endpoints, file
  storage, file index, or relay capability.
- **Client:** scans local folders, announces itself, finds matching peers, and
  pulls changed files over authenticated, encrypted HTTPS connections. Every
  machine runs a client; clients both serve and receive files.

The server can run on a VPS while the clients run on your desktops and laptops.
Local folder paths can differ between machines. Matching folder **IDs and
secrets** determine which folders synchronize.

## What this release includes

- Multiple independent folders with separate shared secrets.
- Automatic local address selection, optional interface selection, and IP refresh.
- Optional NAT-PMP/UPnP TCP port mapping with signed LAN/WAN endpoint alternatives.
- Automatic bidirectional polling, offline edits, and persistent version history.
- TLS 1.3 peer connections with authenticated certificate pinning.
- SHA-256 verification, streamed downloads, and temporary files before replacement.
- Version vectors to detect concurrent edits without relying on modification times.
- Saved conflict copies and saved copies before remote replacements/deletions.
- Optional deletion propagation; it is disabled by default.
- Ignore patterns, per-folder file-size limits, orderly shutdown, and folder locks.
- Linux and Windows configuration paths and included service examples for Linux.
- Previously verified peer addresses remain usable during a discovery outage.

## Network requirement

**Clients need to reach one another's advertised HTTPS endpoints.** Use LAN
addresses on the same network, routable public addresses with forwarded TCP
ports, or VPN addresses such as those on a WireGuard network. Each client must
allow incoming TCP connections on its configured peer port (7444 by default).

Clients can request NAT-PMP or UPnP router port mappings automatically (see
below). The discovery server remains a directory: it never proxies or relays
file traffic. Routers must support and allow the mapping protocol, and the
client's OS firewall must allow its TCP peer port. CGNAT, double NAT, and routers
that refuse mappings can still require a VPN or manual forwarding. This release
does not implement STUN/ICE, UDP/TCP hole punching, PCP, or a relay fallback.

Only the discovery HTTPS port needs to be public on the VPS. File traffic goes
between the clients. The VPS does not need access to the client folders.

## Automatic addresses and NAT traversal

For ordinary LAN use, omit `advertise_url` or set it to `"auto"`. Keep a wildcard
listener so it can accept traffic after DHCP or network changes:

```toml
[client]
device_id = "desktop"       # keep your existing ID
listen = ":7444"            # no machine IP needed
advertise_url = "auto"      # optional: automatic is the default
nat_traversal = true        # optional; defaults to false
discovery_url = "https://sync.example.com"
discovery_token = "YOUR_EXISTING_DISCOVERY_TOKEN"
```

Keep your existing folder IDs, paths, secrets, and `.douchesync` history.
If `listen` previously contained a machine IP, replace it with `":7444"`.
Existing explicit `advertise_url` settings still work and override detection;
automatic NAT mapping requires automatic advertisement.

Automatic mode selects the local source IP used to reach discovery, preferring
IPv4 when available. It respects an explicitly bound listener address and uses
the actual listening port. If route selection is unavailable, it considers
active interfaces and prefers private IPv4 addresses. Ambiguous interfaces
produce an actionable error instead of guessing. For a selected LAN/VPN adapter,
add `advertise_interface = "YOUR_INTERFACE_NAME"`; its address is still automatic.
Router mapping prefers IPv4 because NAT-PMP and this UPnP implementation map IPv4.
Globally scoped IPv6 endpoints are supported; automatic link-local/scoped IPv6
advertisement is not supported.

Addresses are checked on each discovery heartbeat (normally every 20 seconds).
Wildcard listeners can continue running when the selected IP changes. A listener
bound to a specific IP needs reconfiguration/restart when that IP disappears.

With `nat_traversal = true`, the client finds the selected interface's IPv4
default gateway, tries NAT-PMP, then UPnP IGD WANIPConnection/WANPPPConnection.
It requests a TCP mapping for its actual peer port and learns the public address
and assigned external port from the router. NAT-PMP mappings request one-hour
leases; granted leases shorter than one minute are rejected. UPnP mappings request
one-hour leases, with a permanent-lease fallback for routers that require it.
Existing UPnP mappings belonging to another application are preserved; a different
external port is selected if needed. Mappings are refreshed alongside discovery
heartbeats at least once per minute and cleanup is attempted on clean shutdown.
Permanent mappings may remain after a crash or an unreachable-router/network change.

The client advertises **both** its LAN address and mapped public endpoint. Other
clients try the alternatives with the same signed certificate pin, remember a
working endpoint, and retry alternatives if it fails. LAN peers do not need router
hairpin NAT. Public endpoints that disappear are withdrawn while LAN discovery
continues. If mapping is unavailable, the client logs why and continues on the LAN.
It never substitutes a guessed public IP or sends file traffic through discovery.

To enable these features, **upgrade discovery and all participating clients to
0.2.0 first**. Older clients cannot decode announcements containing alternative
endpoints. Upgrade the server executable and restart its service; Caddy and its
configuration do not need changes. No sync-history reset is required.

The logs show the selected local address and, when successful:

```text
NAT traversal: public peer endpoint https://PUBLIC_IP:ASSIGNED_PORT
```

`DoucheSync diagnose` reports the registered LAN/WAN endpoints and tests each one.
It does not request or change router mappings. An unreachable LAN endpoint with a
working public endpoint is a successful peer diagnosis.

## Diagnose missing peers

Keep both clients running. In another terminal on each machine, run:

```sh
./DoucheSync diagnose
# If the client uses an explicit config, use the same file:
./DoucheSync diagnose -config=client.toml
```

This command only queries discovery and tests signed, certificate-pinned peer
manifest requests. It does not register a device, acquire client locks, scan,
download files, or change folders or saved identities. It can run alongside
the normal client. Its output does not include tokens, folder secrets, folder paths,
filenames, or manifest contents.

Compare **Discovery** and **Discovery group** on the two machines: both must
match. The group derives from the folder ID and secret; different groups mean
one or both of those settings differ. **Device** IDs must differ. Avoid changing
an existing device ID or deleting `.douchesync` history as a troubleshooting step.

- HTTP 401 from discovery means the token does not match the server's token.
- No own-device registration means the running client is not registered in this
  group; inspect its `discovery:` errors and confirm it uses the same config.
- An own registration with no other devices means discovery has no other verified
  client in that group. Check the second client's registration, config, and clocks.
- A discovered peer followed by a connection failure means the advertised endpoint
  is unreachable; check its address, running client, routing, and TCP firewall.
- A fingerprint mismatch means the endpoint served a different certificate from
  its signed announcement. Check the address and device ID and use saved identities.
- An `OK` peer result confirms pinned TLS and authentication succeeded from this
  machine. Run the check in both directions.

The normal `no matching peers discovered yet` message describes an empty peer
list. It is not a failed reachability test. The discovery server does not need
an upgrade for this diagnostic command.

Normal client logs now also show peer activity, including empty folders:

```text
[test-folder] discovered peer laptop at https://10.0.0.63:7444
[test-folder] peer laptop reachable at https://10.0.0.63:7444; sync check complete
```

Discovery is logged when a peer first appears or its endpoint/identity changes.
A completed sync check is logged on the first success and after recovery from
a failed check. File receipts, deletions, conflicts, and errors are logged as
they happen. Removed discovery announcements are logged as well; an abruptly
stopped client's advertisement may remain until its lease expires.

Checks and transfers use short HTTPS requests, rather than permanently open
peer connections. An unchanged, healthy peer does not print the same success
every scan; quiet output after a completed check is normal.

## Build from source

Install **Go 1.25 or newer**, using a current patch release:

```sh
git clone https://github.com/SeraphinaDX/DoucheSync.git
cd DoucheSync
CGO_ENABLED=0 go build -trimpath -o DoucheSync .
./DoucheSync version
```

Or run `make build` from the checkout. There is no cgo requirement. The only
Go module dependency is the BurntSushi TOML parser. Its source and MIT license
are vendored, so building from a complete checkout does not download modules.

On Windows, use PowerShell:

```powershell
git clone https://github.com/SeraphinaDX/DoucheSync.git
cd DoucheSync
$env:CGO_ENABLED = "0"
go build -trimpath -o DoucheSync.exe .
.\DoucheSync.exe version
```

Use `.\DoucheSync.exe` in place of `./DoucheSync` in the examples below.
The source supports Linux, Windows, and macOS. Local validation was performed
on Linux; Windows and macOS builds were checked by cross-compilation.

## Quick start: two machines and a VPS

### 1. Generate three different keys

```sh
./DoucheSync keygen
./DoucheSync keygen
./DoucheSync keygen
```

Each command prints a new 256-bit random key. Use one as the **discovery
token**, one as the **documents secret**, and one as the **photos secret**.
Share those keys privately among the appropriate machines. Never reuse the
discovery token as a folder secret.

### 2. Configure discovery on the VPS

Copy `examples/server.toml` to `server.toml` and replace its token placeholder
with the discovery token. Start it with:

```sh
./DoucheSync check-config -config=server.toml -mode=server
./DoucheSync server -config=server.toml
```

By default it listens only on `127.0.0.1:7443`. Put an HTTPS reverse proxy in
front of it. `examples/Caddyfile` contains a minimal example: replace
`sync.example.com` with your hostname and route it to the loopback listener.

Alternatively, supply `tls_cert` and `tls_key` in `[server]` and set `listen`
to `":7443"` to serve HTTPS directly. Those must be an existing certificate
and key, and the certificate must be trusted by your clients. A public server
without TLS is rejected. Plain HTTP mode accepts a loopback listener only.

**Folder secrets belong only on clients. The server needs only the discovery
token.** It never receives folder secrets or filenames.

### 3. Configure the desktop

Create the directories before starting, so a missing disk or mount cannot
silently become a new, empty sync root:

```sh
mkdir -p ~/Documents/Shared ~/Pictures/Shared
cp examples/client-desktop.toml client.toml
```

Edit `client.toml`:

- Leave `advertise_url` automatic, or set an explicit reachable LAN/VPN/public URL.
- Optionally enable `nat_traversal = true` for compatible routers.
- Set `discovery_url` to your HTTPS discovery hostname.
- Set `discovery_token` to the server's token.
- Replace the documents and photos secret placeholders with their distinct keys.
- Set the local paths to the directories you actually want to share.

Then run:

```sh
./DoucheSync check-config -config=client.toml
./DoucheSync client -config=client.toml
```

All commands use explicit `-config=...` syntax. Put options after `client`,
`server`, or `check-config`.

### 4. Configure the laptop

Use `examples/client-laptop.toml`. It uses a different device ID and automatically
selects its address, with the **same discovery token, folder IDs, and folder secrets**.
Create its local folders and start its client using the same command.

Add a file to either shared folder. It should appear on the other machine
after discovery and scanning. New peers are discovered every 20 seconds;
scans normally run every 10 seconds. A long scan or transfer can lengthen the
time between scans, while discovery heartbeats continue independently.

### 5. Add more machines or folders

Give every machine a **unique, stable** `device_id`, and a reachable endpoint.
Add another `[[folders]]` section to each participating client's TOML with a
matching `id` and `secret`. A machine can share only some of the folders.
Folder roots must exist and must not overlap or contain one another.

## TOML reference

```toml
[client]
device_id = "desktop"                     # unique and stable on this machine
listen = ":7444"                          # local incoming peer listener
advertise_url = "auto"                    # optional; auto is the default
# advertise_interface = "enp3s0"          # optional preferred adapter
nat_traversal = false                     # true enables NAT-PMP/UPnP mapping
discovery_url = "https://sync.example.com"
discovery_token = "YOUR_RANDOM_DISCOVERY_KEY"
scan_interval = "10s"                     # 1s through 24h
transfer_timeout = "30m"                  # 1s through 24h
parallel_transfers = 4                    # simultaneous downloads; 1..32
allow_http_discovery = false

[[folders]]
id = "documents"                         # match across participating clients
path = "~/Documents/Shared"               # local path; may differ by machine
secret = "YOUR_DISTINCT_RANDOM_FOLDER_KEY"
sync_deletes = false
ignore = ["*.tmp", "*.swp", ".git", "node_modules"]
max_file_size = 10737418240               # bytes; default 10 GiB
```

Clients download up to four files simultaneously by default. Set
`parallel_transfers` under `[client]` to tune this (1..32); use 1 for sequential
transfers. Peer TLS connections are reused within each sync check. Folders and
peers are checked in sequence, so the worker count is a client-wide download
limit during normal operation. Incoming transfers have separate connections.
Downloads stream to disk and are hash-verified; installing files and saving
history remain serialized. Increasing the setting helps when per-file network
latency is the bottleneck, but cannot exceed your network or disk throughput.

Scans still hash all files, including unchanged files, and changed files are
transferred in full. This release does not implement filesystem watching,
block-level transfers, or parallel chunks of a single large file. Initial
scan time and disk/history writes can still limit throughput.

Version 0.3.1 is a client performance update. Stop the clients, run
`git pull --ff-only` and `make build`, then restart them. Existing configurations
get four workers without edits; keep your IDs, secrets, identity files, and
`.douchesync` history. A 0.2.0 discovery server remains compatible.

Keys must contain at least 32 characters. Generate real keys with `keygen`;
placeholder strings are not secure keys. Unknown TOML settings are rejected
to catch spelling mistakes.

Paths starting with `~/` expand to your home directory. Other relative paths
resolve from the process's working directory; absolute paths are recommended
for services. Environment variables in TOML values are not expanded.

Ignore patterns use Go's `filepath.Match` glob syntax, tested against a full
relative path and each component/basename. An ignored directory excludes its
descendants. `**` recursive glob syntax is not supported. `.douchesync` is
always excluded. Keep ignore rules consistent between peers for predictable
results.

Default config locations when `-config` is omitted:

| Platform | Default path |
| --- | --- |
| Linux | `$XDG_CONFIG_HOME/douchesync/config.toml`, normally `~/.config/douchesync/config.toml` |
| Windows | `%AppData%\douchesync\config.toml` |
| macOS | `~/Library/Application Support/douchesync/config.toml` |

## Deletions, conflicts, and recovery

With `sync_deletes = false`, removing a file locally does not delete other
machines' copies. The missing file is restored when a peer still has it.
Set `sync_deletes = true` **on every client sharing that folder** to propagate
deletions. Deletions only apply to files that were previously tracked; an
initially empty folder never means "delete everything on the other machine."

Since 0.3.1, each peer's deletions are applied before downloading its files,
so a slow download does not hold up that peer's deletion batch. Deletion
history is saved as small atomic records, then checkpointed after every 128
updates and at the end of the deletion phase. Pending records are replayed
on startup, preserving version clocks after an interrupted batch. Recovery
copies are still byte-verified and retained before removing a local file.

Local deletes are discovered by the normal scan, so the delay still includes
`scan_interval` (10 seconds by default) and scan time. Large files also take
time to retain as recovery copies. Increasing `parallel_transfers` does not
speed up that serialized disk work. The default `sync_deletes = false` restores
missing files instead of propagating deletion; enable it on every client for
each folder where deletions should sync.


When a file is edited independently on two machines, version vectors detect
the conflict. The live version with the lexicographically greater SHA-256 hash
becomes the canonical copy, so peers converge without trusting their clocks.
The other version is saved. A concurrent edit survives a concurrent deletion.
This is a deterministic rule, not a preference for whichever edit is newer.

Each shared root has a **local-only** `.douchesync` directory:

- `state.json` and `identity`: persistent folder and version history.
- `conflicts/`: saved losing versions from resolved concurrent edits.
- `versions/`: saved local contents before remote replacement or deletion.
- `transfers/`: incomplete downloads and temporary archive/history files.
- `updates/`: pending history records, replayed on startup and cleared after a checkpoint.
- `lock`: an OS file lock that prevents two processes from using the same root.

Saved versions use `<path-sha256>-<content-sha256>` filenames; the adjacent
`.json` file records the original relative path and version metadata. To
restore one, stop this client, copy the saved byte file (not its JSON sidecar)
to the recorded path, and restart. It becomes a new local edit and syncs.

Archives are local and have **no automatic retention limit** in this release.
You may remove old files from `conflicts/` and `versions/` after reviewing
them while the client is stopped. Deleting the whole `.douchesync` directory
resets history and can cause old files from offline clients to reappear.
Do not reset it as routine cleanup. Do not copy it from one machine to another,
and do not change device IDs or folder secrets on initialized roots without a
planned history reset. Missing or mismatched history causes startup to stop.

Peer certificates and private keys are generated automatically and saved to
`identity/<device_id>.pem` inside the user configuration directory, normally
`~/.config/douchesync/identity/` on Linux. An optional `[client] identity_dir`
setting changes that location. Keep the saved identity on its original
machine; do not share or synchronize it. Unix private-key files use mode 0600.

Normal restarts reuse the same certificate, so cached peers and discovery
leases remain valid. A damaged saved identity stops startup rather than
silently replacing the certificate. An OS lock prevents two local clients
from using the same saved device identity at once.

When upgrading from 0.1.0 to 0.1.1, stop the old clients first, rebuild both,
and restart discovery once to clear the old in-memory certificates. The new
clients create their saved identities automatically. Folder configuration and
`.douchesync` history need no changes. Deliberately replacing an identity may
also require discovery to forget its old lease (normally 90 seconds).

If you bootstrap folders using another copy tool, **exclude `.douchesync`**.
The folder lock is released automatically even if the process crashes; do not
remove the lock file to bypass a running client.

## Security model

- Anyone with a folder secret is a trusted member of that folder and can
  receive files or introduce changes. There is no per-device approval UI.
- Use private, strong secrets and protect config files, for example with
  `chmod 600 client.toml server.toml` on Linux.
- Peer advertisements are signed with the folder secret. Clients check both
  the signature and the peer's certificate fingerprint. Peer certificates
  are generated automatically on first startup and saved for reuse; no CA
  setup or manually supplied certificates are needed for peers.
- LAN and WAN endpoint alternatives are covered by that signature and use the
  same certificate pin. UPnP descriptions/control URLs are restricted to the
  selected default gateway; redirects and off-router URLs are rejected.
- Peer requests are authenticated with HMAC, timestamped, and replay-checked.
  Keep machine clocks reasonably aligned (within about one minute).
- Files are SHA-256 checked before installation. Unsafe paths and symlinks
  are rejected. Go's rooted filesystem API confines file access to a root.
- The server sees device/network metadata and opaque room identifiers. Use
  HTTPS for discovery to protect its bearer token and peer registry in transit.
- This is early software with automated tests, not an independently audited
  synchronization system. Start with a backed-up test folder.

## Linux services

For a client running under your user account:

```sh
mkdir -p ~/.local/bin ~/.config/douchesync ~/.config/systemd/user
cp DoucheSync ~/.local/bin/
cp client.toml ~/.config/douchesync/config.toml
chmod 600 ~/.config/douchesync/config.toml
cp examples/douchesync-client.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now douchesync-client.service
journalctl --user -u douchesync-client.service -f
```

To keep the user service running after logout, enable lingering for your user
if appropriate for that machine: `sudo loginctl enable-linger "$USER"`.

For the VPS discovery service, `examples/douchesync-server.service` expects
a `douchesync` system user, `/usr/local/bin/DoucheSync`, and
`/etc/douchesync/server.toml`. Install those and use:

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin douchesync
sudo install -m 755 DoucheSync /usr/local/bin/DoucheSync
sudo install -d -m 750 -o root -g douchesync /etc/douchesync
sudo install -m 640 -o root -g douchesync server.toml /etc/douchesync/server.toml
sudo cp examples/douchesync-server.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now douchesync-server.service
```

If that user already exists, skip `useradd`. Set up the HTTPS reverse proxy
separately. Discovery requires no writable data directory.

## Local testing without a proxy

Run the discovery server on `127.0.0.1:7443`. On the same machine, set each
client's discovery URL to `http://127.0.0.1:7443` and
`allow_http_discovery = true`. Give the clients different roots, device IDs,
and listening ports, e.g. 7444 and 7445, with their corresponding
`https://127.0.0.1:PORT` advertised URLs. Peer transfers are still encrypted.

`allow_http_discovery` also permits an HTTP reverse proxy on a trusted LAN,
but sending the discovery token over unencrypted public networks is unsafe.

```sh
go test -buildvcs=false ./...
go test -race -buildvcs=false ./...
go vet -buildvcs=false ./...
go build -trimpath -o DoucheSync .
python3 scripts/smoke.py ./DoucheSync
```

GitHub Actions runs tests and builds on Linux and Windows, plus Linux race,
vet, and process checks. The process smoke test runs one discovery server and three real clients in
temporary directories, checks multi-peer convergence, updates, deletions, and
server filesystem isolation, then shuts down the processes.

For one scan and pull cycle:

```sh
./DoucheSync client -config=client.toml -once
```

Use a continuous client on the other machine. `-once` announces, pulls, and
exits; it does not wait for all peers to discover or pull from it. No peers
found is logged and is not an error. Discovery or transfer errors make
`-once` exit nonzero; continuous mode logs errors and retries next cycle.

## Current limits

This initial release synchronizes **regular file contents** and their nested
paths. It creates parent directories for received files; empty directories
are not exchanged, and empty directories left after deletions are not removed.
Symlinks, devices, ACLs, extended attributes, ownership, hard-link relationships,
and permission-only changes are not synchronized. File permissions and mtime
are applied when receiving changed content. File-to-directory type changes
require manual reconciliation.

Files transfer whole rather than as deltas, and interrupted downloads restart.
Scans hash every file to avoid relying solely on timestamps. Large trees may
use noticeable disk I/O. A folder has a 64 MiB manifest/state limit and up to
100,000 tracked paths (including retained missing-file and deletion history),
128 device counters per file, and a default 10 GiB individual-file limit.
The discovery server defaults to 1,000 registered folder/device pairs.

Filenames must be relative, portable names: Windows device names, backslashes,
colons, control characters, trailing dots/spaces, and reserved characters are
excluded. Keep names unique when compared without case on mixed-OS shares.
Files that exceed `max_file_size`, unreadable files, and tracked paths that
become symlinks or directories stop that folder's scan rather than generating
false deletion records. Active edits during a transfer are checked and
deferred, but applications that write simultaneously with final file
replacement cannot be made transactionally safe across processes; pause
editors or databases when you need a consistent snapshot.

There is no transactional multi-file snapshot or tombstone compaction yet.
Archives protect remote replacements; they do not capture every local editor
save. Use ordinary backups alongside synchronization.

## License

GPL-3.0-or-later. See `LICENSE`. The vendored TOML parser uses its own MIT
license, included under `vendor/github.com/BurntSushi/toml/`.
