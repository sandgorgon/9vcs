# Changelog

All notable changes to 9vcs are documented here. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/); versioning is
[semver](https://semver.org/), with the caveat described in the
README's [Versioning and compatibility](README.md#versioning-and-compatibility)
section — the on-disk patch/bundle format makes no compatibility
promise between pre-`1.0.0` releases.

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
