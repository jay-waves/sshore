# SFTP mount implementation

## Path selection and startup

- `HOST:` uses the remote home and `:` allocates a local mountpoint. Use `--lf` to browse
  omitted paths and Enter to select a directory. Explicit `HOST:~` and `.:` use the defaults
  even with `--lf`.
- Windows drive-letter mounts use the last component of the resolved remote directory.
  For example, `/srv/projects/work` mounts as `\\sshore\work`. Share names use lowercase.
  Concurrent mounts with the same name use the first free name from `work`, `work-2`,
  `work-3`, and so on.
- Direct mounts authenticate before opening a picker. After authentication the
  worker can warm the home listing, and can reply with the current and parent
  listings in one startup message. The selected root is resolved and validated
  through the existing SFTP client before mounting.
- Mounts run in the background. The CLI owns the picker, the account daemon owns
  the task, and one isolated worker owns the SSH connection and native mount.
  The daemon relays picker messages between the CLI and worker.
  The credential broker handles SSH prompts. The worker reports readiness and
  the CLI commits startup with the daemon. Losing the CLI before commit cancels
  startup, including during authentication or browsing.
- Use `sshore kill TASK` to unmount. A busy unmount can be retried after files are
  closed. If the worker stops responding during shutdown, the daemon terminates
  that worker.
- Direct mounts start one SSH exec channel. An `..:` request first goes through
  [local approval](approval.md).
  The approved route binds the remote directory and access mode to the original
  detached shell. The resulting resource owns its connection and can outlive that shell.
  Both routes carry data and notifications in separate `StreamMux` streams. The
  data handshake validates the version and approval before starting SFTP.

See [mount startup and path selection](../cmd/sshore/mount.go),
[mount lifecycle](../internal/files/mount.go) and [SSH topology](network.md).

## Directory and metadata caches

- The SFTP client used for browsing also backs the mount. Its cache is scoped to
  the authenticated connection. Directory listings are immutable snapshots.
  Concurrent reads of the same key share a request, and open directory handles
  advance through their own snapshot.
- Current lifetimes are five seconds for directory listings, one second for
  file metadata and two seconds for capacity results. The cache has entry and
  byte budgets. Wire limits on directory replies apply independently of cache
  admission. An oversized reply fails rather than returning a partial listing.
- Directory reads can seed short-lived metadata for regular entries. A bounded
  prefetch follows likely child directories using visit history. Recent cached
  directories can be checked in the background. The worker loads visit history
  from bbolt at mount startup and merges its visits in one transaction at unmount.
  Directory listings and file metadata live in the connection cache. Each
  operation checks permissions and file identity.
- The picker reuses recent lists and debounces right-column previews. It follows
  the current and parent directories without recursively scanning the remote tree.

See [cache keys and budgets](../internal/files/mount_metadata_cache.go),
[prefetch and refresh](../internal/files/mount_refresh.go) and
[SFTP response limits](../internal/files/sftp.go).

## Request scheduling during bulk transfers

Directory browsing and small file reads share the SFTP data stream with bulk transfers.

Each mount reserves eight outstanding requests for metadata operations and reads
of up to 4 KiB. Larger reads and writes share a separate 16-request window.
When both queues have a free slot and a request ready, the sender alternates
between them. Requests in each queue are sent in arrival order. A bulk transfer
may occupy its entire window while directory and attribute queries still have
room to send. Sustained browsing also leaves send opportunities for bulk I/O.

The scheduler bounds how many bulk requests can get ahead on the data stream.
Already-sent packets can still delay a response. A large directory listing,
network stalls, and slow remote storage can also affect response times. The 4 KiB
cutoff applies to the SFTP read request size. A filesystem may request more bytes
than the application reads.

By default, a request waiting for a send slot can time out after 60 seconds. A
request that times out in this queue is removed before dispatch. Once dispatch
reaches the SFTP transport, the request has its own timeout. A transport timeout
closes the mount connection, and writes are never replayed. See
[`sshore mount --help`](../cmd/sshore/cli_schema.go) for timeout settings and
[SFTP mount safeguards](SECURITY.md#sftp-mounts) for failure semantics.

## Benchmarks

Run on Windows or Linux with the repository's Go toolchain:

```sh
go run ./scripts go test ./tests/benchmarks -run '^$' -bench '^BenchmarkMountResponsiveness$' -benchtime=20x
```

The benchmark uses the real SFTP client and mount agent through `StreamMux` on
a local TCP connection. Thirty-two workers issue 128 KiB reads while the
foreground lists a directory, opens and reads a short file, or waits for a
directory watch acknowledgment on the second stream. The carrier pauses for
2 ms per 64 KiB of bulk frames to keep the connection busy. Both modes use the
same carrier. The shared request window provides the unscheduled baseline and
the reserved request windows use the mount scheduler. The output reports
foreground P95 latency in milliseconds and the carrier's rate for large frames
in MiB/s.

Use these figures to compare changes under the same load. End-to-end
filesystem and SSH performance also depends on the platform, caching, network,
server, and workload.

The SFTP guard inspects responses in the decoder's buffer. On writes, the
transport receives the header and original file payload as separate slices.
Request accounting and directory response budgets apply on both paths.
Measure the cost of processing a 64 KiB frame in memory with:

```sh
go run ./scripts go test ./tests/benchmarks -run '^$' -bench '^BenchmarkMountWireDataFrame$'
```

## Change notification and teardown

- The remote agent watches requested directories on the notification stream and
  coalesces changes. Notifications invalidate cached paths and metadata. If
  notifications are unavailable or the stream fails, TTL expiry and bounded
  background refresh continue to provide updates.
- Local mutations invalidate affected paths and parent listings. A rename
  includes both endpoints. Aliases and existing open snapshots can retain
  older observations. A notification remains advisory and grants no authority.
- An in-flight SFTP request timeout closes the transport.
  The optional startup timeout covers authentication, selection and native mount creation.
  The remote result of a timed-out write can be unknown, so writes are never
  replayed. Graceful unmount attempts to release the filesystem while the
  transport is live. A busy mount stays connected for retry.

See [notification client](../internal/files/mount_notify.go),
[remote watcher](../internal/files/mount_agent.go), [security limits](SECURITY.md#sftp-mounts)
and [OpenSSH mount examples](../tests/integration/openssh_linux_test.go).

When changing startup, verify that an abandoned worker releases its connection and any
mountpoint. When changing caching, exercise both an external update and a local rename.
`mount --stats` reports wire requests and cache counters for checking the cost of a change.
See the [SFTP benchmarks](../tests/benchmarks/sftp_test.go).
