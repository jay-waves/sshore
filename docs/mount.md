# SFTP mount implementation

## Path selection and startup

- `HOST:` starts browsing at the remote home. `HOST:~` selects home directly.
  `here:` opens the local mountpoint picker, and `.:` requests automatic allocation.
  Explicit paths and automatic allocation skip the corresponding picker.
- Direct mounts authenticate before opening a picker. After authentication the
  worker can warm the home listing, and can reply with the current and parent
  listings in one startup message. The selected root is resolved and validated
  through the existing SFTP client before mounting.
- In a background mount the frontend owns the picker, the account daemon owns the
  registered task and relays startup messages, and the isolated worker owns the
  SSH connection and native mount. The credential broker handles SSH prompts.
  After path selection, the worker reports readiness and the frontend commits
  startup. A pending task can be cancelled at any stage, including authentication
  or browsing. Loss of the initiator before commit abandons startup.
- Direct mounts use sibling data and optional notification channels on one SSH
  connection. An `origin:` request first goes through [local approval](approval.md).
  The approved route binds the remote directory and access mode to the original
  detached shell. The resulting resource owns its connection and can outlive that shell.
  Approved mounts carry data and notifications in separate smux streams.

See [mount startup](../mount.go), [path selection](../mount_selection.go),
[mount lifecycle](../internal/files/mount.go) and [SSH topology](NETWORK.md).

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
  directories can be checked in the background. History and cache entries
  improve latency. Permissions and file identity are checked by the operation.
- The picker reuses recent lists and debounces right-column previews. It follows
  the current and parent directories without recursively scanning the remote tree.

See [cache keys and budgets](../internal/files/mount_metadata_cache.go),
[prefetch and refresh](../internal/files/mount_refresh.go) and
[SFTP response limits](../internal/files/sftp.go).

## Change notification and teardown

- The remote agent watches requested directories on the notification stream and
  coalesces changes. Notifications invalidate cached paths and metadata. If
  notifications are unavailable or the stream fails, TTL expiry and bounded
  background refresh continue to provide updates.
- Local mutations invalidate affected paths and parent listings. A rename
  includes both endpoints. Aliases and existing open snapshots can retain
  older observations. A notification remains advisory and grants no authority.
- When enabled, the I/O timeout applies per SFTP request. A timeout closes the transport.
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
