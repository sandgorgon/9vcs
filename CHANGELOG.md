# Changelog

All notable changes to 9vcs are documented here. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/); versioning is
[semver](https://semver.org/), with the caveat described in the
README's [Versioning and compatibility](README.md#versioning-and-compatibility)
section — the on-disk patch/bundle format makes no compatibility
promise between pre-`1.0.0` releases.

## [0.1.8] - 2026-09-11

### Changed

- Bumped `github.com/sandgorgon/9p` to v0.9.1 — a docs/test-flake-only
  upstream release (README/doc comments corrected to document 9P2000.u
  support instead of denying it; a flaky `TestMaxConcurrentRequestsLimitsConcurrency`
  deadlock fixed, test-only, no server behavior changed). No 9vcs code
  changes needed.

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
  it. Root cause: `OpDelete` only ever reconnects around its own
  immediate neighbor, so a same-patch run of two or more consecutive
  line deletes left one surviving one-hop "shortcut" edge alive from
  the gap's start; a following insert into that same gap only retracted
  the direct edge it was told about, never a shortcut reached by
  chaining through dead nodes. A second, related bug in the diff
  engine's LCS reconstruction — matching a common line back to *some*
  old line with equal content, not the exact index the alignment chose
  — could fabricate the same kind of fork whenever a file had a
  duplicate line, even without a multi-line delete run. Fixed in
  `objstore/patches/diff.go`: the LCS step now returns the exact
  `(oldIdx, newIdx)` pairs the alignment chose, and `Diff` collapses a
  same-patch multi-line delete run to a single direct edge before any
  insert lands on that gap. A repo already affected by this does not
  self-heal via a plain re-record (the healing ops target the fork's
  resolved successor, not the actual dangling shortcut) — recover by
  deleting the affected path and recording that, then re-adding it and
  recording again; `Materialize` wipes a path's entire graph object on
  a delete, so the re-add starts a clean line history with content
  preserved on disk throughout.

## [0.1.5] - 2026-09-01

### Added

- `9vcs restore <path>...` discards uncommitted changes to specific
  paths, rewriting each from its recorded state at head instead of
  requiring a whole-tree `checkout` (which refuses outright while
  anything is dirty). A path with no recorded state — an uncommitted
  addition, or one half of an uncommitted rename — is removed rather
  than erroring, so reverting a rename is just naming both paths:
  `9vcs restore old.txt new.txt`. Reuses the existing
  materialize/write-working-tree machinery directly — no new
  object-model concept and no staging index added.

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
  selected or left for later. The core primitive, `repo.SelectOps`,
  re-anchors a selected insert's `Prev` pointer through a
  resolve-through-unselected map when consecutive new lines chain
  through each other's freshly-minted IDs, so a non-contiguous
  selection (e.g. keep the 1st and 3rd of three new consecutive lines)
  still produces a valid, independently-replayable op list.

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
- `apply`: a true N-way merge patch across multiple patches/branches in
  a single step, not chained pairwise merges. The underlying graph
  fork/resolve machinery was already N-way; only the CLI-facing
  `MERGE_HEAD`/`computeMerge` layer above it needed generalizing from a
  hardcoded two sides to an arbitrary list.
- Networking: `9vcs serve` (a 9P2000 server over TLS 1.3 with pinned-
  fingerprint peer authentication), `import` (one-way pull), and
  `reconcile` (bidirectional pull/push), each gated by per-peer
  `read`/`propose`/`write` permissions.
- Offline change exchange: `bundle export`/`import`/`show` (signed,
  single-file `.9vp` bundles — magic + version byte + signer public key
  + signature + payload, signed over the payload bytes exactly as read
  off the wire, no re-encode round-trip needed to verify) and
  `offer`/`offer list`/`offer apply`/`offer remove` (submit a change
  without write access, via a peer's `/offers` mailbox on a running
  `serve`, gated by a new `propose` permission tier narrower than
  `write` — can post and list offers, can't move `/refs`). Both
  `bundle import` and `offer apply` only decode, verify, and store —
  neither touches a ref or auto-integrates anything; that's always a
  separate, explicit `apply`/`merge`/`diff` step.
- Per-patch authorship: optional Ed25519 signing
  (`AuthorFingerprint`/`AuthorSignature`), verified independently of
  transport-level trust — a relay can't forge authorship of a patch it
  merely passes along.
- `9vcs config [-global] user.name|user.email`: configures the identity
  `record` signs patches under (formatted `"Name <email>"`), cascading
  repo-local (`.9vcs/config`) → global (`~/.config/9vcs/config`) → OS
  username, the same precedence model as `git config`.
- `.9vcsignore` support, executable-bit and symlink tracking.
- Client-side known-peers store with trust-on-first-use semantics for
  `import`/`reconcile`.

### Security

A round of audits ahead of this release found and fixed several real
issues:

**Path traversal**

- `FileChange.Path` traversal — a patch's `Path` field reached
  `filepath.Join(r.root, ...)` unchecked on write-out. A local `record`
  can never produce a `../`-laden path (paths only ever come from a
  real directory walk), but a patch received via `import`/`reconcile`,
  a served push, or a bundle controls this field freely — a crafted,
  legitimately-signed patch could write a file anywhere the process
  could reach, entirely outside the repo. Fixed with `validPath`
  (`objstore/patches/patch.go`), checked at `Decode` (every patch
  received from outside the process) and again at `Store.Put` (a
  backstop regardless of construction path) — rejecting an empty path,
  a leading `/`, and a leading `..` segment specifically (`path.Clean`
  alone does not reject `"../outside.txt"`, since it's already
  canonical).
- Ref-name path traversal, network-reachable — branch/ref names were
  never validated at all: `vcsfs` passes a `Twalk`/`Tcreate` name
  straight through to the ref reader/writer, and a single 9P `Wname`
  element can itself contain embedded `/`/`..` with no library-level
  rejection. A peer holding only `PermWrite` (no local filesystem
  access) could get an arbitrary-path write of a hex hash string
  outside `.9vcs/refs`. Fixed with `ValidRefName`
  (`repo/repo.go`, `cmd/9vcs/repo.go` at the time), wired into the
  single choke point every local and remote ref read/write already
  goes through.
- Symlink path traversal via an intermediate path component — the
  `FileChange.Path` fix above only rejects a literal `..` *segment* in
  the path string; it says nothing about a path like
  `evil/nested/file.txt` where `evil` is itself a tracked symlink
  pointing outside the repo. Working-tree writes used a plain
  `filepath.Join` + `os.WriteFile`/`MkdirAll`, which — like any POSIX
  path resolution — follows a symlink at *any* intermediate component,
  not just the final one. Fixed by routing every working-tree write
  through `os.Root` opened at the repo root: it follows a symlink that
  resolves within the root but refuses one that would leave it, while
  still allowing an absolute-target symlink to be *created* as a leaf
  (so a legitimate `bin/env -> /usr/bin/env` still works).
- Binary-conflict sidecar writes bypassed the fix above — three sibling
  call sites (the `merge`/`apply` binary-conflict comparison sidecar,
  and `merge -abort`/`record`'s cleanup of it) still used a plain
  `filepath.Join` plus raw `os.*` calls instead of `os.Root`. This one
  needs no crafted patch at all: an ordinary symlinked cache/vendor
  directory already sitting in the working tree, plus a mundane
  two-sided binary conflict underneath it, was enough to write or
  delete outside the repo. Fixed by routing all four call sites through
  the same `os.Root`-confined helpers.

**Resource exhaustion / DoS**

- Unbounded write-offset allocation, plus an integer-overflow variant —
  the file types backing `/patches`, `/blobs`, `/offers`, and `/refs`
  grew their write buffer to a client-claimed `offset + len(payload)`
  with no upper bound: a 2-byte write claiming a 400MB offset grew
  server heap by ~400MB, reachable at the weakest trust tier
  (`PermPropose`, via `/offers`). A large enough offset also wrapped
  around to a negative `int64`, which Go's slicing panics on
  unconditionally — and since every 9P request runs in its own
  unrecovered goroutine, one such write could crash the entire `9vcs
  serve` process, not just the offending connection. Fixed with
  `checkWriteSize`: rejects a negative offset outright, rejects the
  claimed `offset` alone above a 1 GiB cap *before* adding the payload
  length to it (closing the overflow window), and only then rejects the
  sum.
- Unbounded per-connection memory from concurrently-open write-fids —
  even with a single fid's buffer capped, nothing bounded how many
  write-fids one connection could hold open (and buffering)
  concurrently: create many fids, write close to the cap into each,
  never clunk. Fixed with a connection-wide write-buffer byte budget,
  reserved against before any buffer grows and released on close,
  scoped per-connection so a client that vanishes without clunking
  can't leak accounting server-wide.
- The topological sort underlying `Materialize`/`History`/`Closure`
  re-sorted its entire ready-queue from scratch on every pop instead of
  using a real priority queue — O(n² log n) total. Invisible for a
  normal repo (one root patch), but nothing stops a peer from feeding
  in many mutually-independent patches via `import`/`reconcile`/a
  served push, and once thousands are in play a `log`/`merge`/`checkout`
  pays a real CPU cost. Fixed with a `container/heap`-backed min-heap
  preserving the same deterministic tie-break, O(n log n) total.
- A length-prefixed element count (e.g. an op count) was validated only
  against total bytes remaining in the input, not against how many
  bytes one *element* actually needs — so a claim near the raw byte
  count could demand an allocation up to ~72x larger than the input
  could legitimately back. Go's response to a failed huge allocation is
  a *fatal*, unrecoverable runtime error, not a normal panic — one
  crafted ~1 GiB patch or bundle object could crash the whole server
  via a ~72 GB allocation attempt. Fixed by giving the count-reading
  helper a minimum-element-size parameter per count type, in both
  `objstore/patches` and `bundle` (which had an independent copy of the
  same gap).
- `HashFromHex` accepted the wrong decoded length — valid-but-wrong-
  length hex silently zero-padded (in the worst case, coercing to the
  zero-hash "no such ref" sentinel) or silently truncated, with no
  error either way. No concrete exploit found, but hardened
  defensively, the same "reject rather than silently coerce" shape as
  the path-traversal fixes above.

**Concurrency & atomicity**

- Ref/HEAD writes were neither atomic nor compare-and-swapped: a plain
  `os.WriteFile` straight to the final path (a crash mid-write could
  leave a torn ref), guarded only by an in-memory mutex that does
  nothing across processes. Two local invocations, or a local command
  racing a live `serve`'s incoming push, could silently drop one side's
  update with no conflict ever raised. Fixed with a cross-process
  advisory lock (`withRefLock`, via `os.O_EXCL`, stealing a lock older
  than 10s as abandoned) plus compare-and-swap on every local mutating
  call site, both run inside the lock so check-then-write is atomic.
- `MERGE_HEAD`/`MERGE_SIDECARS` writes had the same gap, local-only: no
  atomic rename and no lock, so a crash mid-write could leave a
  truncated `MERGE_HEAD`, and two concurrent local merge/apply
  invocations could interleave writes after both independently passed
  the "no merge in progress" check. Fixed by routing all four writers
  through the same ref lock and atomic temp-file-then-rename.
- Two goroutines writing *identical* content concurrently (a real
  scenario: two peer connections relaying the same patch to one `serve`
  process at once) shared a temp filename derived only from the
  content hash, so one writer's already-created temp file caused the
  other's create to fail with a spurious "permission denied." Fixed
  with a unique-per-call temp name, plus a fallback: if the rename
  still fails, a concurrent writer having already placed the
  (identical, content-addressed) content there counts as success, not
  failure.

**Merge / conflict-detection correctness**

- N-way `apply`/`merge` silently dropped a binary/symlink conflict when
  "ours" never had the path — the conflict-detection loop anchored
  solely on the first root ("ours"), so a path only two *other* roots
  both introduced with different content was never even visited, and
  the plain union silently picked a side with no conflict reported.
  Directly reachable via `apply`'s whole reason to exist
  (non-"ours"-anchored N-way merges). Fixed by checking every root that
  has the path, not just the first one.
- The fix above still missed a cross-*kind* mismatch — it compared
  same-kind values (blob vs. blob) but a root introducing the same path
  under a completely different kind (e.g. text vs. binary) matched
  neither case and was silently dropped the same way. Fixed by checking
  for a kind mismatch first, reported as its own `"type"` conflict kind
  rather than being conflated with (or crashing) the binary-conflict
  sidecar path, which assumes a real blob to compare.
- Modify/delete resolution had an order-dependent fork bug — not
  security, a correctness bug: finalizing an ordinary modify/delete
  merge conflict (one side edits, the other deletes, resolution keeps
  the edit) intermittently left the working tree looking dirty
  immediately after `record`, because materializing wipes a path's
  *entire* graph object on any delete, and whether the modifying side's
  own node survived that wipe depended on the same hash-based
  topological tie-break that created the conflict in the first place —
  occasionally producing two live nodes with identical content, a
  genuine fork. Fixed by explicitly emitting a delete for every node
  the modifying side's own already-correct resolution reports alive,
  before the fresh insert that records the kept content, regardless of
  wipe-vs-insert ordering.
- The O(n·m) diff cost underlying rename detection was called once per
  deleted×added path pair; a changeset touching several large files
  (plausible after a peer import) turned an ordinary `status`/`diff`
  into a multiplicative pile of expensive diffs never directly asked
  for. A cell-count bound plus a hash-based fast path for exact-content
  renames closed the acute cost, but the underlying shape (still
  O(deleted × added) candidate pairs, each paying an O(file size) hash)
  remained — not worth the ongoing cost/complexity for a purely
  cosmetic, display-time feature. See Removed below.
- A signing-order bug in `apply`'s merge patches, found live rather
  than by a unit test: patches were signed *before* the store's
  internal field-reordering (`Normalize`) ran, so the signed bytes and
  the later-verified bytes diverged whenever a patch had more than one
  dependency to reorder. Invisible for an ordinary two-way merge, but
  any `apply`-produced merge patch has several dependencies —
  surfacing as `9vcs log` printing `(INVALID SIGNATURE)` on an
  otherwise-correct, cleanly-applied merge. Fixed by having the signing
  step normalize the patch itself first (idempotent, so the store's
  later normalization is a no-op).

### Removed

- Rename detection was built, then removed the same day, pre-release —
  its remaining O(deleted × added) candidate-pair cost (see Security
  above) wasn't worth carrying for a purely cosmetic, display-time
  feature. A moved file now records as a plain delete+add pair, exactly
  as it's stored regardless of whether a rename is inferred.

### Known limitations

- iOS and Windows builds are unverified — untested on either platform
  (see PLAN.md's "Open items to revisit").
- No format-compatibility promise between `v0.x` releases — see
  [Versioning and compatibility](README.md#versioning-and-compatibility).
