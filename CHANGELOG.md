# Changelog

All notable changes to 9vcs are documented here. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/); versioning is
[semver](https://semver.org/), with the caveat described in the
README's [Versioning and compatibility](README.md#versioning-and-compatibility)
section — the on-disk patch/bundle format makes no compatibility
promise between pre-`1.0.0` releases.

## [0.1.2] - 2026-08-25

### Added

- A release workflow (`.github/workflows/release.yml`) that, on each
  `v*` tag push, cross-compiles `cmd/9vcs` for linux/amd64,
  linux/arm64, darwin/arm64, and darwin/amd64, and publishes each as a
  `.tar.gz` (binary + `LICENSE` + `README.md`) with a `.sha256`
  checksum to the GitHub Release — so installing no longer requires a
  Go toolchain. Documented in the README's new "Install" section.

## [0.1.1] - 2026-08-25

Follow-ups from actually dogfooding v0.1.0 (a real two-person-plus
workflow: `serve`/`import`/`reconcile` including genuine divergence
and conflict resolution, `bundle export`/`import`, `offer`/`offer
apply`). No functional or security issues found — these are all
polish/documentation gaps.

### Fixed

- `9vcs help`'s `status` line still advertised the removed rename
  detection's `R`/`R+` codes; corrected to `(A/M/D/U)`.
- Text conflicts (the most common kind) fell through to a generic
  `CONFLICT: path` message in `merge`/`apply`'s output, unlike
  binary/symlink/type/modify-delete conflicts, which all got a
  specific, actionable one. Added an explicit message pointing at the
  `<<<<<<<`/`=======`/`>>>>>>>` markers.

### Documentation

- Clarified that `serve` only reads `.9vcs/authorized-peers` once, at
  startup — editing it doesn't revoke or grant access on an
  already-running server the way "immediately" implied.
- Documented that running `offer list`/`apply`/`remove` against your
  own `serve` requires your own fingerprint to also be listed in your
  own `authorized-peers` — otherwise the connection is refused.
- Documented the "a network push refuses to move the branch checked
  out on the serving machine" behavior in the team workflow section,
  with the actual recommended pattern (keep a non-shared branch
  checked out where you serve from).

## [0.1.0] - 2026-08-25

First tagged release.

### Added

- Full local operation: `init`, `record`, `log`, `branch`, `diff`,
  `checkout`, `merge`, `status` — real patch-graph conflict detection
  (line-level forks, binary/symlink conflicts, modify/delete races),
  not a three-way text diff.
- `apply`: N-way merge of multiple patches/branches in a single step.
- Networking: `9vcs serve` (a 9P2000 server over TLS 1.3 with pinned-
  fingerprint peer authentication), `import` (one-way pull), and
  `reconcile` (bidirectional pull/push), each gated by per-peer
  `read`/`propose`/`write` permissions.
- Offline change exchange: `bundle export`/`import`/`show` (signed,
  single-file patch bundles) and `offer`/`offer list`/`offer apply`
  (submit a change without write access, via a peer's `/offers`
  mailbox).
- Per-patch authorship: optional Ed25519 signing
  (`AuthorFingerprint`/`AuthorSignature`), verified independently of
  transport-level trust — a relay can't forge authorship of a patch it
  merely passes along.
- `.9vcsignore` support, executable-bit and symlink tracking.
- Client-side known-peers store with trust-on-first-use semantics for
  `import`/`reconcile`.

### Security

A round of audits ahead of this release found and fixed several real
issues: path traversal (via patch file paths, ref names, and
symlinks), a couple of unbounded-allocation/CPU-exhaustion vectors
reachable over the network, a local ref-write race, and a
merge-conflict-detection gap that could silently drop one side's
content instead of reporting a conflict. See [PLAN.md](PLAN.md) for
the full writeup of each.

### Known limitations

- iOS and Windows builds are unverified — untested on either platform
  (see PLAN.md's "Open items to revisit").
- No format-compatibility promise between `v0.x` releases — see
  [Versioning and compatibility](README.md#versioning-and-compatibility).
