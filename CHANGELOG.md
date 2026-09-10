# Changelog

All notable changes to 9vcs are documented here. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/); versioning is
[semver](https://semver.org/), with the caveat described in the
README's [Versioning and compatibility](README.md#versioning-and-compatibility)
section — the on-disk patch/bundle format makes no compatibility
promise between pre-`1.0.0` releases.

## [0.1.7] - 2026-09-09

### Added

- A `-C <path>` flag, checked before every command resolves its repo:
  under a [9sh](https://github.com/sandgorgon/9sh) session
  (`$_9SH_UNIX_SOCK` set), `<path>` is tried against that shell's own
  namespace first — a relative path rooted at `local` (9sh's real
  launch directory), an absolute one as given, which may resolve to
  anything else 9sh has bound — before falling back to a literal OS
  path exactly like the no-flag case (`repo.Find()` itself is
  untouched). Storage and working-tree materialization both work fully
  over this path, symlinks included — see PLAN.md decision #9 for the
  full design, including one documented residual limitation (a
  namespace-resolved write is confined to wherever 9sh bound the
  region, not the specific repo within it).

### Changed

- Bumped `github.com/sandgorgon/9p` to v0.9.0. Two real gaps surfaced
  while building the above, both filed and fixed upstream:
  `client.File` couldn't rename or remove a file it held (blocking a
  safe atomic ref/HEAD write or lock release over 9P), and
  `examples/dirfs`'s path confinement didn't defend against a symlink
  planted at an intermediate path component — the same live bug class
  this project's own working-tree writes were already fixed for. A
  third gap, discovered next — base 9P2000 has no symlink
  representation at all — landed as optional 9P2000.u support.
- `objstore/patches` and `repo.Repo`'s ref/HEAD/lock storage now go
  through a small internal `fsx.FS` seam instead of calling `os`/
  `filepath` directly (no behavior change locally); working-tree
  materialization (`checkout`, `status`, `diff`, `record`) similarly
  moved onto a separate `fsx.Tree` seam.

## [0.1.6] - 2026-09-03

### Fixed

- A same-patch edit that deleted two or more consecutive lines while
  inserting new content into that same gap left the file's line graph
  with a structural fork — indistinguishable from an unresolved merge
  conflict — with no concurrent patch involved at all. `status` then
  reported the file as permanently modified after every future record,
  and `diff` rendered an empty `--- / +++` header with nothing under
  it. See PLAN.md's Status section for the full root-cause writeup and
  the recovery path for a file already affected by this in existing
  history.

## [0.1.5] - 2026-09-01

### Added

- `9vcs restore <path>...` discards uncommitted changes to specific
  paths, rewriting each from its recorded state at head instead of
  requiring a whole-tree `checkout` (which refuses outright while
  anything is dirty). A path with no recorded state — an uncommitted
  addition, or one half of an uncommitted rename — is removed rather
  than erroring, so reverting a rename is just naming both paths:
  `9vcs restore old.txt new.txt`.

### Changed

- Bumped `github.com/sandgorgon/9p` to v0.7.0 (picks up v0.6.0's `9pc
  -net` flag and v0.7.0's `Fid.OpenFile`/`CreateFile`). No 9vcs code
  changes needed.

## [0.1.4] - 2026-08-29

### Added

- Selective (partial) record: `9vcs record -p` interactively prompts
  (darcs-style) over which pending changes to fold into a patch, or
  `--lines PATH:ID[,ID...]` / `--files PATH[,PATH...]` select them
  programmatically. Anything not selected stays pending, exactly as if
  it hadn't been touched — keyed by 9vcs's existing per-line identity
  rather than a fragile byte-offset hunk, so a selection stays
  well-defined even as other pending edits in the same file are
  selected or left for later.

### Changed

- The repo-state/working-tree-diff logic (`ChangedFiles`,
  `WriteWorkingTree`, ref/HEAD/merge-state handling, and related
  helpers) moved out of the unexported `cmd/9vcs` package into a new
  importable `github.com/sandgorgon/9vcs/repo` package, so an external
  Go program can open a repo and compute a working-tree diff without
  shelling out to the `9vcs` binary. No CLI behavior change.

## [0.1.3] - 2026-08-28

### Added

- `9vcs version`, and the running version printed in `9vcs help`'s
  usage line.

### Changed

- Identity, fingerprinting, TLS config, and known-peers/authorized-
  peers handling moved out of 9vcs's own `identity/` package into the
  shared, zero-dependency [`github.com/sandgorgon/9auth`](https://github.com/sandgorgon/9auth)
  module (v0.1.0) — one identity, one trust decision, shared with
  other 9-family programs instead of a copy per project. The
  identity/known-peers directory moves from `~/.config/9vcs` to
  `~/.config/9`; an existing install's identity is copied forward
  automatically on first use, preserving its fingerprint (and every
  peer's existing pin of it). `user.name`/`user.email` config still
  lives at `~/.config/9vcs/config`, unaffected by this move. No user-
  facing command or permission-model changes.

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
