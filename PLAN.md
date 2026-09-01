# 9vcs — plan (as of 2026-08-23)

A version control system built on `github.com/sandgorgon/9p` (a pure-Go,
stdlib-only 9P2000 client/server library), leaning into Plan 9's actual
concepts (namespaces, union composition, synthetic file servers) rather
than treating 9P as just a transport. Implementation language: Go only,
throughout. No GitHub-shaped vocabulary (no clone/remote/push/pull/fork/PR).

## Library facts (verified against github.com/sandgorgon/9p source, not assumed)

- Packages: `p9` (wire encoding — `Marshal`/`Unmarshal`, `Qid`, `Stat`,
  `Mode`), `p9/client` (`Dial`, `NewClient`, `Attach`, `Walk`, `Open`,
  `File` as `io.Reader/Writer/ReaderAt/WriterAt/Seeker/Closer`),
  `p9/server` (`FileSystem` interface — one method, `Attach`; `File`
  interface — `Qid/Stat/WStat/Walk/Open/Create/Read/Write/Remove/Close`;
  `Server{FS, Msize}.Serve(net.Listener)` / `.ServeConn(conn)`).
- `client.NewClient(rwc io.ReadWriteCloser, ...)` wraps an **already
  connected** transport (confirmed by reading client.go) — TLS-wrapping
  the connection before handing it in requires zero library changes.
- `server.ServeConn(nc)` likewise takes an existing connection, and
  `Server` is a cheap two-field struct — safe to construct one per
  accepted connection.
- No auth: `Tauth` always fails ("authentication not required"), client
  always attaches with `NOFID`. Real peer auth has to be done at the
  transport layer (TLS), not via 9P's own auth messages.
- Only base 9P2000 — no `.u`/`.L` extensions. (This is why relying on
  Linux's native `mount -t 9p` was ruled out — see "rejected" below.)
- `Tflush` cancellation reaches backend `context.Context` params — useful
  for cancelling expensive synthesis mid-flight.

## Core architectural decisions

### 1. Object model: patch theory, not snapshots (Pijul-style)

History is a set of content-addressed **patches** (graph operations —
insert/delete a node/edge in a per-file line-graph) rather than
snapshots. Patches commute when they don't touch overlapping graph
regions, so partial pulls, cherry-picks, and reconciling two
independently-evolved histories are well-defined without an explicit
merge step — unlike git's heuristic three-way diff, and fixing the
correctness/performance issues Darcs' original informal patch theory had
(Pijul's actual motivation for existing). Conflicts show up as a fork in
the graph (multiple outgoing edges from one node), resolved by recording
a *new* patch that orders the diverging edges — not a blocking failure
for the rest of the repo.

Honest cost: this is the real research-grade bet in the whole design.
Needs either a persistent MVCC structure (what Pijul built, Sanakirja) or
— the chosen path here — an in-memory synthesis+cache layer (below) to
avoid replaying full history on every read. "Well-founded and
well-tested" is the realistic bar; "formally verified" would need actual
proof work (e.g. a TLA+ spec of the commutation/reconcile protocol),
called out as future work, not assumed.

#### Author identity — concrete scope (2026-08-24, not yet built)

Today `Patch.Author` is just `os/user.Current().Username` — whatever the
local OS account happens to be called, no email, nothing configurable.
Two improvements, deliberately scoped separately since they have very
different blast radius:

**Tier 1 — configurable name/email (git-style, no format change).**
`Patch.Author` stays exactly what it is today: a single string. What
changes is where it comes from:

1. repo-local `.9vcs/config` (`name = ...` / `email = ...`, plain
   `key = value` lines — same spirit as `authorized-peers`/`known-peers`'s
   plain text formats, not real INI)
2. global `~/.config/9vcs/config` (`os.UserConfigDir()/9vcs/config` —
   same directory `identity` already owns, but this file is user
   preference, not key material, so it lives alongside it rather than in
   the `identity` package itself)
3. OS username — unchanged fallback, so a fresh install behaves exactly
   as it does today until configured

`9vcs config user.name "..."` / `9vcs config user.email "..."` write to
the repo-local file by default, `--global` writes to the user-wide one;
called with no value, it prints the resolved value. `author()` in
`cmd/9vcs/workingtree.go` becomes: resolve name/email through the
precedence above, format as `"Name <email>"` (both set), `"Name"` (name
only), or the OS-username fallback (neither set). Zero per-patch
friction — configured once, read silently by every `record` after that,
same as `git config user.name`/`user.email`.

**Tier 2 — `AuthorFingerprint` + `AuthorSignature`, actually verified
(real format change).** Revised 2026-08-24: the user stated 9vcs is meant
for real multi-user adoption, not solo-only use, as an explicit design
assumption from here on — see PLAN.md's "Status"/Open items and the
project's multiuser-goal memory. That reverses the reasoning an earlier
version of this subsection used ("informational only... not worth
signing while solo, no second identity to correlate against"): the whole
value of per-patch provenance — telling a genuine author from a forged
claim, and having that survive a patch being relayed through more than
one hop — only exists once more than one identity is actually in play,
which is now the assumed baseline, not a hypothetical. Tier 2 is
therefore scoped as real signing from the start, not an unsigned
stepping stone — see the dedicated subsection below.

**Format-compatibility question, resolved simple.** Adding fields changes
the byte layout for every patch, and this project is pre-release — one
person, no data anywhere that depends on the format staying stable, so
there's nothing to preserve compatibility with yet. Rather than building
real multi-version dispatch prematurely, `Patch.Encode()` just gained one
leading `patchFormatVersion` byte that `Decode` checks and refuses to
misparse past — cheap insurance against a silent misparse, not an attempt
at supporting multiple format versions. Real version dispatch is a
decision to make once there's a formal release and therefore real data
whose compatibility actually matters — see the dedicated subsection
below for the full reasoning (including what got built and then
deliberately removed once this was clarified).

#### Config file format for user.name/email — concrete scope (2026-08-24, not yet built)

**File format.** Plain text, one `key = value` per line, split on the
*first* `=` only (a value may legitimately contain more of them) with
both sides trimmed; blank lines and `#`-prefixed comments ignored — same
missing-file-is-not-an-error convention `identity.LoadKnownPeers` already
uses. Deliberately *not* git's sectioned INI (`[user]` / tab-indented
keys): the two known keys are stored flat as `user.name` / `user.email`
— the same dotted string the CLI takes — which needs no section-header
parser at all and stays trivially extensible to more `9vcs config` keys
later without a format change, consistent with the flat, minimal
`key value`/`key = value` shape every other config-ish file in this
codebase (`known-peers`, `authorized-peers`) already uses.

```
user.name = Ramon de Vera Jr.
user.email = ramondevera@gmail.com
```

**Two files, resolved per-key like git, not per-file.** Repo-local
`.9vcs/config` (`filepath.Join(r.dir, "config")`) is checked first for a
given key; if that key specifically is absent, `~/.config/9vcs/config`
(`os.UserConfigDir()/9vcs/config` — the directory `identity` already
owns; reuse its existing `configDir()` logic via a small exported
accessor rather than re-deriving the path in `cmd/9vcs`, since the file
itself is a sibling of `identity.key`/`known-peers` in that same
directory even though it isn't identity/crypto material) is checked next;
absent from both means "unconfigured" for that key. Per-key, not
per-file, matters concretely for the case that motivated this: run
`9vcs config --global user.email ramondevera@gmail.com` once, machine-
wide, and it applies everywhere — but a specific repo can still override
just `user.email` locally (e.g. a work checkout wanting a different
address) without needing to also repeat `user.name` there, exactly like
`git config` cascades.

**CLI.** `9vcs config [--global] <key> [<value>]`:
- one arg → get: prints the *resolved* value (repo-local, else global,
  else empty/"not configured"); `--global` restricts the lookup to only
  the global file, skipping the repo-local override
- two args → set: writes to the repo-local file by default, or the
  global file with `--global`; `findRepo()` is only required for a
  non-`--global` call (get or set) — a bare `--global` operation touches
  only the user-wide file and works outside any repo, same as `9vcs
  identity show` needs no repo today
- `<key>` is validated against a fixed known-key list (`user.name`,
  `user.email` only, for now) rather than accepting arbitrary keys — this
  is deliberately not a general config system yet, just the two keys
  `author()` actually needs; the flat file format doesn't need to change
  when that list grows later, only the CLI's validation does

**`author()` gets a new required parameter.** It's currently `func
author() string` with no arguments; resolving repo-local config needs
`r.dir`, so it becomes `func author(r *repo) string`, cascading
`user.name`/`user.email` through the precedence above and formatting
`"Name <email>"` / `"Name"` / the existing OS-username fallback exactly
as already described. The one ripple: `record.go`'s call site changes
from `Author: author()` to `Author: author(r)` — `r` is already in scope
there.

#### AuthorFingerprint + AuthorSignature, single-format (built 2026-08-24)

Built, then deliberately simplified the same day once the underlying
assumption was corrected: this project is pre-release with exactly one
person using it, so there's no real data anywhere that a format change
needs to keep decoding — see "Why no real multi-version support yet"
below. What shipped is real per-patch signing without the multi-version
dispatch machinery that would matter once a release actually exists.

**Why bundle-level and TLS-peer-level trust aren't enough on their own.**
Decision #8's (unbuilt) bundle signature and decision #7's TLS peer auth
both only establish *who handed you this patch*, not *who wrote it*, once
more than two people are involved — a relayed bundle, a shared hub
re-serving other contributors' patches, a peer that's honest about its
own patches but merely forwarding someone else's. In every one of those,
the transport-level guarantee covers only the most recent hop. Only a
**signature that travels with the patch itself**, checked independently
of transport, makes authorship survive being relayed — the scenario that
matters once this is actually shared between people instead of staying
on one machine, which is now this project's explicit baseline assumption
(see the note at the top of the Author identity subsection above).

**Fields.** `Patch` carries `AuthorFingerprint [32]byte` (the recording
install's raw Ed25519 public key — not the hex string, same fixed-size
treatment `Hash` already gets) and `AuthorSignature [64]byte` (an Ed25519
signature over everything else in the encoding). Both all-zero together
means "unsigned" — a real Ed25519 key/signature pair is never all-zero,
so the zero value is unambiguous — and that's a fully legitimate,
accepted state (see Verification), not an error: this is opportunistic
signing, never a hard requirement that could turn a missing identity into
a blocked `record`.

**Why no real multi-version support yet.** An earlier pass at this built
actual dual-format dispatch — a `patchEncodingV1`/`patchEncodingV2` split,
a sticky per-`Patch` `version` field, `decodeV1Fields` factored out as a
subroutine both versions' decoders shared — all in service of one
property: re-`Encode`ing a `Decode`'d patch must always reproduce its
original bytes, so a future format change can't silently break
`fetchPatch`/`vcsfs`'s hash re-check for patches that predate it. That
property only matters once there's a second version of the format for
some patch to have predated — which requires a release with real,
depended-upon data. Pre-release, that never happens: there's one person,
one format, and every existing patch anywhere is disposable test data.
Carrying the dispatch machinery bought nothing yet and cost real
complexity, so it came back out. What's kept: a single leading
`patchFormatVersion` byte, always the same value, that `Decode` checks
and refuses to misparse past — cheap insurance for the day this format
does need to change out from under existing data, at effectively zero
cost today. **When a formal release happens**, that's the trigger to
reintroduce real version dispatch for the *next* format change from that
point on — this subsection's git history is a working example of what
that refactor looks like when it's actually needed.

**What `AuthorSignature` actually signs.** Everything the encoding
contains *except* the signature itself — the format byte,
`Dependencies`/`Author`/`Time`/`Message`/`Changes`, and
`AuthorFingerprint` — via `SignablePayload()`, kept as its own method
(not inlined into `Encode`) specifically so `Encode` stays a pure
serializer with no key-material dependency; signing happens exactly
once, explicitly, in `cmd/9vcs/record.go`'s new `signPatch`
(`workingtree.go`), after constructing the patch and before `store.Put`:

```go
func signPatch(patch *patches.Patch) {
    id, err := identity.Load()
    if err != nil {
        fmt.Fprintf(os.Stderr, "warning: recording unsigned (identity unavailable): %v\n", err)
        return
    }
    copy(patch.AuthorFingerprint[:], id.Key.Public().(ed25519.PublicKey))
    sig := ed25519.Sign(id.Key, patch.SignablePayload())
    copy(patch.AuthorSignature[:], sig)
}
```

`identity.Load()` failing (permissions, disk full, whatever) produces an
unsigned patch and a stderr warning, never a failed `record` — `record`
is the single most-invoked command, and nothing else it does today can
fail this way.

**Verification — the actual point of doing this.**

```go
func (p *Patch) VerifyAuthorSignature() bool {
    if p.AuthorFingerprint == ([32]byte{}) {
        return true
    }
    return ed25519.Verify(p.AuthorFingerprint[:], p.SignablePayload(), p.AuthorSignature[:])
}
```

Wired into every path that ingests a patch from outside this process —
`sync.go`'s `fetchPatch` (client pulling via `import`/`reconcile`) and
`vcsfs.go`'s patch-write path (server receiving a push), both alongside
their existing `Hash` check but catching a genuinely different thing:
the hash check proves *these bytes are what's stored under this hash*
(transit integrity), not that the claimed authorship is real — a
dishonest peer can craft arbitrary content and correctly self-hash it,
but cannot produce a valid signature for a fingerprint whose private key
it doesn't hold. A failed `VerifyAuthorSignature` is refused with the
same severity as a hash mismatch; an unsigned patch is accepted exactly
as before. Bundle import (decision #8, still unbuilt) gets the same
check once it exists — independent of the bundle's own signer signature,
since a bundle can legitimately carry patches individually signed by
people other than whoever sent it.

**Display.** `cmd/9vcs/log.go` prints a `Fingerprint: ... (verified)` /
`(INVALID SIGNATURE)` line under `Author:` when `AuthorFingerprint` is
non-zero, via the same `identity.Fingerprint` call peer auth already
uses — copy-pasteable against `9vcs identity show` or an
`authorized-peers`/`known-peers` entry.

**Verified live**, over real TLS+9P between two distinct identities, not
just unit/integration tests: peer A records a patch (auto-signed, shows
`(verified)` in A's `log`); peer B `import`s it from A, and the same
patch — now materialized against a completely different machine's
identity — independently re-verifies as `(verified)` on B's side too,
proving the authorship claim travels with the patch, not just with the
transport that carried it. Unit tests cover the encode/decode round trip
(unsigned and signed), signature verification (valid, tampered-after-
signing, wrong-key, unsigned), and format-byte rejection
(`objstore/patches/encoding_test.go`); `vcsfs`'s
`TestPatchWriteForgedAuthorshipRejected`/`TestPatchWriteSignedAuthorshipAccepted`
cover the same over a real 9P connection, confirming a forged claim is
refused before ever being stored under any hash.

#### File mode and symlinks — concrete scope (2026-08-25, built)

The gap: `FileChange`/`PathState` tracked line content and whole-file
blobs, but nothing about a file's executable bit, and `workingFiles`
(`cmd/9vcs/repo.go`) excluded symlinks from the working-tree walk
outright — so a symlink was invisible to `record`/`diff`/`status`
entirely, and checking out an executable script from a peer silently
dropped the bit (`writeWorkingTree` always wrote `0o644`). Identified by
directly checking the codebase rather than assuming, while assessing
whether the patch/bundle format was stable enough to tag a first
release — it wasn't yet, so this shipped before that tag, as an in-place
format change rather than triggering real version-dispatch machinery
(see `patchFormatVersion`'s doc comment: that trigger needs *both* a
formal release *and* a subsequent change after it — only the second half
was true here).

**Format change: `patchFormatVersion` 1 → 2 (later renumbered back to 1,
see "Release versioning" below).** `FileChange`/`PathState` both gained
`Executable bool` (meaningful for `KindText`/`KindBlob` only — a
symlink's own mode bits are POSIX-meaningless) and a new `KindSymlink`
with `SymlinkTarget string` (stored as a plain string, not
blob-addressed — a target is a handful of bytes, not file content worth
a separate content-addressed entry). `Encode`/`Decode` updated in
lockstep, pinned by `TestEncodeDecodeRoundTripExecutableAndSymlink`.

**Where it touches, methodically enumerated before writing anything** —
every non-test file referencing `ChangeKind` was checked, not assumed
safe:
- `workingFiles` (`repo.go`) now includes symlink entries, not just
  regular files — `filepath.WalkDir` already doesn't follow a symlink to
  see what it points at, so this needed no other change.
- `changedFiles` (`workingtree.go`) `Lstat`s each path first: a symlink
  short-circuits to a `KindSymlink` change via `os.Readlink`; otherwise
  the executable bit is read alongside content as before.
- `writeWorkingTree` creates real symlinks (`os.Symlink`, clearing
  whatever's there first) and restores the executable bit via
  `os.Chmod` after writing.
- `computeMerge` (`mergeutil.go`) treats a symlink-target disagreement
  as an atomic conflict exactly like a binary one — same "keep ours,
  flag it" policy — but reports it as its own `"symlink"` kind rather
  than reusing `"binary"`, since the conflict message lists every
  differing target inline (a target is a short, readable string) rather
  than needing `"binary"`'s comparison-sidecar file.
- `record.go`'s modify/delete-conflict resolution, `diffRefs`,
  `renderDiff`, and `log.go`'s per-change summary all gained explicit
  symlink/executable handling — each was individually a plausible place
  for a symlink or an exec-bit-only change to either crash, get silently
  dropped, or render wrong, not just add a case for completeness.

**Two real bugs, both caught by dedicated regression tests before this
was called done — not found by accident:**
1. `changedFiles`/`diffRefs`'s "unchanged" checks compared only content
   (blob hash, or line ops + trailing newline) — never `Executable`. A
   bare `chmod +x` with no other edit would have been completely
   invisible to `record`/`diff`/`status`, despite the field existing and
   encoding correctly. Fixed by adding `Executable` to every unchanged
   comparison; pinned by
   `TestChangedFilesExecutableOnlyChangeIsDetected`.
2. `os.WriteFile`'s mode argument only applies when it actually creates
   the file — POSIX `open(2)` leaves an already-existing file's
   permission bits untouched — so `writeWorkingTree` overwriting an
   *already-checked-out* path (the ordinary case: switching branches,
   not a fresh checkout) would silently fail to actually toggle the
   executable bit. Fixed with an explicit `os.Chmod` after every write;
   pinned by `TestWriteWorkingTreeSetsExecutableBitOnExistingFile`.

**Verified live** with the real binary, not just `go test`: recorded an
executable script, a symlink, and a plain file in one patch (`status`/
`log` showed all three correctly, including the `(symlink)` and
`(executable)` annotations); deleted everything, recorded the deletion,
and checked out the prior hash from scratch — the script came back
executable and the symlink came back as a real symlink pointing at the
right target, not regenerated content. A real three-way symlink conflict
correctly reported `CONFLICT (symlink)` with the other side's target
listed, kept "ours," and `merge -abort` correctly restored the
pre-conflict target. Switching back and forth between two real branches
that differ only in one file's executable bit correctly flipped the bit
each direction, on the same already-existing path — the exact scenario
bug 2 above would have silently gotten wrong.

#### Release versioning — decided (2026-08-25)

Resolves the "No release/versioning process yet" open item that used to
sit at the end of this document. Two separate things needed a scheme:

**The project's own release version:** standard semver, starting at
`v0.x.y`. Nothing 9vcs-specific here — Go modules treat `v0`/`v1`
identically import-path-wise, so no extra decision is needed until a
hypothetical `v2` (which would need the `/v2` import-path suffix Go
modules require for major versions ≥ 2).

**The patch/bundle encoding format version** (`patchFormatVersion`,
bundle's `formatVersion`) is the one with real teeth: `Patch.Hash()` is
`sha256(p.Encode())`, so the hash covers the *encoded bytes, including
the version byte* — every patch's identity, and every other patch's
`Dependencies` reference to it, is pinned to its exact original
encoding forever. A future format change can't just swap old decoding
for new without real multi-version dispatch (or a full history rewrite
that mints new hashes for everything). `SignablePayload()` also embeds
`patchFormatVersion` as its first byte, so re-encoding a patch under a
different format version changes what its `AuthorSignature` was
computed over — a naive re-encode would silently invalidate every
historical signature, and the original author usually isn't available
to re-sign old history years later.

Decided: defer the *mechanism* (real dispatch), commit to a *policy*
instead — the "single format until both a formal release and a real
need to change it again" default already in place stays exactly as-is.
`v0.x` releases make no format-compatibility promise between
themselves (documented in the README/CHANGELOG when the first tag
ships); the actual permanent promise — "format version N will always be
decodable" — is a `v1.0.0` decision, not this one. When a real reason to
change the format eventually shows up, the current self-describing,
leading-version-byte encoding makes adding a new dispatched `case`
cheap to do *then*, against a concrete, known change — not speculatively
now against an unknown one.

Ahead of committing to that promise, `patchFormatVersion` was
renumbered from 2 back to **1** (see `patchFormatVersion`'s doc comment
and the "File mode and symlinks" write-up above): the in-place 1→2 bump
happened entirely pre-release, so neither value was ever externally
depended on, and shipping a first release with "2" as the first number
anyone outside this repo ever sees would have been a pointless artifact
of pre-release iteration. (Caveat for anyone who already has a real
`.9vcs` repo — even a personal dogfooding one — with patches recorded
under the old value 2: this renumbering makes that repo's stored
patches undecodable; it needs reinitializing, or bundle-exporting its
current content before the renumbering and importing after.)

**Migration tooling, for when a format change eventually happens:** a
generic "convert any old format to the current one" utility isn't
really a buildable thing today — the actual field-by-field mapping
depends entirely on what a *future* change actually does, which isn't
known yet, and (per the signature point above) any such conversion
would need to either invalidate old signatures or find some way to
preserve them, which also depends on the specifics of what changed. So
rather than build speculative converter machinery now, the commitment
is a **principle**: any future format-breaking change ships together
with its own purpose-built migration path (e.g. a `9vcs migrate`
subcommand, or a documented procedure), written by whoever makes that
specific change, since only they know the exact old-to-new mapping —
the same "spec it when there's a concrete need, not before" approach
this project already uses for `github.com/sandgorgon/9p` library
changes.

This doesn't leave existing users with nothing today, though: `9vcs
bundle export`/`import` already transports patches by their existing
hash, unchanged — no re-encoding involved, so it's unaffected by any
future format bump on either side as long as both installs still
recognize whatever formats are actually present in the bundle. And
worst case, even without any dedicated migration tooling at all, a
user's *current* working-tree content is never at risk — `9vcs init` a
fresh repo and record the current snapshot as a new root is always
available as a last resort. What a future incompatible format bump
could cost, absent a purpose-built migration path, is the fine-grained
patch *history* (the graph itself), never the actual file content.

#### Path traversal via `FileChange.Path` — real vulnerability, found and fixed (2026-08-25)

**Found while auditing for other gaps, not reported by anyone — verified
live before being called real, not assumed from reading code alone.**
`writeWorkingTree` (`cmd/9vcs/workingtree.go`) reaches every write via a
plain `filepath.Join(r.root, filepath.FromSlash(p))`. `filepath.Join`
does not confine the result to stay under `r.root` — a `p` containing
`../` segments resolves straight through and out. A real local `record`
can never produce such a path (`workingFiles` only ever discovers paths
via an actual `filepath.WalkDir` under the repo root), so this was
never reachable through normal use — but nothing validated
`FileChange.Path` on *ingestion*, and a patch's path is exactly the kind
of field a received-from-elsewhere patch controls: `import`/`reconcile`
(a peer's push), a served `/patches` write, or a bundle file all hand a
`Path` string to this codebase with no local walk ever having produced
it.

**Proven exploitable, live, before writing a single line of fix**: a
`.9vp` bundle was hand-crafted (a legitimately Ed25519-signed patch,
signature and all — exactly what any authorized `propose`/`write` peer
could produce) with a `FileChange.Path` of `../../../../pwned.txt`,
imported via the real `9vcs bundle import` and integrated via the real
`9vcs apply` against a real victim repo. The file landed exactly where
crafted, completely outside the victim's repo directory. `AuthorSignature`
verification passed throughout — the entire point of this finding: a
signature proves *who wrote the patch*, never that its *paths* are safe
to materialize, and nothing else in the design was checking that
separately.

**Fix: `validPath` (`objstore/patches/patch.go`)**, checked at both
places a `Patch` can come to be persisted:
- `Decode` — every patch received from outside this process (a peer
  fetch, a bundle, a served write) goes through this, so a malicious
  patch is refused the moment it's first decoded anywhere, before it can
  even propagate.
- `Store.Put` — every patch that's ever actually stored, however the
  `*Patch` value was built, as an independent backstop.

`validPath` rejects an empty path, a leading `/`, and — the part that
took a second pass to get right — a bare `..` at the front, since
`path.Clean` cannot eliminate a *leading* `..` in a relative path (its
own doc comment: retained when there's no preceding non-`..` element to
cancel it against) — meaning `path.Clean(p) == p` alone, the obvious
first attempt, does **not** reject `"../outside.txt"` at all, since
Clean leaves it untouched as already-canonical. Caught by
`TestValidPath` failing against exactly that input before the fix
shipped, not found by accident: fixed by an explicit per-segment scan
for a literal `".."` component, layered on top of the `Clean` check
(which still does its job for every other case — empty segments,
redundant `./`, `a/../b`, a trailing `/`).

**Verified**: `TestValidPath`, `TestDecodeRejectsPathTraversal`, and
`TestStorePutRejectsPathTraversal` (`objstore/patches`) cover the
matcher and both ingestion points directly; `vcsfs`'s
`TestPatchWritePathTraversalRejected` covers the same over the real
network write path (a served peer push), refused with the same severity
as a hash mismatch or a forged authorship claim, and never stored under
any hash — mirroring how `TestPatchWriteForgedAuthorshipRejected`
already proved the *authorship* side of "don't trust bytes just because
they parse."

#### Ref/HEAD write atomicity and local concurrency — found and fixed (2026-08-25)

**The gap, found by auditing rather than reported**: every local ref/HEAD
write (`setRefHash`, `setHeadBranch`, `setHeadDetached`) was a plain
`os.WriteFile` straight to the final path — not the temp-file-then-rename
pattern `rawStore.put` already used for the content-addressed store — so
a crash mid-write could leave a torn ref. Worse, none of it was
compare-and-swap: `record`/`merge`/`apply`/`reconcile`'s local pull all
read a branch's current hash, decided a new one, and wrote it
unconditionally, with `refMu` — an in-memory `sync.Mutex` — as the only
guard, and that only ever protects goroutines *within one process*. Two
separate local CLI invocations (two terminals, or a script and a
person), or a local command racing a live `9vcs serve`'s incoming push,
are different OS processes sharing no memory to synchronize through at
all: whichever write landed last would silently win, discarding another
command's patch from the branch with no conflict ever raised.

**Fix, two parts, in `cmd/9vcs/repo.go`:**
- **`withRefLock`**: a cross-process advisory lock using `os.O_EXCL` as
  the actual mutex primitive (Go's stdlib has no `flock`, and this
  project stays stdlib-only) — atomically creating a `.9vcs/lock` file
  is the acquire, removing it is the release. A lock older than 10s is
  assumed abandoned by a crashed process and stolen rather than
  deadlocking forever, since the critical section it guards is always a
  single small file read + write (milliseconds, never legitimately
  longer). Acquisition retries for up to 5s before giving up with a
  clear, actionable error.
- **CAS everywhere, not just for network writes**: `setRefHashCAS`
  (unchanged behavior — the write side of vcsfs's `/refs` contract, still
  refuses to move the checked-out branch) and a new `setLocalRefCAS`
  (same compare-and-swap, *without* that refusal — a local command always
  updates the working tree and the ref together, so the rule a network
  push needs doesn't apply) now share one implementation
  (`casWriteRef`), and both run their entire compare-then-write sequence
  inside `withRefLock` — not just the final write, since checking "is
  `old` still current" and writing the new value have to be atomic
  *together*, or two callers can both pass the check before either
  writes. Every local mutating call site (`record`, `merge`'s
  fast-forward, `apply`'s fast-forward, `branch`/`checkout -b`'s
  new-branch creation, `reconcile`/`import`'s local pull) already had
  the "old" hash it needed in scope — the value it read earlier in the
  same function — so converting them from a blind `setRefHash` to
  `setLocalRefCAS` needed no new bookkeeping, just routing through the
  same compare-and-swap the network path always had. `refMu` is retired
  entirely: the file lock subsumes it (a real cross-process primitive
  covers everything the in-memory mutex covered, plus what it couldn't).

**Verified**: `TestWithRefLockMutualExclusion` (20 goroutines, a guarded
critical section, asserting the observed concurrent-holder count never
exceeds 1), `TestWithRefLockStealsStaleLock`, `TestAtomicWriteFileLeavesNoTempFile`,
`TestSetLocalRefCASConflict`/`TestSetLocalRefCASAllowsCheckedOutBranch`,
and — the actual property this exists for —
`TestConcurrentSetLocalRefCASOnlyOneWins`: 10 goroutines racing to move
the same ref from the same observed old value to 10 different new
values; exactly one succeeds, the rest fail with `errRefConflict`, and
the ref ends up at precisely the one winner's value, never a corrupted
or unexpected one. `go test -race ./...` clean throughout. Verified live
against the real binary too, not just `go test`: a normal `record`
leaves no lock file behind; a stale (backdated) lock is silently stolen
rather than blocking; a genuinely-held lock causes a real pending change
to be refused cleanly after the full retry window — not corrupted, not
silently dropped — confirmed by `log` showing the same patch count
before and after, with the identical command succeeding normally once
the lock was released.

#### Unbounded write-offset allocation — found and fixed (2026-08-25)

**Found auditing `vcsfs.go`'s write path for other issues, proven live
before being called real**: `writeFile.Write`/`refFile.Write` (backing
`/patches`, `/blobs`, `/offers`, and `/refs`) grew their in-memory
buffer to `offset + len(p)` with no upper bound — and that size is
entirely *client-claimed*, not derived from how much data was actually
sent. A single `Twrite` with a tiny payload but an enormous offset
forces the server to attempt an allocation of that claimed size. Proven
live: a 2-byte write claiming a 400MB offset grew the server's heap by
~400MB; nothing bounds how much further that scales short of exhausting
the machine's memory. Reachable before any content, hash, or signature
is ever checked (that only happens at `Close`), and by the *lowest*
trust tier this server has — `PermPropose`, via `/offers` — not only
`PermWrite`.

**A second failure mode found while reading the same code, not
separately live-tested (backed instead by Go's own unambiguous slicing
semantics)**: a large enough unsigned 9P offset wraps into a negative
`int64` once decoded; the existing code would have reached
`buf[offset:]` with a negative index, which Go slicing always panics
on, unconditionally — a crash, not just a resource exhaustion.

**Fix**: `checkWriteSize` (`vcsfs/vcsfs.go`), checked before either
`Write` method touches its buffer at all — rejects a negative offset
outright, and rejects `offset + len(p)` beyond `maxObjectSize` (1 GiB,
chosen as generous relative to what any of these object types
legitimately need — patches/refs are small metadata, blobs are the only
case that could reasonably be sizable — a loose bound against a
malicious or buggy offset claim, not a considered product policy on
maximum file size).

**Verified**: `TestWriteRejectsOversizedOffset`/
`TestWriteRejectsNegativeOffset` (`vcsfs`) cover both rejections
directly. Re-ran the exact original live proof against the fixed code
with an offset actually over the cap (2GB, since the original 400MB
proof — chosen to be safe to run, not to test the specific limit — is
comfortably *under* the new 1GiB cap and remains correctly allowed):
heap growth dropped from ~400MB to ~0MB, confirming the rejection
happens before any allocation, not after a partial one.

#### Symlink path traversal via an intermediate component — found and fixed (2026-08-25)

**A second, distinct traversal vector, found auditing `writeWorkingTree`
after the `../`-string fix already existed — and not caught by it.**
`validPath` (the earlier fix) only rejects a literal `..` *segment* in
the path *string*. It has nothing to say about a path like
`evil/nested/proof.txt` — perfectly canonical, no `..` anywhere — where
`evil` is *itself* a tracked symlink pointing outside the repo. Two
`FileChange`s (a `KindSymlink` at `evil`, plus any ordinary change at
`evil/nested/proof.txt`) are each individually valid; `writeWorkingTree`
built the write target with a plain `filepath.Join(r.root, ...)` and
handed it to `os.MkdirAll`/`os.WriteFile`, which — like any POSIX path
resolution — follows a symlink at *any* intermediate component, not
just the final one. The existing symlink-clearing logic only ever
`Lstat`'d the *final* component, so it caught "this leaf is a stale
symlink" but had nothing to say about "an ancestor directory of this
leaf is a symlink."

**Proven live**: a hand-crafted patch with exactly those two changes,
applied against a real repo, wrote `proof.txt` into a directory
completely outside it, through the `evil` symlink — confirmed by
locating the file at the symlink's target, not inside the repo.

**Fix: `os.Root`** (`writeWorkingTree`, `cmd/9vcs/workingtree.go`) — a
stdlib API purpose-built for exactly this (added specifically to let
code work with untrusted path components without a symlink escaping a
directory tree): every operation goes through an `os.Root` opened at
`r.root` instead of a bare `os.*` call on a manually-joined path. `Root`
follows a symlink that resolves *within* the root, but refuses one that
would leave it, and refuses an absolute-target symlink as an
intermediate component outright — while still allowing an absolute
target to be *created* as a leaf symlink (creating one doesn't require
resolving where it points), so the legitimate case
(`bin/env -> /usr/bin/env`) is unaffected. `record.go`'s modify/delete-
conflict-resolution snippet — the one other place in this codebase that
reads working-tree content by path outside `changedFiles` (which is
safe by construction: its paths come from an actual `filepath.WalkDir`,
which never descends into a symlink to discover a path beneath it) —
was converted to the same `os.Root`-confined reads, for the same reason
and consistency, not because a live exploit was separately built for it.

**Verified**: `TestWriteWorkingTreeRefusesSymlinkPathEscape` reproduces
the exact live-proven case and asserts both the error and that nothing
landed outside the repo; `TestWriteWorkingTreeAllowsAbsoluteTargetLeafSymlink`
guards against a regression of the legitimate case. Re-ran the original
live exploit against the fixed binary: `apply` now fails cleanly
(`"mkdirat evil/nested: path escapes from parent"`) and the file never
appears outside the repo. Live-reconfirmed the legitimate absolute-target
leaf symlink case round-trips correctly end to end (record, delete,
checkout from scratch, `readlink` matches).

#### Modify/delete resolution: order-dependent fork — found and fixed (2026-08-25)

**Not a security vulnerability — a correctness bug, found by accident
while live-testing the symlink fix above, then properly root-caused
rather than patched blind.** Finalizing an ordinary modify/delete merge
conflict (one branch edits a file, the other deletes it, resolution
keeps the edit) intermittently left the working tree looking dirty
immediately after `record` finished — `9vcs status` reporting `M f.txt`
against the patch that had *just* recorded that exact content. Bisected
first, before touching anything: reproduced identically on the
pre-existing binary (2 of 5 runs), confirming it predated every other
fix in this session and wasn't caused by any of them.

**Root-caused with a deterministic, standalone reproduction** against
`objstore/patches` directly (fixed patch content, no wall-clock
`Time` field, so hashes — and therefore `topoOrder`'s tie-break between
two simultaneously-ready patches — are reproducible run to run instead
of effectively random the way real `record`-driven timestamps make
them). The mechanism, confirmed empirically rather than by hand-tracing
alone (an earlier by-hand attempt at this was wrong): `Materialize`
wipes a path's *entire* graph object on any `KindDelete` for that path —
not just the one node the deleting side targeted. The modify/delete
resolution's own change (unconditionally `Diff(nil, ...)`, a fresh
insert under a brand-new line ID) always applies last, being part of
the merge patch — but whether the *modifying* side's own original node
is still alive when that fresh insert runs depends entirely on whether
the deleting side's wipe landed before or after it, which depends on
the same hash-based topological tiebreak that created the conflict.
When the modifying side's node survives, the fresh insert creates a
second, independent node with identical content — a genuine,
order-triggered fork (two alive nodes, one content, both reachable),
not a cosmetic display glitch.

**Two candidate fixes were tried and rejected before the real one,
each disproven empirically, not assumed**: omitting the override
entirely (fails — the deleting side's own delete can then legitimately
win the tiebreak and wipe the file); relying on `Diff` against the
existing content to naturally produce empty ops when nothing textually
differs (fails for the same reason as omitting the override — an empty
op list is a true no-op, so it doesn't pin anything either).

**Fix** (`modifyDeleteKeptTextOps`, extracted out of `cmd/9vcs/record.go`
into its own testable function): before the fresh insert, explicitly
emit a delete for every node `base[path]`'s own graph already reports
alive — `base` here being exactly `computeMerge`'s own already-correct
resolution (`mergeutil.go`'s `idxs[j][p]`, the modifying side's isolated
materialize, unaffected by the wipe since it never replays the deleting
side at all). Deleting an already-wiped, nonexistent node is a harmless
no-op (`graph.go`'s `Delete` case creates a dead placeholder rather than
erroring); deleting a still-alive one neutralizes it. Either way, only
the fresh insert's single node survives, regardless of which order the
wipe and the insert actually happened in. Scoped to `KindText` only:
`KindBlob`/`KindSymlink` are plain value overwrites in `Materialize`,
not additive graph operations, so they were never susceptible to this.

**Verified**: `TestModifyDeleteKeptTextOpsIsOrderIndependent` samples
20 different patch-message salts (each producing different hashes, and
therefore sampling both possible topological orderings — the test
itself asserts both were actually observed, not just assumed) against
the real `modifyDeleteKeptTextOps`/`computeMerge`, requiring every
single one to materialize to exactly the kept content with zero forks.
Re-ran the exact original reproduction live against the rebuilt binary
15 times: 15/15 clean (previously roughly 40% failed) — and confirmed
the recorded content is genuinely correct, not just coincidentally
"clean" (`cat f.txt` shows the real kept content).

#### Concurrent identical-content Put race — found and fixed (2026-08-25)

`rawStore.put` (`objstore/patches/rawstore.go`) used a temp filename
derived only from the content hash (`path + ".tmp"`). Two concurrent
`Put` calls for the *same* content — a real, reachable scenario: two
peer connections relaying the same patch to one `9vcs serve` process at
once, each its own goroutine per the library's own concurrency model —
shared that temp filename, so one writer could create/rename it out from
under the other. Reproduced live before the fix: a spurious "permission
denied" (the first writer's read-only temp file already sitting at that
exact path when the second tried to create it) on an otherwise
completely harmless race — the content two concurrent callers write here
is identical by construction, since it's keyed by its own hash. Fixed
with `os.CreateTemp` for a unique-per-call name, plus a fallback check
(if `os.Rename` still somehow fails, re-`Stat` the target — a concurrent
writer having already placed it there is success, not failure, for a
content-addressed, idempotent `Put`). Verified: `TestConcurrentPutSameContentDoesNotError`
(20 goroutines writing identical content, 5 attempts, under `-race`) —
reproduced the original failure against the pre-fix code first, then
confirmed clean after.

#### Unbounded rename-detection diff cost — found and fixed (2026-08-25)

`patches.Diff`'s underlying LCS (`objstore/patches/diff.go`) is a
textbook O(n·m) dynamic program — a full 2D table, not just
O(min(n,m)) — in both time *and* memory. `detectRenames`
(`cmd/9vcs/rename.go`) calls it for every deleted×added path pair in a
changeset, so a changeset touching several large text files (plausible
after `import`/`reconcile` pulls one in from a peer) made an ordinary
`status`/`diff` attempt a multiplicative pile of expensive diffs the
user never directly asked for — not a remote-write exploit like the
traversal bugs above, but a real hang/OOM risk from otherwise-normal
usage. Fixed with `maxRenameDiffCells`, a bound on the candidate pair's
line-count product (25,000,000 — comfortably covers two ordinary
~5,000-line files, rejects a pathological pairing before ever attempting
it): over the bound, the pair is treated as "not a match" rather than
scored, the same tradeoff already accepted elsewhere in rename detection
(losing detection for an extreme case beats hanging on it). Verified:
`TestRenameCandidateSkipsExpensiveDiffForHugeFiles` uses a pair sized
well past the threshold and requires the call to return within 5s.

**Follow-up, same day, prompted by a user question about hashing as a
cheaper check**: the size bound above had a real gap of its own — it
rejected an oversized pair *unconditionally*, including the case where
the file was renamed with zero edits, which never needed the expensive
diff to detect in the first place. `KindBlob`/`KindSymlink` above
already skip straight to an exact-match check by hash/string equality,
with no O(n·m) cost at all; text never had that fast path, always
paying for a full diff even when byte-identical — the single most
common real case (a plain rename, no edit). Fixed by hashing both
sides' content (SHA-256, matching every other content-addressing
decision in this codebase — no reason to introduce a different
algorithm for a one-off equality check) *before* the size bound is even
consulted: an exact match short-circuits to a detected, unmodified
rename regardless of size (hashing is O(n)), and the size bound only
ever has to reject the genuinely ambiguous case — large, and not
identical, so an actual similarity score is needed. Verified:
`TestRenameCandidateExactMatchDetectedEvenWhenOversized` (a pair sized
well past the threshold, identical content, must still be detected) and
live: renaming a real 6,000-line file with no edits is now detected
correctly in 16ms.

#### Binary-conflict sidecar writes bypassed the symlink-traversal fix — found and fixed (2026-08-25)

The same bug class as "Symlink path traversal via an intermediate
component" above, in three sibling call sites the earlier fix didn't
touch: `merge`/`apply`'s binary-conflict comparison sidecar (e.g.
`logo.png.a1b2c3d4e5f6` written next to a losing side's content) and
`merge -abort`/`record`'s cleanup of it each used a plain
`filepath.Join(r.root, ...)` + `os.WriteFile`/`os.MkdirAll`/`os.Remove`
— none of it routed through `os.Root`, unlike `writeWorkingTree`
(fixed earlier for exactly this). `binaryConflictSidecar`'s path is a
legitimate join of an already-`validPath`-checked tracked path plus a
hash suffix — the string itself is fine; nothing about it says what
currently sits on disk at an intermediate path component.

Unlike the `writeWorkingTree` case, this one doesn't even need an
attacker-crafted patch to trigger: an ordinary symlinked cache/vendor
directory already sitting in the working tree, plus a completely
mundane two-sided binary conflict at a path underneath it (e.g.
`vendor/pwned.bin` — any real add/add or edit/edit binary conflict at
that path), sends the sidecar write straight through the symlink and
outside the repo. The same path is also reachable the way the earlier
bug was — a symlink introduced by an earlier, already-recorded commit,
sitting on disk by the time a later merge introduces a binary conflict
underneath it.

Fixed by adding `writeSidecarFile`/`removeSidecarFile` (`repo.go`),
both opening `r.root` via `os.OpenRoot` the same way `writeWorkingTree`
does, and switching all four call sites (`merge.go`'s sidecar write and
its abort-time removal loop, `apply.go`'s sidecar write, `record.go`'s
removal loop) to use them instead of the raw `os.*` calls.

Verified: reproduced live against the pre-fix code first — a real
symlink to a temp directory outside the repo, a genuine two-sided
`MERGE_SIDECARS` state pointing through it, and the real
`cmdMergeAbort` entrypoint (not a hand-rolled reimplementation) deleted
a file entirely outside the repo. Reverted, re-ran against the fixed
code: refused with `path escapes from parent`, victim file untouched.
Permanent regression coverage in `cmd/9vcs/sidecar_test.go`:
`TestWriteSidecarFileRefusesSymlinkPathEscape`,
`TestRemoveSidecarFileRefusesSymlinkPathEscape` (the helpers directly),
and `TestMergeAbortRefusesSidecarRemovalThroughSymlinkEscape` (the real
`cmdMergeAbort` entrypoint, proving the fix is actually wired in, not
just correct in isolation).

#### Ref-name path traversal, reachable over the network — found and fixed (2026-08-25)

A different, more severe finding from the same audit pass: ref/branch
names were never validated the way `FileChange.Path` is (see the
first entry in this section). `refPath` (`cmd/9vcs/repo.go`) does a
plain `filepath.Join(r.dir, "refs", name)`, and nothing upstream of it
— `refHash`, `casWriteRef`/`writeRefFileLocked`, `branch`/`checkout
-b` (local CLI args), or `refAdapter` (the concrete
`vcsfs.RefReader`/`RefWriter` a repo hands to `vcsfs.FS`) — ever
checked `name` for a `".."` segment.

The serious part is the network reach: `vcsfs` itself has *no* path
logic for refs at all — `dirFile.Walk`/`Create` for `kindRefs` pass
`name` straight through to `RefReader.RefHash`/`RefWriter.SetRefHash`.
And the `9p` server library performs no validation of its own on a
`Twalk`/`Tcreate` name element either (confirmed against
`server/dispatch.go`'s `tWalk`: each `Wname` element is passed
straight to `File.Walk`, no rejection of `".."` or embedded `/`). A
well-behaved client (`client.Client`, what `sync.go`/`reconcile.go`
use) splits a path into multiple small `Twalk` elements before
sending, and those get stopped early here — after resolving `refs`,
the *next* element is looked up as a ref name in its own right by
`kindRefs`'s `Walk`, not walked further as a subdirectory, so a
multi-element `../../../tmp/evil` doesn't reach far. But nothing
requires a client to split at all: the wire format's `Wname string`
field can contain embedded `/` and `..` in a single element, and nothing
server-side rejects that. A peer with only `PermWrite` — not local
filesystem access — could send one such name and get an arbitrary
`Hash.String()`-shaped value written to any path the serving process
can write to, entirely outside `.9vcs/refs`. Content is limited to a
hex hash plus a newline (`writeRefFileLocked`'s exact format), not
arbitrary bytes, so this is corruption/DoS rather than arbitrary code
execution — still a real, unauthenticated-content write to a path of
the attacker's choosing.

Fixed with `validRefName` (`cmd/9vcs/repo.go`), the same shape as
`patches.validPath` (reject empty, an absolute name, or any `".."`
segment) for the same reason — but kept separate from it rather than
exported and shared, since nested branch names are an intentional
feature here (`writeRefFileLocked`'s own `MkdirAll`) in a way that
doesn't map cleanly onto file-content paths. Wired into `refHash`
alone: `casWriteRef` already calls `refHash` before ever writing, so
one choke point covers every read and write, local and remote,
without touching every caller.

Verified: reproduced live against the pre-fix code first —
`refAdapter{r}.SetRefHash` (the literal `vcsfs.RefWriter` a real
`serve` hands to the network) with a name string built via
`filepath.Rel` to land outside the repo wrote a real file there.
Reverted, re-ran against the fix: refused with `invalid ref name`, no
file appeared. Permanent regression coverage in
`cmd/9vcs/refname_test.go`: `TestValidRefNameRejectsTraversal` /
`TestValidRefNameAllowsOrdinaryAndNestedNames` (the predicate itself,
including that legitimate nested names like `feature/foo` still work),
`TestRefHashRejectsInvalidName`,
`TestRefAdapterSetRefHashRejectsTraversalEscape` /
`TestRefAdapterRefHashRejectsTraversalEscape` (the exact network-facing
interface, both directions), and `TestSetLocalRefCASRejectsTraversalName`
(the purely-local path).

#### `HashFromHex` accepted the wrong length — hardened, not a confirmed exploit (2026-08-25)

Different in kind from the two findings above, and worth being honest
about the difference: this is a defensive-hardening fix, not a proven
vulnerability. Noticed while reading the hash-parsing code adjacent to
the ref-name fix, not from a live-reproduced attack.

`hex.DecodeString` only rejects malformed hex (bad characters, odd
length); it says nothing about the *decoded* length. `HashFromHex`
then did `copy(h[:], b)` unconditionally, so any valid-hex string
shorter than 32 bytes silently zero-padded into a real `Hash` value
with no error — for the shortest inputs (e.g. `"00"`, or even `""`),
that value is the zero hash itself, the sentinel this codebase uses
everywhere to mean "no such ref" / "no dependency" (`Hash.IsZero`'s
callers). A too-long input silently truncated the same way. Unlike
`FileChange.Path` or ref names, this alone isn't a path-traversal
vector — `Hash.String()` always re-encodes to a canonical 64-char
string, so a mis-parsed `Hash` still only ever resolves to a
normal-looking, on-tree path — and no concrete scenario turned up
where coercing to the zero-hash sentinel grants a capability the
protocol didn't already expose by legitimately sending that exact
sentinel value. Still, it's the same shape of "accepts more than it
should" that both of the findings above turned out to be exploitable
elsewhere, so it's fixed the same way: reject outright
(`len(b) != len(h)`) instead of silently coercing. Covered by
`TestHashFromHexRejectsWrongLength` / `TestHashFromHexRoundTrip` in
`objstore/patches/patch_test.go`.

#### N-way `apply`/`merge` silently dropped a binary/symlink conflict when "ours" never had the path — found and fixed (2026-08-25)

Found by a follow-up security audit after the run of fixes above, not
from a user-reported bug. `computeMerge`'s binary/symlink
conflict-detection loop (`cmd/9vcs/mergeutil.go`) anchored solely on
`idxs[0]` ("ours"): `for p, ourSt := range idxs[0] { switch ourSt.Kind
{ case KindBlob: ...; case KindSymlink: ... } }`. A path `idxs[0]` never
had at all — because "ours" never touched it, not because it was
deleted — was never even visited by this loop. If two *other* roots
(`9vcs apply <a> <b>` where neither is "ours," or a three-way `apply`
where only roots[1] and roots[2] introduce the path) both add the same
path with different blob content or different symlink targets, that's
exactly as much a real conflict as one "ours" also touched — but it
went completely undetected: `Materialize`'s plain union just silently
picked whichever patch applied later in deterministic topological
order, with no error, no `CONFLICT` line, no sidecar. Directly reachable
via `apply`'s own reason to exist (N-way, non-"ours"-anchored merges),
which this project treats as first-class, not a rare edge case.

Fixed by checking every root, not just roots[0]: for each path present
in *any* root (not just idxs[0]), find the first root in `roots` order
that has it as `KindBlob`/`KindSymlink` (the "anchor" — roots[0] when it
has the path, preserving the original "ours wins" tie-break exactly;
falling through to the next root that does have it otherwise) and check
every later root against that anchor's value, same as before. Verified
by `TestComputeMergeThreeWayBinaryConflictAbsentFromOurs`
(`cmd/9vcs/mergeutil_test.go`): roots[0] never touches the path,
roots[1] and roots[2] introduce conflicting blobs — must be flagged as
a binary conflict and keep roots[1]'s content (the first root that has
it), not whatever the union happened to pick. Fails against the
pre-fix code (empty `conflicts`, silently-picked content) and passes
against the fix.

#### `topoOrder`'s ready-queue re-sort — O(n² log n) CPU-exhaustion DoS, found and fixed (2026-08-25)

Also found by the same audit. `topoOrder` (`objstore/patches/replay.go`,
underlying `Materialize`/`History`/`Closure`/`UniqueChanges`) re-sorted
its entire `ready` queue from scratch on every pop, instead of using a
proper priority queue — `sort.Slice(ready, ...)` inside the `for
len(ready) > 0` loop. A normal repo has exactly one root patch, so
`ready` starts at size 1 and this is invisible in practice. But
`topoOrder` runs over whatever patches are transitively reachable from
the roots passed in, and those patches can arrive from a remote peer —
stored via `vcsfs.go`'s `writeFile.Close` (a connected client's `Twrite`
to `/patches`) or `cmd/9vcs/sync.go`'s `import`/`reconcile` pull. Nothing
stops an adversarial or buggy peer from constructing many
mutually-independent (zero-`Dependencies`) patches — cheap, since any
two just need to differ, e.g. by message — and getting them stored.
Once k such patches are included in any later `Closure`/`Materialize`/
`History`/`UniqueChanges` call (the served repo computing its own
closure during `reconcile`, or a local `log`/`merge`/`checkout` after
import), `ready` starts at ≈k and the loop pays O(k) pops each preceded
by an O(k log k) full re-sort — O(k² log k) total, a real CPU sink for
k in the tens of thousands. Not guarded by the connection-level
rate-limit/conn-cap in `hardening.go`, which bounds TLS-handshake cost,
not post-handshake decode/replay cost.

Fixed with a `container/heap`-backed min-heap (`hashHeap`, ordered by
the same byte-comparison tie-break topoOrder always used for
reproducibility) in place of the plain slice — O(log n) per push/pop
instead of a full re-sort, so the whole loop is O(n log n) over the
closure size. Verified by
`TestTopoOrderManyIndependentPatchesIsFast` (`objstore/patches/replay_test.go`):
20,000 mutually-independent patches must return within 5s (actual: well
under 2s with the fix; the old re-sort-per-pop approach would take far
longer at this size) — and `TestTopoOrderDeterministicTieBreak` confirms
the heap preserves the exact same ascending-hash tie-break ordering as
before.

#### Unbounded per-connection memory from concurrent open write-fids — found and fixed (2026-08-25)

Also found by the same audit. `checkWriteSize`/`MaxObjectSize` (fixed
earlier the same day, see "Unbounded write-offset allocation" above)
bound a *single* fid's buffer to 1 GiB, checked against the claimed
offset before ever allocating. But nothing bounded how many write-fids
one connection could hold open — and buffering — *concurrently*:
`dirFile.Create` (`vcsfs/vcsfs.go`) accepts any syntactically-valid
64-hex-char name under `/patches`, `/blobs`, or `/offers`; verification
only happens later, at `Close`. A client can `Tcreate` many fids in a
row, `Twrite` close to `MaxObjectSize` into each, and simply never send
`Tclunk` — each such buffer stays live in server memory for as long as
the connection does. The underlying `github.com/sandgorgon/9p` v0.5.0
server library stores fids in a plain unbounded per-connection map, with
no fid-count cap of its own (`server/conn.go`). Reachable at the
*weakest* trust tier this server has — `PermPropose`, via `/offers`,
explicitly meant for lower-trust external contributors — not just
`PermWrite`.

Fixed with a connection-wide write-buffer byte budget
(`vcsfs.writeBudget`, `vcsfs/vcsfs.go`): `Server.ConnContext`
(`cmd/9vcs/serve.go`) attaches a fresh one to each connection's base
context via the new `vcsfs.WithWriteBudget`, the same mechanism
`WithPermission` already uses for per-connection identity. Every
`writeFile`/`refFile.Write` reserves against it before growing its
buffer (same "reject before allocating" shape as `checkWriteSize`), and
`Close` releases its reservation regardless of whether finalizing
succeeded. Deliberately scoped *per-connection*, not tracked
server-wide with one shared counter: if a client disconnects without
ever clunking its fids, the per-connection `writeBudget` struct —
reachable only through that connection's now-dead state — becomes
unreachable and is garbage-collected along with it, so there's no
shared accounting that could leak upward and eventually wedge the
server for everyone (a real risk a naive server-wide counter would have
introduced, since nothing in the underlying library calls `Close` on a
connection's still-open fids when it drops — confirmed by reading
`server/conn.go`/`server.go`: no such cleanup exists there). Combined
with `-max-conns` (default 64), total server memory from this vector is
now bounded by roughly `maxConns × -max-conn-write-buffer` (new flag,
default 2 GiB — headroom for one near-`MaxObjectSize` object plus some
concurrent small writes, well under the "100 fids × ~1GiB" scenario the
audit demonstrated). `MaxObjectSize` itself was exported (was
`maxObjectSize`) so `serve.go` can reference it for this default without
duplicating the magic number. Verified by
`TestWriteBudgetRejectsExceedingConnectionTotal` (two fids, 60 bytes
each, 100-byte connection budget — the second write must be rejected
even though neither fid alone is anywhere near `MaxObjectSize`) and
`TestWriteBudgetReleasedOnClose` (closing the first fid must free its
reservation for the second).

#### `MERGE_HEAD`/`MERGE_SIDECARS` writes weren't atomic or lock-protected — found and fixed (2026-08-25)

Also found by the same audit. Every other ref/HEAD mutation in this
codebase goes through `atomicWriteFile` (temp file + rename) under
`withRefLock` (see "Ref/HEAD write atomicity and local concurrency"
above) — but `setMergeHeads`/`setMergeSidecars`/`clearMergeHeads`/
`clearMergeSidecars` (`cmd/9vcs/repo.go`) used plain `os.WriteFile`/
`os.Remove`, with no lock at all. Not remotely reachable (these paths
are local-only; `vcsfs` never touches them), but a real local-concurrency
gap: a crash mid-write could leave a truncated `MERGE_HEAD` that
`HashFromHex` then errors on for every subsequent command until
manually removed, and two concurrent local invocations (`9vcs merge` in
two terminals, or `merge` racing `apply`) could interleave their writes
after both independently pass the "no merge in progress" check. Fixed
by routing all four through `withRefLock`, and the two writers through
`atomicWriteFile`, bringing them in line with every other ref/HEAD
write. (The broader "no merge in progress" check-then-act race across
the whole merge operation — not just these two files' writes — is a
separate, harder problem not addressed here; see this writeup's
reasoning for why closing it would mean holding the ref lock across a
full replay+working-tree-write, a much bigger change than this fix's
scope.) Verified by `TestSetMergeHeadsAndSidecarsWriteAtomically`
(`cmd/9vcs/repo_test.go`): no leftover `.tmp` file after either writer,
mirroring `TestAtomicWriteFileLeavesNoTempFile`'s check of the
underlying primitive.

#### Rename detection — removed (2026-08-25)

See the Status section's "Rename detection" entry below for the full
writeup: built earlier the same day (concrete scope above), then
removed after the same audit flagged its remaining O(deleted × added)
candidate-pair cost as not worth carrying for a purely cosmetic,
display-time feature.

#### `checkWriteSize` integer overflow — a single message could crash the whole server, found and fixed (2026-08-25)

Found by a code review of the fixes above's own commit, not the original
audit — the reviewer traced it all the way to an unrecovered panic,
which is why this one got fixed immediately rather than staying a
"gap to revisit." `checkWriteSize` (`vcsfs/vcsfs.go`) checked `offset <
0`, then computed `end := offset + int64(len(p))` and checked `end >
MaxObjectSize` — but never checked `offset` alone against
`MaxObjectSize` first. A `Twrite` with `offset` near `math.MaxInt64`
makes that addition overflow and wrap around to a large *negative*
number, which is never greater than a positive limit — so the check
passed, silently accepting the write. The caller (`writeFile.Write`/
`refFile.Write`) then did `f.buf[offset:]` with that same huge offset
against a small buffer, which panics. Every 9P request runs in its own
goroutine (`server/conn.go`'s `dispatchOne`/`handle`) with no
`recover()` anywhere in `github.com/sandgorgon/9p`'s server package, so
one such write — reachable by any connected peer at even the weakest
trust tier, `PermPropose` via `/offers` — crashed the entire `9vcs
serve` process, taking down every connection, not just the offending
one. Fixed by checking `offset > MaxObjectSize` before ever computing
`end`, closing the overflow window entirely (once `offset` itself is
bounded to at most 1 GiB, adding a 9P-message-sized `len(p)` to it can't
overflow int64). Verified by `TestWriteRejectsOverflowingOffset`
(`vcsfs/vcsfs_write_test.go`) — checked directly against
`checkWriteSize`, not over the network, since the panic itself would
kill the test process rather than just fail the test.

#### N-way conflict detection still missed a cross-kind mismatch — found and fixed (2026-08-25)

Also found by the same code review, in the "N-way `apply`/`merge`
silently dropped a binary/symlink conflict" fix above — a real gap left
by that fix, not a new one introduced by it (the pre-fix code had the
same blind spot too, just less visibly). The rewritten loop picks an
"anchor" root (the first root, in order, that has the path at all) and
compares every *other* root's value against it — but only within a
`switch anchorSt.Kind` that had cases for `KindBlob`/`KindSymlink`
alone. A root whose value is present but under a *different* Kind
entirely (e.g. the anchor is text, another root replaced the same path
with a binary blob) matched neither case, so it was silently ignored —
`Materialize`'s union again just picked a side with no conflict
reported, discarding the other. Worse than a cosmetic gap:
`merge.go`/`apply.go`'s "binary" conflict handling assumes the
conflicting side really is a `KindBlob` `PathState` with a real `Blob`
hash to fetch for the comparison sidecar, so naively labeling a
text-vs-blob mismatch "binary" would have fed it a zero-value `Hash`
and failed with a confusing store-lookup error instead of a clean
conflict report.
Fixed by checking for a **kind mismatch** first, across every root that
has the path, before any same-kind value comparison — and giving it its
own conflict kind, `"type"` (`mergeConflict.Kind`), so it's never
conflated with `"binary"`/`"symlink"` and never reaches the
Blob-assuming sidecar code in `merge.go`/`apply.go` (both gained a
`case "type":` in their conflict-printing switch; the sidecar-writing
loops already only run `if c.Kind == "binary"`, so `"type"` is
automatically excluded from them, no separate change needed there).
Verified by `TestComputeMergeThreeWayTypeMismatchConflict`
(`cmd/9vcs/mergeutil_test.go`): root a introduces a path as text, root b
introduces the same path as a binary blob — must be flagged as a `type`
conflict and keep root a's text content, not whichever `Materialize`'s
union happened to pick.

#### `readCount`'s per-count bound didn't account for per-element size — hardened, allocation-amplification, in both `objstore/patches` and `bundle` (2026-08-25)

Escalated from "low severity, deliberate tradeoff, not worth fixing on
its own" (noted but deliberately left alone during the audit above) to
"fix it" after working through the actual worst case with the user:
`readCount` (`objstore/patches/patch.go`) validated a length-prefixed
count only against total bytes remaining in the reader — `n >
int64(r.Len())` — not against how many bytes one *element* of that count
actually needs. For `nOps` (each `LineOp` is ~72 bytes: a kind byte plus
four length-prefixed strings), a claim near the raw byte count remaining
could demand a `make([]LineOp, 0, nOps)` allocation up to ~72x larger
than the actual input could ever legitimately back — and Go's answer to
an allocation request that large failing is a *fatal*, unrecoverable
runtime error (`runtime: out of memory`), not a normal panic `recover()`
catches. Concretely: an attacker sends a patch object up to
`MaxObjectSize` (1 GiB, already permitted at the weakest trust tier) —
minimal headers, then a crafted `nOps` claiming close to 1 billion while
nearly the whole buffer is still "remaining" — and the server attempts a
single ~72 GB allocation, which crashes the whole `9vcs serve` process
on essentially any real machine. Same shape of bug as the write-offset
allocation fixed earlier the same day, just costing the attacker real
bytes proportional to the target allocation divided by ~72 instead of a
2-byte write with a fake offset.

Fixed by giving `readCount` a `minElemSize` parameter — the fewest bytes
any single element's encoding can possibly take (1 for a raw byte count
like a string's; `hashSize` (32) for a `Dependencies` count;
`minFileChangeSize` (59: every `FileChange` field's own minimum
encoding, including a zero-length path/symlink-target string and a
zero-op-count) for a changes count; `minLineOpSize` (33: a kind byte
plus four zero-length strings) for an ops count) — and checking `n >
int64(r.Len())/minElemSize` instead of `n > int64(r.Len())`. A count
that passes this bound is always backed by enough remaining bytes to
actually encode that many elements, even in the smallest legal case, so
the resulting allocation can never exceed what the input could
legitimately justify. `bundle/bundle.go` had an independent (not
shared) copy of the identical `readCount` helper with the identical
gap — `nPatches`/`nBlobs` bounded only against raw remaining bytes, not
each entry's own minimum size (`minPatchEntrySize` = 8, its own length
prefix; `minBlobEntrySize` = 40, a fixed Hash plus its own length
prefix) — fixed the same way for consistency, since a `.9vp` bundle file
is exactly as untrusted an input as a patch fetched over the wire.
Verified by `TestDecodeRejectsDependencyCountNotBackedByHashSize`
(`objstore/patches/encoding_test.go`) and
`TestDecodeRejectsPatchCountNotBackedByMinEntrySize`
(`bundle/bundle_test.go`): each crafts a count that the *old* bound
would have accepted (count ≤ bytes remaining) but the fixed bound
correctly rejects (not enough bytes remaining for that count at the
type's real minimum size).

### 2. Workspace = private namespace, built as a union (no staging/index)

A workspace is the union of:
- a **read-only lower layer**: patch-graph state at some point,
- a **writable delta layer**: locally new/changed content.

This is Plan 9's union-directory semantics (`bind -a`/`bind -b`) applied
at the application level: reads fall through to the lower layer when
absent from the upper, writes always land in the upper. The delta layer
*is* the staging area — there's no separate `add`/index step or format.
Snapshot/`record` = diff the delta layer against what it shadows, build
patches for the changed paths only.

Sparse and multi-workspace checkouts fall out of the same mechanism: a
workspace's lower-layer bind can be any subtree, and multiple concurrent
workspaces against one repo are just multiple private namespaces — no
`git worktree`/sparse-checkout special case needed.

#### Ignore patterns — concrete scope (2026-08-25, built)

The gap that made this necessary: `changedFiles`' scan of the delta layer
(`workingFiles`, `cmd/9vcs/repo.go`) walked every regular file under the
repo root with zero filtering, so the very first `record` in a real
working directory would sweep in editor swap files, `.DS_Store`, build
output, `node_modules` — silently, since nothing about it fails loudly.

**File and format.** `.9vcsignore`, at the repo root — deliberately
*not* under `.9vcs/` alongside `authorized-peers`/`known-peers`/`config`:
those are host-specific and never recorded, this one is meant to be
recorded and shared with the team, same as `.gitignore`. One pattern per
line; blank lines and `#`-comments ignored, matching every other flat
text file in this codebase.

**Pattern semantics, deliberately a subset of gitignore's, not the full
spec**: a line with no `/` matches at any depth (`*.log` matches
`nested/dir/debug.log`); a line containing a `/` (or starting with one)
is anchored to the repo root; a trailing `/` restricts a match to a real
ancestor directory rather than a same-named file. What's cut on purpose:
no `!`-negation (order-dependent re-inclusion is a real source of
gitignore footguns, not worth it for a first pass) and no `**` (git
added it later for a reason — plain single-segment globs via stdlib
`path.Match` cover the common cases named above without it). A malformed
pattern is a load-time error (`loadIgnore` validates every pattern via a
throwaway `path.Match` call before use), not a silently-never-matching
one.

**Where the check actually lives, and why there specifically**: not
inside `workingFiles` itself, but in `changedFiles` (`workingtree.go`),
gated on `!existed` (the path isn't in `base`, the materialized index
being compared against). This is the one property the whole design
hinges on and got a dedicated regression test for
(`TestChangedFilesNeverDropsAlreadyTrackedFile`): an ignore pattern only
ever suppresses a genuinely *new*, untracked file from being swept in —
exactly like `.gitignore` never un-tracks a file git's index already
knows about. Filtering inside `workingFiles` instead would have no way
to distinguish "new" from "already tracked," and would make adding a
pattern that happens to match an already-recorded file look like a
silent deletion the next time anyone ran `diff`/`record`. Every caller of
`changedFiles` — `record`, `diff`, and the dirty-tree check `checkout`/
`merge`/`apply`/`reconcile`'s pull path all share — gets this for free.

**A real bug found by CLI smoke testing, not the unit tests**: a bare
anchored pattern with no trailing slash (e.g. `/build`) matched only the
literal path `build`, never `build/out.bin` beneath it — so a file under
an ignored directory still leaked through. Real gitignore's rule is that
matching a directory always prunes its whole subtree regardless of
whether the pattern happened to end in `/`; the trailing `/` only
additionally *restricts* a match to landing on a directory rather than a
same-named file, it doesn't gate the pruning behavior itself. Fixed by
matching every pattern as a sliding window over path segments (anchored
patterns fix the window at position 0, unanchored patterns slide it to
any starting depth) and, when the window doesn't land on the path's
final segment, treating that as a directory match that covers everything
beneath it regardless of `dirOnly` — pinned by
`TestIgnorePatternWithoutTrailingSlashStillCoversDirectoryContents`.

Verified live with the real binary, not just `go test`: a repo with
`*.log`, `node_modules/`, and `/build` in `.9vcsignore` correctly
recorded only the tracked source and `.9vcsignore` itself, excluding a
`.log` file, a `node_modules` subtree, and `build/`'s contents; a
separate run confirmed a file tracked *before* a matching pattern
existed survived `diff` unchanged (no false deletion) and still picked
up a genuine edit afterward.

### 3. Synthesized in-memory filesystem (Plan 9 synthetic-file-server pattern)

Content under `/patches/<hash>` is real, durable, on-disk storage (it's
already immutable and content-addressed — no synthesis needed). A
separate `/view/<workspace>/...` namespace region is **computed on
open**, not stored — same pattern as Plan 9's kernel devices (`#p`,
`#e`) and userspace synthetic file servers: a `Walk`/`Open` triggers a
patch-graph replay for just that path, cached in memory keyed by
`(workspace, path, patch-set-version)`, invalidated via `Qid.version`
bumps when a patch touches that region. This is what makes patch-graph
replay viable without building a custom storage engine up front.

This same synthesis function has two consumers: `checkout` runs it once
and writes plain files to disk (the default, always-available path);
`9vcs serve --view` runs it live over 9P for anyone who explicitly wants
a mounted-style live namespace (opt-in, never required — see below).

### 4. Platform targets ruled the mounting question, not the other way around

Targets: Linux/UNIX, Windows, and iOS, all as a **CLI binary** — "usable
as a CLI in any environment" was the explicit requirement. This
eliminated every OS-level mount approach as the *default* path:

- **Rejected as core**: native `mount -t 9p` (Linux's v9fs kernel
  client) — needs root (`modprobe 9p`), Linux-only, and defaults to the
  9P2000.L dialect while this library only speaks base 9P2000 (would
  need explicit `-o version=9p2000`, unverified across kernels). No path
  on iOS or Windows at all.
- **Demoted to optional future convenience, never required**: FUSE
  (near-universal unprivileged mounting on Linux, but no iOS story, and
  Windows would need a separate WinFsp/Dokan integration).
- **Chosen baseline**: workspaces materialize as **ordinary files**,
  written by the `9vcs` CLI itself acting as its own 9P/synthesis client
  — plain `os.WriteFile`, identical on every platform, works inside
  constrained environments (e.g. a-Shell on iOS) with nothing to keep
  alive between invocations.

### 5. Local-first: 9P only appears at the process/network boundary

Local operations (`init`, `record`, `log`, `diff`, `checkout`, `branch`)
operate directly on the local on-disk object/patch store — no socket, no
daemon, no 9P at all. This is what makes single-invocation CLI use work
identically everywhere.

9P shows up only for explicit, foreground, you-asked-for-it commands:
- `9vcs serve` — runs a 9P server exposing this repo's `/patches` +
  `/refs` (+ optional `/view`) for a peer to reach. Not a background
  daemon by default; systemd/launchd/Windows-service integration is
  optional packaging, not core design.
- `9vcs import <addr>` — 9P client, pulls a copy of a ref (and whatever
  patches are missing, transitively by dependency) into the local store.
  Content addressing means no separate have/want negotiation.
- `9vcs reconcile <peer>` — exchanges only what's missing, either or both
  directions.

### 6. Peer topology: symmetric peer-to-peer, no hub concept

No designated "server" repo in the GitHub sense — any host running
`serve` is reachable by any other host's `import`/`reconcile`. A team
could informally run one long-lived `serve` as a de facto hub, but
that's a deployment choice, not something the architecture assumes or
requires. No discovery/registry service is part of the core design —
peers are addressed directly (`host:port`), same reasoning as the
no-GitHub-vocabulary decision.

### 7. Auth: pinned-fingerprint TLS, not a CA (peers may be reached over the internet)

- **Identity**: each `9vcs` install generates a long-lived Ed25519
  keypair on first use, wrapped in a minimal self-signed X.509 cert
  (required by Go's `crypto/tls` API even for a bare keypair). Stored
  under `os.UserConfigDir()/9vcs/identity.{key,cert}`, `0600`.
  Fingerprint = hash of the public key, shown via `9vcs identity show`
  for out-of-band exchange.
- **Transport**: TLS 1.3 only (`MinVersion: tls.VersionTLS13`).
  - Server: `tls.NewListener(rawListener, cfg)` in front of
    `server.Serve` works unmodified for the transport itself.
  - Client: `net.Dial` → `tls.Client` → handshake → `client.NewClient`.
- **Peer verification, no CA**: both sides set `InsecureSkipVerify: true`
  and supply a custom `VerifyPeerCertificate` doing exact fingerprint
  matching (SSH's model, not Web PKI).
  - Server: `ClientAuth: tls.RequireAnyClientCert`; checks the presented
    fingerprint against an `authorized-peers` allowlist file
    (fingerprint + `read`/`write` permission, one per line — shape of
    `authorized_keys`). Unknown fingerprint is rejected at the
    handshake, before `Attach` is ever reachable.
  - Client: checks the server's fingerprint against either an explicit
    pin (`--peer-fingerprint <hex>`) or a local `known-peers` store with
    TOFU semantics (first-connect prompt + pin; later mismatch is a loud
    refusal by default — `known_hosts` behavior).
- **Authorization needs one deliberate deviation from the obvious
  approach**: `server.Serve(l)`'s built-in accept loop doesn't expose the
  connection to the `FileSystem`, so there's no hook for verified peer
  identity to reach `Attach`. Fix: don't use `Serve(l)`; run a manual
  accept loop — accept, TLS-handshake, extract the verified fingerprint,
  construct `server.Server{FS: vcsfs.New(peerFingerprint, permission)}`
  per connection, call `.ServeConn(tlsConn)`. No library fork needed.
  `vcsfs`'s `Attach`/`File.Write` check the captured permission directly
  (reads need `read`; new patch content and `/refs/*` CAS-writes need
  `write`).
- **Internet-facing hardening**: `Server.Msize` capped; connection cap +
  per-IP rate limit ahead of the TLS handshake; `Tflush` bounds one slow
  request, a per-connection concurrent-request cap is phase-2 if needed.
  Revocation = edit the `authorized-peers` file, no CRL/OCSP machinery.
  NAT traversal/reachability is explicitly out of scope (operational
  concern, not something to build a relay for).

### 8. Change submission and review (bundles + offers, not pull requests)

"Propose a change, let someone review it, let them selectively integrate
it" is a real need even with "pull request" ruled out of scope earlier —
patch theory gives a cleaner answer than git's PR model rather than a
worse one, because the unit being reviewed is already a small,
independently-addressable patch instead of a whole-branch diff.

**Primary mechanism: signed patch bundles, fully offline, no server
needed.**

```
9vcs bundle export -o fix-parser.9vp <patch-range>   # sender (flags before the ref/hash args — see below)
9vcs bundle import fix-parser.9vp                     # recipient
```

- `export` packages the chosen patches plus their full dependency
  closure (so the bundle applies cleanly regardless of the recipient's
  exact history) and **signs it with the sender's identity keypair**.
  This matters specifically here: unlike `reconcile`, where TLS already
  authenticates the sender, a file handed over email/chat/USB has no
  built-in provenance otherwise. Signature verification at `import` time
  is what lets the recipient trust it actually came from the claimed
  fingerprint.
- `import` only adds the patch objects to local storage — it does
  **not** touch any ref. Nothing is integrated until reviewed:
  `9vcs bundle show <file>` / `9vcs diff <patch-hash>` to inspect.
- Integration is explicit and can be **selective**, patch by patch:
  `9vcs apply <patch-hash> <patch-hash> ...`. Because patches commute
  and are independently addressable, accepting a subset of a submission
  is well-defined — no manual surgery the way partially accepting a git
  PR requires.

**Optional live variant, for when a maintainer is reachable:** an
`/offers/<id>` namespace region on a running `serve`, gated by a new,
narrower permission tier — `propose` (can add patches and post an
offer) sitting between `read` and `write` (can CAS-write `/refs`
directly) in the `authorized-peers` model from Auth. Same underlying
mechanism as a bundle, just transported over an active `serve`
connection instead of a file:

```
9vcs offer <peer-addr>              # post a bundle to their live /offers
9vcs offer list <peer-addr>         # maintainer: see what's pending
9vcs offer apply <peer-addr> <id>   # maintainer: fetch + selectively apply
```

**What's deliberately left out**: no comment threads, no review UI, no
CI hook — that's the actual substance of what a hosting platform adds
beyond version control, and it's out of scope on purpose. Review
conversation happens over whatever channel delivered the bundle, which
is how patch-based review worked (Linux kernel mailing lists, etc.)
long before hosting platforms existed — cryptographically verifiable,
dependency-aware patches instead of plain-text diffs mailed around is
the actual upgrade, not a review UI.

#### Bundle export/import — concrete scope (2026-08-24, not yet built)

New top-level package `bundle/` (sibling to `identity/`, `vcsfs/`,
`synth/`), plus `cmd/9vcs/bundle.go` for the `bundle export|import|show`
subcommands. Needs no changes to `objstore/patches` or `identity` — it
only consumes their existing exported surface: `patches.Closure` (already
variadic, so an arbitrary multi-root selection unions for free),
`Store.Get`/`Put`, and `identity.Load().Key` used directly as a plain
`ed25519.PrivateKey` (signing is a transport-provenance concern, separate
from `identity`'s TLS/peer-auth one, so `bundle` calls `crypto/ed25519`
itself rather than adding a signing method to `Identity`).

**Wire format** (`.9vp` file), self-delimiting length-prefixed fields in
the same style as `patch.go`'s `Encode`/`Decode`:

```
magic "9VCB" + version byte
signerPub   (32 bytes, raw Ed25519 public key)
signature   (64 bytes, ed25519.Sign over the payload bytes below, as-read)
payload:
  message      (length-prefixed string, from -m)
  patch count  + each patch's raw Patch.Encode() output, length-prefixed
  blob count   + each blob's hash (32 bytes) + content, length-prefixed
```

The signature covers the exact payload bytes as read off the wire — no
re-encode round-trip needed to verify. Same content-addressing
philosophy as everywhere else in this design: a patch is trusted because
it hashes right; a bundle is trusted because it verifies right. No
separate hash-pinning is needed for the patches/blobs inside a verified
bundle either — `Store.Put`/`BlobStore.Put` re-derive their hash from
content on the way in regardless, same as every other write path.

**`9vcs bundle export [-m MSG] -o file.9vp <ref-or-hash>...`**: resolves
each arg the way `repo.resolveRef` already does (branch name, then
full/abbreviated patch hash), unions their closures via `patches.Closure`,
fetches the actual `Patch` objects via `store.Get`, collects any
`KindBlob` content those patches reference, signs the payload with this
install's identity key, writes the file. `-o` is required — no accidental
binary-to-terminal default. Flags come *before* the ref/hash arguments,
not after (a real bug caught by live testing, not just a style choice):
Go's `flag` package stops parsing flags at the first non-flag argument,
so `-o`/`-m` placed after a variable-length list of positional args
never actually get parsed as flags — every other multi-arg command in
this CLI already follows flags-then-positionals for the same reason.

**`9vcs bundle import <file>`**: decodes, verifies the Ed25519 signature,
`store.Put`s every patch and blob, prints the signer's fingerprint +
message + a `log`-style summary of what was added. Touches no ref, per
the mechanism above — nothing is integrated until a human reviews
(`bundle show` / `diff`) and runs `apply`.

**`9vcs bundle show <file>`**: same decode+verify+print as import, minus
the `store.Put` — pure inspection, doesn't touch local storage at all.

**Deliberately no persistent trusted-signers store**, unlike the
live-peer TOFU model (`identity.KnownPeers`): a bundle arrives over
email/chat/USB with no repeated connection to protect, so the signature's
whole job is making the printed fingerprint trustworthy for *this one*
review — the human decides whether to trust it at `apply` time, the same
way code review already requires judgment. Note the bundle signature
(who assembled and sent this file) is a different question from each
patch's existing `Author` field (who wrote the change) — a bundle can
carry patches authored by people other than its signer.

#### `apply` — concrete scope (2026-08-24, not yet built)

The open question this used to leave undecided — auto-record between
each pairwise merge, or a true N-way merge patch — is resolved by
actually reading the graph/merge code rather than guessing: **a true
N-way merge patch wins outright**, not just as the nicer alternative. The
core conflict machinery (`objstore/patches/linearize.go`'s `Linearize`/
`Resolve`) is *already* N-way, with no changes needed — a fork is just
"a node with more than one alive outgoing edge," and every place that
walks `Fork.Alternatives` already loops over however many there are, not
an assumed two (verified by reading `walkFrom`'s `candidates`-length
switch and `Resolve`'s `for _, alt := range f.Alternatives`). Only the
CLI-facing layer above that graph — `cmd/9vcs/mergeutil.go`'s
`computeMerge`, and `merge.go`/`repo.go`'s single-hash `MERGE_HEAD` — is
hardcoded to exactly two sides. So the real work is generalizing that
layer, not inventing N-way conflict detection from scratch, and the
"auto-record between clean merges" alternative stops being necessary at
all: with one real N-way merge, there's exactly one merge-in-progress
state to resolve and finalize, the same one-`record`-call UX `merge`
already has today — not N sequential ones.

**What has to change, concretely:**

- **`repo.go`'s `MERGE_HEAD` becomes a list, not a single hash** —
  `mergeHead()`/`setMergeHead(h)` become `mergeHeads()`/
  `setMergeHeads(hs)`, one hash per line. This is git's own actual
  `MERGE_HEAD` format (git has supported multiple lines for octopus
  merges since it added the feature), not a novel scheme. `merge.go`'s
  existing two-way call sites become one-element-list calls — no
  behavior change there.
- **`mergeutil.go`'s `computeMerge(r, ours, theirs)` generalizes to
  `computeMerge(r, roots ...patches.Hash)`.** The union/text-conflict
  part needs no change (`r.materialize(roots...)` is already variadic,
  and `Linearize`'s fork detection is already N-way, as above). Two
  loops are genuinely 2-way today and need rewriting as N-way:
  - **Binary conflicts**: today compares `oursIdx[p]` against
    `theirsIdx[p]` pairwise. Generalizes to: for each path, materialize
    each of the N roots individually, and conflict if more than one
    distinct blob hash appears among them (keeping the first — `ours` —
    by the same policy as today, still simple majority-free "pick a
    side, flag it" resolution).
  - **Modify/delete races**: today's `UniqueChanges(store, theirs,
    oursClosure)` asks "what did theirs uniquely do relative to ours."
    Generalizes to: for each root `i`, `UniqueChanges(store, roots[i],
    patches.Closure(store, otherRoots...))` — `patches.Closure` is
    already variadic, so "the union of everyone else's closure" is a
    direct call, not new machinery.
- **Binary-conflict sidecars** (`merge.go`'s `binaryConflictSidecar`)
  currently always writes exactly one `path.theirs` file. With N sides,
  a binary conflict can have more than one losing side to show for
  comparison — rename to include which side, e.g.
  `path.<short-hash>` per differing side, rather than a fixed
  `.theirs` suffix. `record.go`'s sidecar cleanup already iterates a
  list of paths from `mergeSidecars()`, so it needs no structural
  change, just more entries in that list.

**New `cmd/9vcs/apply.go`: `9vcs apply <patch-hash-or-ref> [<patch-hash-or-ref>...]`.**
Reuses `repo.resolveRef` for each target exactly like `merge` does (no
reason to forbid branch names, even though the expected case is patch
hashes pulled from a `bundle show`). Filters out any target already in
`ours`'s closure (reported as already-applied, not an error); if nothing
remains, "already up to date." Otherwise computes the merge across
`ours` plus whatever targets remain via the generalized `computeMerge`,
writes the working tree, calls `setMergeHeads` with the remaining
targets, and reports exactly like `cmdMerge` does today — "merged
cleanly; run `9vcs record` to finish" or the conflict list. `record.go`'s
existing finalize path (already reading `mergeHead`/sidecars generically
enough) needs updating to the list-based `mergeHeads()` API and to build
`Dependencies` from `head` plus every merge head, not just one.

**Deliberately not touched in this pass**: `cmd/9vcs merge`'s own CLI
surface stays single-target. Once `computeMerge`/`mergeHeads` are
generalized, giving `merge` the same multi-arg (octopus-merge) capability
`apply` gets is very close to free — but that's a distinct CLI decision
(does a user-facing octopus `merge` pull its weight?) from what `apply`
actually needs, so it's left for later rather than bundled in here.
`apply` needs no dependency on the `bundle` package at all — it operates
purely on patch hashes already present in the local store, however they
got there (`bundle import`, `reconcile`, or a plain `record`).

#### `/offers` live variant — concrete scope (2026-08-24, not yet built)

**The workflow this is for**, confirmed with the user before scoping the
rest: a small trusted team, membership managed entirely by the person
running `serve` (the "owner"). Onboarding a new member needs no new
mechanism — it's exactly decision #7's existing `authorized-peers` file:
the new member runs `9vcs identity show` to get their fingerprint, hands
it to the owner out of band (however a small trusted team already talks),
and the owner appends one line, `<fingerprint> propose`. Revocation is
the same file, minus a line. `PermPropose` (`identity/authorized.go`) has
sat unused since decision #7 specifically for this — its doc comment
already says "not load-bearing yet... included now so the
authorized-peers file format doesn't need a breaking change once there
is." This is that day.

What offers add over the primary bundle mechanism (decision #8's
export/import, already built): bundle export/import assumes the sender
and the maintainer are *not* both online at once — the file goes over
email/chat/USB, no server needed. Offers are for when the maintainer's
`9vcs serve` **is** reachable right now, and the question is whether a
`propose`-permission peer (no `write`, so no way to move `/refs`
themselves) has to leave the tool to submit something. Without offers,
they'd `bundle export` to a file and hand it over some entirely separate
channel, even though they already hold a live, TLS-authenticated
connection to the maintainer's server the moment they can reach it at
all. Offers close that gap: same connection, same already-established
identity, the bundle lands in a place *on the maintainer's own server*
instead of being routed around it.

**An offer is a bundle sitting in a mailbox — literally.** No new wire
format, no new crypto. `bundle.Export`/`Decode`/`Verify`/`Store`
(decision #8, already built) are reused as-is; the only thing that
changes is where the resulting bytes go. This keeps the new code surface
small: a storage location, one new `vcsfs` region, and three CLI verbs.

**Storage: no new type.** An offer is exactly the same shape as a blob —
raw bytes, content-addressed, no structure the store itself needs to
understand — so `patches.BlobStore` is reused directly rather than
inventing an `OfferStore`. `(*repo)` gains `offers *patches.BlobStore`,
opened via `patches.OpenBlobs(filepath.Join(dir, "offers"))` alongside
the existing `store`/`blobs` opens in `findRepo()`/`initRepo()`
(`.9vcs/offers`, same on-disk fan-out layout `.9vcs/blobs` already uses).

Two small additions to `objstore/patches` are needed, since `BlobStore`
today only exposes `Put`/`Get`/`Has`:
- **`rawStore.list()` / `BlobStore.List() ([]Hash, error)`**: walks the
  two-level fan-out directory and returns every stored hash. Nothing
  needs this today — `dirFile`'s doc comment in `vcsfs.go` is explicit
  that `/patches` and `/blobs` deliberately don't support enumeration,
  since content-addressed pull only ever fetches a hash it already knows.
  Offers are the opposite case: browsing the pending queue (`offer list`)
  *is* the point, so this is new, not a gap being filled.
- **`rawStore.remove(h Hash) error` / `BlobStore.Remove(h Hash) error`**:
  deletes the on-disk object for `h`. Every other content-addressed
  object in this design (patches, blobs, refs' history) is permanent by
  design — nothing removes them anywhere. An offer is different: it's
  inherently transient, pending review, and with a small team the volume
  is low but not zero — leaving handled offers to accumulate forever with
  no cleanup path felt like an oversight worth avoiding now rather than
  a problem to notice later, at effectively zero cost (`Remove` is
  already a stubbed-out method on every `vcsfs` file type today, always
  returning "not supported" — this gives it one real implementation
  instead of a fifth stub).

**`vcsfs.go` changes**, sibling to the existing `kindPatches`/
`kindBlobs`/`kindRefs` machinery:

- `FS` gains an `Offers *patches.BlobStore` field.
- New `kindOffers` dirKind, name `"offers"`; `kindRoot`'s `Walk` and
  `Read` (root directory listing) both gain an `"offers"` entry.
- `dirFile.Walk` gains a `kindOffers` case: parse `name` as a hex hash,
  `d.fs.Offers.Get(h)`, wrap in the existing `objFile` — **no new
  read-side permission check**. This was the one real design decision in
  this whole feature, resolved by reading the code rather than assuming:
  `objFile` already carries a `perm` field on every read path today, but
  it's never actually checked — `/patches`, `/blobs`, and `/refs` reads
  are gated only once, at the TLS handshake, by holding *any* authorized
  permission (`PermRead` or above). Making offers specifically
  `PermWrite`-only-to-read would be new gating logic invented for a
  privacy property nobody asked for, on top of being inconsistent with
  every other region of this filesystem. For a small trusted team,
  teammates seeing each other's pending offers isn't a leak — it's
  arguably useful (avoids two people proposing the same fix) — so offers
  are exactly as readable as everything else already is: `PermRead`+.
- `dirFile.Read`'s `kindOffers` case lists via `d.fs.Offers.List()`,
  same `p9.Stat`-per-name shape `kindRefs` already builds.
- `dirFile.Create`'s permission check currently gates every kind
  uniformly at `d.perm < identity.PermWrite`, checked once before the
  kind switch. This has to become per-kind, since offers accept the
  narrower `PermPropose`: `kindOffers` requires `PermPropose`;
  `kindPatches`/`kindBlobs`/`kindRefs` keep requiring `PermWrite`,
  unchanged. `Create`'s `kindOffers` case itself returns a `writeFile`
  exactly like `kindPatches`/`kindBlobs` do — the claimed name is the
  poster's own SHA-256 of the bundle bytes it's about to write (computed
  client-side, see below — the same "the client claims a hash, the
  server verifies it on Close" shape `pushPatch`/`pushBlobIfMissing`
  already use).
- `writeFile.Close`'s existing two-way switch (`kindBlobs` / default
  `kindPatches`) gains a third arm for `kindOffers`: decode the buffered
  bytes with `bundle.Decode`, reject on a decode failure exactly like a
  malformed patch is already rejected; check `b.Verify()` — the bundle's
  *own* signer signature — refusing an unverifiable offer outright rather
  than letting garbage sit in the queue; then `f.fs.Offers.Put(data)` and
  the same `got != f.want` hash-mismatch check every other kind already
  does. Deliberately **not** checked at this point: each inner patch's
  own `AuthorFingerprint`/`AuthorSignature`. That check already lives in
  `Bundle.Store` (decision #8), which only runs later when the maintainer
  actually calls `offer apply` — reusing it there means zero duplicated
  verification logic, and matches decision #8's existing principle that
  nothing is integrated (or even fully trusted) until a human explicitly
  acts on it. An offer sitting in the mailbox is inspectable, not yet
  trusted at the per-patch level.
- `objFile.Remove` currently returns "read-only" unconditionally for
  every kind. It gains one real branch: for `kindOffers` only, require
  `f.perm >= identity.PermWrite` and call `f.fs.Offers.Remove(h)` (`h`
  re-parsed from `f.name`, the same hex string `Walk` already validated
  building this `objFile` — no new field needed). Every other kind's
  `Remove` is untouched, still permanently read-only.

**`cmd/9vcs/offer.go`**, dispatching on `args[0]` the same style
`cmdBundle` already uses, with posting as the unlabeled default case
(matching the CLI sketch already in this section's Vocabulary layout):

```
9vcs offer [-m MSG] [-peer-fingerprint FP] <peer-addr> <ref-or-hash>...
9vcs offer list <peer-addr>
9vcs offer apply <peer-addr> <offer-id>
9vcs offer remove <peer-addr> <offer-id>
```

- **Post** (`cmdOfferPost`): resolves each `<ref-or-hash>` via
  `r.resolveRef` exactly like `bundle export` does, `dialPeer`s to
  `<peer-addr>` (reusing the same `-peer-fingerprint`-or-known-peers-TOFU
  path `import`/`reconcile` already use), calls `bundle.Export` in memory
  (no file — `-o` doesn't apply here), computes
  `id := patches.Hash(sha256.Sum256(data))`, then
  `c.Create("offers/"+id.String(), 0o644, p9.OWRITE)` /
  `Write(data)` / `Close()`, mirroring `pushPatch`'s exact shape in
  `reconcile.go`. Prints the assigned id and patch count on success.
- **List** (`cmdOfferList`): `dialPeer`, `c.Open("offers", p9.OREAD)`,
  `f.ReadDir()` (already exists in the 9p client library,
  `client.File.ReadDir`/`ReadDirContext` — confirmed by reading
  `client/file.go`, no library gap here). For each entry, fetches and
  `bundle.Decode`s it — **not** `bundle.Store`d, pure inspection, the
  same decode-without-persisting shape `bundle show` already has — and
  prints one line per offer: short id, signer fingerprint
  (`identity.Fingerprint(b.SignerPub)`), message, patch count. Touches no
  local storage at all.
- **Apply** (`cmdOfferApply`): fetches the one named offer id, decodes,
  checks `b.Verify()`, calls `b.Store(r.store, r.blobs)` — the exact same
  three calls `cmdBundleImport` already makes on a file's bytes, just
  sourced from `c.Open("offers/"+id, p9.OREAD)` instead of
  `os.ReadFile`. **Deliberately stops there** — it does not run `9vcs
  apply`'s merge itself. This is the one naming ambiguity worth being
  explicit about: PLAN's original sketch called this "fetch +
  selectively apply," which could be read either way. Resolved as
  fetch-and-store only, ending with the same "nothing integrated yet"
  message `bundle import` already prints, because (a) it reuses 100% of
  existing plumbing with zero new merge-triggering logic, and (b) an
  `apply` command that silently ran a merge and touched `MERGE_HEAD`
  without the user separately typing `9vcs apply <hash>...` would be the
  one command in this entire design that skips the explicit
  review-then-integrate split every other path (`bundle import` /
  `import` / `reconcile`) enforces. The maintainer's actual flow is:
  `offer list` to see what's pending, `offer apply <id>` to pull one
  in locally and verify it, then the ordinary `9vcs apply <hash>...` (or
  `9vcs diff`/`9vcs merge`) to actually decide what lands — same two
  deliberate acts the offline bundle path already has.
- **Remove** (`cmdOfferRemove`): `dialPeer`, `c.Attach(...)` (`*client.Fid`,
  not `*client.File` — confirmed by reading `client/fid.go`, no library
  gap here), `root.Walk("offers", id)` then `.Remove()`
  (`client/fid.go`'s `Fid.Remove`/`RemoveContext`, which issues `Tremove`
  and clunks the fid regardless of outcome). This is the first
  client-side delete `cmd/9vcs` will have issued — every other push path
  in this codebase only ever creates or writes — but the library already
  supports it, verified directly against v0.5.0's source and exercised
  live in `vcsfs`'s own `TestOfferRemove`/
  `TestOfferRemoveRequiresWritePermission`. Server-side, this reaches
  `objFile.Remove`'s new `kindOffers` branch above, gated at
  `PermWrite` — only the maintainer can clear a handled offer, matching
  the same tier that can already move `/refs`.

**Not in scope for this pass**: no notification/push when an offer
lands — `offer list` is polling, same as `reconcile`'s own
already-established style, not an event-driven addition. No offer
"status" (accepted/rejected) tracked anywhere — `remove` is the only
lifecycle transition, and it's a blunt "I'm done looking at this," not a
recorded decision. No comment thread, matching decision #8's original
"what's deliberately left out" for the whole offers concept.

## Vocabulary (deliberately not GitHub-shaped)

| Instead of | Use |
|---|---|
| clone | import |
| remote / origin | peer |
| push / pull | reconcile |
| fork (GitHub sense) | dropped — branching + import-and-diverge covers it |
| pull request | out of scope for the VCS core |
| index / staging | delta layer (implicit, no separate command) |
| commit (as a noun/verb pair with add) | record (records a patch from the current delta layer) |

## Namespace layout (9P side)

```
/patches/<hash>        # patch objects (graph ops), content-addressed, immutable, durable on disk
/refs/<name>            # small file: the current set of applied patch hashes (closer to a version
                          vector than a single commit pointer), CAS-protected on write
/rev/<ref-or-hash>/...   # historical materialized view at a point in time, read-only
/view/<workspace>/...    # optional, opt-in: live synthesized workspace view (server --view only)
/offers/<id>              # optional, opt-in: pending patch bundles awaiting maintainer review,
                          # requires the "propose" permission tier (narrower than "write")
```

## Module layout

```
9vcs/
  go.mod                  # require github.com/sandgorgon/9p, github.com/sandgorgon/9auth
  objstore/patches/        # patch graph encode+hash (SHA-256, stdlib), on-disk CAS, local-only, no network
  synth/                    # replay/materialization engine + in-memory cache, shared by
                             # checkout (write-once-to-disk) and serve --view (live over 9P)
  vcsfs/                    # server.FileSystem + server.File impl of the namespace above,
                             # including permission checks fed by peer identity (github.com/sandgorgon/9auth)
  merge/                    # conflict resolution patch construction (graph-fork ordering)
  cmd/9vcs/                 # single CLI binary: init, record, log, checkout, branch, diff,
                             # serve, import, reconcile, identity, bundle (export/import/show),
                             # apply, offer (post/list/apply)
```

Ed25519 keypair, self-signed cert, fingerprint, known-peers/authorized-peers
file handling, and TLS config construction used to live in 9vcs's own
`identity/` package; as of 2026-08-28 that's `github.com/sandgorgon/9auth`
(package `auth`) instead — see the Status entry below.

(Single `9vcs` binary rather than a separate daemon binary — consistent
with "usable as a CLI in any environment" and "no default persistent
daemon.")

## Status (as of 2026-08-23)

Local-only operation is built and working: `go.mod`, `objstore/patches`
(patch object model, on-disk CAS, per-file line graph, deterministic
DAG replay), and `cmd/9vcs` with `init`/`record`/`log`/`branch`/`diff`/
`checkout`/`merge` — including true patch-graph conflict detection
(line-level forks, binary conflicts with a comparison sidecar, and
modify/delete races), not a 3-way text diff.

The networking foundation is built too, verified end to end (two
separate simulated peers — distinct identities, real TLS 1.3 over real
loopback TCP, real 9P): `identity/` (Ed25519 keypair + self-signed
cert, fingerprint-based peer auth per decision #7, `authorized-peers`
allowlist), `vcsfs/` (Store/BlobStore/refs as a 9P `server.FileSystem`,
now read-write — `/refs` writes are CAS-protected, see below), `9vcs
serve` (the manual accept loop decision #7 calls for), `9vcs import`
(one-directional content-addressed pull, fast-forward only — refuses on
divergence).

`9vcs reconcile` — the bidirectional case, the last piece of the
original open item below — is now built too, verified end to end
against all four directions (up to date, pull, push, genuine
divergence) over the same two-peer TLS/9P setup: it classifies which
direction is safe (`sync.go`'s `classify`), pulls or pushes the closure
of missing patches/blobs accordingly, and on real divergence fetches
the missing history but leaves both refs untouched, deferring to a
local `checkout` + `merge` rather than trying to resolve or propose a
conflict over the wire — there's no working tree or human on the far
end of a reconcile to resolve one against, so this ended up simpler
than originally envisioned: no wire-level conflict-proposal protocol
was needed, just fetch-then-defer. Pushing to a ref that's the *peer's* currently
checked-out branch is refused server-side (`setRefHashCAS`, same
default as git's `receive.denyCurrentBranch=refuse`) — needed because,
unlike git, this design has no remote-tracking refs to keep a pushed-to
peer's working tree and HEAD ref from silently desyncing.

`import`/`reconcile`'s `-peer-fingerprint` is now optional, backed by a
client-side known-peers store (`identity.KnownPeers`,
`~/.config/9vcs/known-peers`) with the TOFU semantics decision #7 called
for: a genuinely new address gets an interactive first-connect trust
prompt on stderr; a known address is checked against its recorded
fingerprint exactly, silently on a match, with a loud refusal — no
prompt — on a mismatch (`identity/known_peers.go`'s doc comment has the
full rationale). An explicit `-peer-fingerprint` pin still works
unprompted and now also (re-)records that address in known-peers, which
doubles as the recovery path for a legitimate fingerprint change.
Verified end to end: decline, accept-and-remember, silent reuse on a
second call, loud refusal on a changed fingerprint with no stdin read,
and re-pinning to recover.

Resolved along the way, superseding what the original open items below
used to say:

- `client.Create`/`Client.Create` (github.com/sandgorgon/9p v0.3.0),
  `Tclunk`/`Tremove` now propagating `File.Close`'s error instead of
  discarding it (v0.4.0), and `Server.MaxConcurrentRequests` (v0.5.0,
  decision #7's per-connection concurrent-request cap) were all found
  missing while building this, specced and reported upstream, fixed
  there, and adopted here — `go.mod` is on v0.5.0. reconcile's push path
  trusts `Close`'s returned error directly as of v0.4.0; no read-back
  verification workaround was needed once it landed. See Status for the
  full v0.5.0 story, including a real deadlock the spec's own first
  draft would have introduced, caught before implementation.

- Patch encoding: `Patch.Dependencies []Hash` (a real DAG, not a single
  parent), `LineOp{Kind, ID, Prev, Next, Content}` as explicit graph
  edge operations, `FileChange.Kind` (Text/Blob/Delete) for how a path
  maps to non-text content — see `objstore/patches/graph.go`.
- Simplified-first line-graph model: decided and built. Not Pijul's
  full categorical treatment — see the `graph.go`/`linearize.go` doc
  comments for exactly where this cuts corners (single reconnect route
  through a dead node, no true multi-way concurrent-edit handling).
- Hashing: SHA-256 (`crypto/sha256`, stdlib) — not BLAKE3. Deliberately
  moved off BLAKE3 (and its `lukechampine.com/blake3` +
  `klauspost/cpuid` dependencies) to keep the dependency graph
  stdlib-only, consistent with `9p` itself being pure-Go/stdlib-only.
  `go.mod` currently has zero non-stdlib requirements.
- `9vcs serve` hardening: built and verified live. `cmd/9vcs/hardening.go`
  wraps the raw TCP listener, ahead of the TLS handshake, with a
  connection cap (`-max-conns`, default 64 — a live count via
  `sync/atomic`, decremented on `Close`) and a per-address rate limit
  (`-max-conns-per-ip-per-min`, default 30 — a fixed-window counter,
  opportunistically swept, no background goroutine). `server.Msize` is
  pinned explicitly to `p9.DefaultMsize` rather than left at the zero
  value (the library already defaults there, but now it's this
  command's own choice, not an incidental one). A per-connection
  concurrent-request cap — decision #7's "phase-2 if needed" — is now
  built too: specced as `Server.MaxConcurrentRequests` (mirrors
  `golang.org/x/net/http2.Server.MaxConcurrentStreams`) since 9vcs
  couldn't add this from outside the library — `server.Server.Serve`/
  `ServeConn` all bottom out in an unexported `conn.serve()` loop that
  spawned an unconditional goroutine per incoming request, no hook to
  throttle it, and 9P2000's own tag-multiplexing lets one connection
  have unboundedly many requests in flight regardless of a connection
  cap. The library landed it in v0.5.0 (2026-08-24), and it's a
  materially better design than the spec's own first draft proposed: an
  early draft would have had `serve()`'s read loop block synchronously
  on the semaphore, which deadlocks — a `Tflush` for an already-running
  request queued behind a second, still-waiting request could never be
  read, so the slot it would free never frees, so the connection wedges
  permanently (worse than the original problem, since there's then no
  client-driven way out). The shipped fix keeps spawning a goroutine per
  request unconditionally, moves slot acquisition into that goroutine as
  a `select` against the request's own `ctx`, and exempts `Tflush`
  entirely from the limit — verified by both reading `conn.go`'s
  `dispatchOne` directly and running the library's own
  `TestMaxConcurrentRequestsTflushReachesInFlightRequestAtCap`, the
  exact regression case, before adopting it. `go.mod` bumped to v0.5.0;
  `cmd/9vcs/serve.go` gained a new `-max-requests-per-conn` flag
  (default 16) passed straight through, verified live (ordinary
  serve/import still works under the new default). Doc comment worth
  restating since it changed what the feature actually guarantees: it
  bounds concurrent dispatch into the backend, not the number of
  goroutines or pending requests a connection can accumulate — a client
  pipelining far ahead of the server still produces one parked,
  backend-untouched goroutine per pending request until a slot frees or
  it's flushed. Bounding *that* buildup (a queue-depth cap, or an
  idle/read timeout) is explicitly out of scope for this feature, left
  as a separate concern if it ever turns out to matter.
- `synth/`: built, as its own package (matching the originally proposed
  module layout — see "Resolved" below for where it diverges), and
  wired into every `cmd/9vcs` command via a `repo.materialize` method
  that replaces direct `patches.Materialize` calls. `synth.Cache` is a
  plain, unbounded in-memory map from a roots-tuple key to the resulting
  `Index`, scoped to one `repo` value's lifetime (i.e. one CLI
  invocation): correct with zero invalidation logic, because patches are
  immutable and content-addressed, so `Materialize(store, roots...)` is
  a pure function of `roots` alone — see `synth/cache.go`'s doc comment.
  This targets a real, found-not-assumed redundancy: a single `merge`
  invocation replays `ours`, `theirs`, *and* their union in one call
  (`mergeutil.go`), which used to mean three full closure replays where
  two branches' histories overlap substantially; now the overlapping
  work happens once. `TestCacheHitNeverTouchesStore` in `synth/` proves
  a repeat call is a genuine hit (not just a correct recomputation) by
  deleting the backing patch object between calls and confirming the
  second call still succeeds. Verified live too: a clean merge, a real
  text conflict with resolution, `diff`, and `checkout` all still
  produce correct output with the cache wired in.

  What this does *not* solve: a cache scoped to one process gives
  nothing back across separate CLI invocations, so a growing repo's
  total history still costs more to replay from scratch release over
  release — the concern PLAN.md's original synth/ design actually named
  was in-memory caching for a long-running process (`serve --view`,
  still unbuilt) rather than cross-invocation persistence, which the
  CLI-first design in decision #4 explicitly avoids needing ("nothing to
  keep alive between invocations"). A disk-persisted, cross-invocation
  cache was considered and deliberately not built: it would need either
  an unproven "merge two independently-replayed graphs" primitive (risky
  — could silently break the reproducible fork-ordering guarantee
  `topoOrder`'s own doc comment calls out as required for two peers to
  materialize identical bytes) or a linear-history-only fast path that
  falls back to a full replay for any merge patch, which is a real
  future option if the per-invocation cache turns out not to be enough.
- Author identity, Tier 1 (decision #1's "Author identity — concrete
  scope," the config-file half only): built and verified live.
  `cmd/9vcs/config.go` implements the flat `key = value` file format and
  the repo-local-then-global cascade exactly as scoped, `9vcs config
  [-global] <key> [<value>]` gets/sets `user.name`/`user.email`, and
  `author()` (now `author(r *repo) (string, error)`, its one call site in
  `record.go` updated) formats `"Name <email>"` / `"Name"` / the
  unchanged OS-username fallback through a separately-tested pure
  `formatAuthor` step. Verified end to end: an unconfigured repo still
  records under the OS username exactly as before; configuring
  repo-local `user.name`/`user.email` changes only patches recorded
  *after* that point (already-recorded patches stay exactly as they
  were, immutable); an unknown key is rejected with a clear error; and
  the per-key cascade actually cascades per key, not per file — setting
  global `user.name`/`user.email` while a repo overrides only
  `user.email` locally correctly resolves `user.name` from global and
  `user.email` from the repo-local file.
- `AuthorFingerprint` + `AuthorSignature`, real per-patch signing: built
  and verified live, exactly as rescoped under decision #1 once real
  multi-user adoption became this project's explicit baseline
  assumption. `objstore/patches/patch.go`'s `Patch` carries both fields;
  `Encode`/`Decode` are a single current format behind one
  `patchFormatVersion` tripwire byte, deliberately *not* real
  multi-version dispatch — this project is pre-release with one user and
  no data anywhere whose decodability needs protecting yet, so that
  machinery (built, then removed the same day once this was clarified)
  would have bought nothing; see decision #1's subsection for the full
  "why" and the exact trigger for reintroducing it (a formal release,
  plus an actual subsequent format change — both, not either).
  `cmd/9vcs/record.go` signs every patch it builds via a new `signPatch`
  (`workingtree.go`) using this install's existing identity key,
  degrading to an unsigned patch plus a stderr warning — never a failed
  `record` — if `identity.Load()` fails, same "don't make the
  most-invoked command fragile" posture Tier 1 already established.
  `Patch.VerifyAuthorSignature` is wired into both `sync.go`'s
  `fetchPatch` and `vcsfs.go`'s write path, refused with the same
  severity as a hash mismatch on an invalid signature — but, critically,
  independent of that hash check: hash verification proves the bytes
  match what was requested, not that the claimed authorship is genuine,
  which is exactly what a relay forging a patch under someone else's
  fingerprint would otherwise get away with. `9vcs log` prints a
  `Fingerprint: ... (verified)` / `(INVALID SIGNATURE)` line via the same
  `identity.Fingerprint` call peer auth already uses. Verified live
  end-to-end over real TLS+9P between two distinct identities (not just
  unit tests): peer A records a patch, which is signed and shows
  `(verified)` in `9vcs log`; peer B `import`s it from A and the same
  patch, now materialized on a completely different machine identity,
  still independently re-verifies as `(verified)` there too — the
  authorship claim travels with the patch, not just with the transport
  that carried it, which was the entire point. Unit tests cover the
  encode/decode round trip (unsigned and signed), signature verification
  (valid, tampered-after-signing, wrong-key, unsigned), and format-byte
  rejection; `vcsfs`'s `TestPatchWriteForgedAuthorshipRejected`/
  `TestPatchWriteSignedAuthorshipAccepted` cover the same over a real 9P
  connection, confirming a forged claim is refused before it's ever
  stored under any hash.
- Import/reconcile fingerprint cross-check: built and verified live —
  the informational signal decision #1's Author identity subsection
  floated (does a fetched patch's own `AuthorFingerprint` match the
  fingerprint this connection already TLS-verified, or is the peer
  relaying someone else's patch). `dialPeer` (`sync.go`) now returns the
  verified peer fingerprint alongside the client; `importClosure` takes
  it and classifies every newly-fetched patch into one of
  authored-by-this-peer / relayed-from-elsewhere / unsigned
  (`importStats`), purely informational — it never changes whether a
  patch is accepted, independent of and additional to
  `VerifyAuthorSignature`'s forged-claim refusal. `import`/`reconcile`
  print one summary line only when something was actually fetched (a
  no-op "already up to date" run stays silent, matching the
  not-noisy-by-default stance the subsection called for). Verified live
  over real TLS+9P with four distinct identities: a direct import from
  the authoring peer correctly reports "authored by this peer"; the same
  patch relayed through a second peer (imported there first, then pushed
  onward to a third under a different ref) correctly reports "relayed
  from elsewhere" when a fourth peer pulls it from the relay.
- Bundle export/import: built and verified live, exactly as scoped under
  decision #8. New `bundle/` package (`Export`, `Decode`, `Bundle.Verify`,
  `Bundle.Store`) plus `cmd/9vcs/bundle.go`'s `export`/`show`/`import`
  subcommands. `.9vp` files are magic+version+signerPub+signature+payload,
  signed with this install's identity key over the payload bytes exactly
  as read off the wire — no re-encode round-trip needed to verify, same
  as everywhere else in this design. `export` unions the closures of
  however many `<ref-or-hash>` args are given (`patches.Closure` is
  already variadic) and pulls in any `KindBlob` content those patches
  reference; `import` verifies the bundle's own signature, then each
  patch's own `AuthorFingerprint`/`AuthorSignature` (all-or-nothing —
  see `Bundle.Store`'s doc comment: a forged claim anywhere in the
  bundle refuses the whole import before anything is persisted, a real
  gap found and closed after the fact — see Open items), then `Store`s
  every patch/blob (content-addressed, so naturally idempotent, no
  separate hash-pinning needed), and touches no ref — matching decision
  #8, nothing is integrated until a human reviews with `diff`/`show` and
  runs `merge` selectively. `apply` itself stayed out of scope, as
  originally planned. Verified live end-to-end: a sender exports two patches to a
  file, a completely separate repo/identity `show`s it (pure inspection,
  no storage touched), `import`s it (patches present locally, `log` on
  `main` still empty — no ref moved), then `diff`/`merge`s the imported
  hash to bring the content in.

  Two real bugs found and fixed via that live testing, not caught by
  unit tests alone: (1) the originally-scoped/documented command syntax
  (`bundle export <ref-or-hash>... -o file.9vp`, flags *after* the
  positional args) doesn't actually work — Go's `flag` package stops
  parsing flags at the first non-flag argument, so `-o`/`-m` placed after
  a variable-length ref/hash list are silently swallowed as extra
  positional args instead of being parsed; fixed to flags-first
  (`bundle export [-m MSG] -o file.9vp <ref-or-hash>...`), matching every
  other multi-arg command in this CLI. (2) `patches.Decode` (and the new
  `bundle.Decode`, which copied the same shape) could panic — `makeslice:
  len out of range` — on a corrupted or adversarial length-prefixed count
  (dependency count, string length, change/op count, or in bundle's case
  patch/blob count and length), because nothing validated a count against
  how many bytes were actually left before using it to size a `make()`.
  Found by literally corrupting a byte in an exported `.9vp` file and
  running `bundle show` on it. Fixed with a new `readCount` helper (in
  both `objstore/patches` and `bundle`) that bounds every such count
  against the reader's remaining length before it's used, turning a crash
  into a clean decode error — this closes a real, network-reachable
  robustness gap (`import`/`reconcile` already fed untrusted peer bytes
  through the same `patches.Decode`, and the fix now applies there too),
  not just a bundle-specific one.
- `apply`: built and verified live, exactly as scoped under decision #8
  — a true N-way merge patch, not chained pairwise merges. Confirmed by
  reading the graph code (not assumed) that `Linearize`/`Resolve` needed
  zero changes: a fork is already however-many-alternatives, not a
  hardcoded two. What did need generalizing: `repo.go`'s `MERGE_HEAD`
  is now a list (`mergeHeads`/`setMergeHeads`/`clearMergeHeads`, one hash
  per line — git's own actual octopus-merge format), `mergeutil.go`'s
  `computeMerge(r, ours, theirs)` is now `computeMerge(r, roots
  ...patches.Hash)` (binary-conflict and modify/delete detection
  rewritten to loop over N roots, reusing `patches.Closure`'s existing
  variadic union — text-conflict detection needed no change at all), and
  `binaryConflictSidecar` now names its sidecar by short hash instead of
  a fixed `.theirs` suffix, so an N-way binary conflict can show more
  than one losing side. `merge.go`'s existing two-way call sites became
  one-element-list calls with no behavior change (all of `merge`'s
  existing tests still pass unmodified). New `cmd/9vcs/apply.go`:
  `9vcs apply <patch-hash-or-ref>...` resolves and dedupes targets
  (already-applied ones are skipped, not errors), fast-forwards the
  single-target degenerate case, otherwise computes one real N-way merge
  and hands off to the same `record`-to-finish flow `merge` already has
  — no separate finalize path needed.

  Verified live end-to-end, not just unit tests: three independent
  branches applied together in one call, cleanly; the already-applied
  case correctly skipped and reported; a fast-forward case; and a
  genuine three-way *text* conflict — real content, not synthetic —
  correctly rendered with all three alternatives (`Linearize`'s N-way-ness
  holding up live, not just in isolation), resolved, and finalized.
  Unit tests (`mergeutil_test.go`) cover three-way clean/text/binary/
  modify-delete cases directly, including confirming a third,
  uninvolved root doesn't mask or alter a modify/delete race between the
  other two.

  **A real bug found by that live testing, not caught by unit tests**:
  `record.go` signed a patch *before* `Store.Put`'s internal `Normalize()`
  reordered `Dependencies`/`Changes`, so the signed bytes and the
  later-verified bytes diverged whenever there was more than one
  dependency to reorder — invisible for an ordinary two-way merge (often
  zero or one `Change`, and `Normalize` sorting a one-element
  `Dependencies` slice is a no-op), but a merge patch from `apply` always
  has several dependencies. Surfaced as `9vcs log` printing `Fingerprint:
  ... (INVALID SIGNATURE)` on an otherwise-correct, cleanly-applied
  merge. Fixed by having `signPatch` (`workingtree.go`) call
  `patch.Normalize()` itself before signing — idempotent, so `Store.Put`'s
  own later call is a harmless no-op — with a regression test
  (`objstore/patches`' `TestSigningMustHappenAfterNormalize`) pinning
  the property directly: sign only after `Normalize`, never before.

- `/offers` live variant: built and verified live, exactly as scoped —
  see decision #8's "`/offers` live variant — concrete scope" for the
  design. `objstore/patches` gained `rawStore.list`/`remove` (exposed as
  `BlobStore.List`/`Remove`); `vcsfs.go` gained the `kindOffers` region
  (an `FS.Offers *patches.BlobStore` field, absent from the namespace
  entirely when nil — no special case needed for repos that don't care
  about this); `cmd/9vcs/offer.go` implements `offer` (post) /
  `list` / `apply` / `remove`. `repo.offers` is opened at `.9vcs/offers`
  alongside `store`/`blobs`, and `cmdServe` now wires `Offers: r.offers`
  into the served `vcsfs.FS`.

  Verified live end-to-end over real TLS+9P with three distinct
  identities, not just the unit-level `vcsfs` tests
  (`TestOfferPostRoundTrip`, `TestOfferPostRequiresProposePermission`,
  `TestOfferPostInvalidSignatureRejected`, `TestOfferListing`,
  `TestOffersAbsentWhenNil`, `TestOfferRemove*`): a `propose`-permission
  peer (B) posted a real patch as a signed offer to a `write`-permission
  peer's (A) live `9vcs serve`; A — connecting to its own server as a
  client, since offers are only ever readable/manageable over 9P, there's
  no local-disk shortcut — listed it (correct id, signer fingerprint,
  verified status, patch count, message), fetched+verified+stored it via
  `offer apply` (confirmed no ref moved, per the fetch-only semantics the
  spec deliberately chose over auto-integration), then integrated the now-
  local patch with the ordinary `9vcs apply` (clean merge, the file
  actually landed in the working tree — proving the whole
  offer-to-integration pipeline connects, not just that bytes moved), and
  finally cleared the handled offer with `offer remove`, confirmed gone
  from a follow-up `offer list`. Permission boundaries held over the real
  wire in both directions the design calls for: a `read`-only third peer
  (C) could list (matching the "reads aren't specially gated" decision)
  but was refused posting and removing; the `propose`-only poster (B)
  was refused removing its own posted offer. No bugs found in this live
  pass — unlike bundle export/import and `apply`, which each surfaced a
  real bug during their live verification, this one confirmed the design
  as specced without needing a fix.
- `.9vcsignore` support: built and verified live — see decision #2's
  "Ignore patterns — concrete scope" for the full design, scope cuts
  (no `!`-negation, no `**`), and the real directory-pruning bug CLI
  smoke testing caught and fixed (a bare anchored pattern with no
  trailing slash wasn't covering its own subtree). Closes the one gap
  called out as a real blocker to starting daily use with a real working
  tree, not just a nice-to-have.
- `9vcs status`: built and verified live. No new decision needed —
  `cmd/9vcs/status.go` is a thin, one-line-per-path summary
  (`A`/`M`/`D`/`U`) over the exact same `changedFiles`
  record/diff/checkout/merge/apply already share, so it's automatically
  ignore-aware and automatically merge-aware (comparing against
  `computeMerge`'s result, not a raw `materialize(head)`, while a merge
  is in progress — the same reason `record.go`'s own `midMerge` branch
  does the same thing, otherwise a path that lost a modify/delete race
  would misreport as dirty). There's deliberately no separate staged/
  unstaged/untracked split the way git's status has — this design has no
  staging index at all (decision #2), so there's only one bucket of
  "changed" to ever report. `U` reuses the exact unresolved-conflict-
  marker check `record.go` already uses to refuse finalizing a patch.
  Verified live: a clean tree, a mix of added/modified/deleted/ignored
  paths in one working tree, and a real two-branch text conflict shown
  as `U` mid-merge, correctly downgrading to `M` once markers were
  hand-resolved and clearing to "nothing to record" once `record`
  finalized it.
- `9vcs merge -abort`: built and verified live. One implementation
  abandons a merge in progress regardless of whether `merge` or `apply`
  started it — both write the exact same `MERGE_HEAD`/`MERGE_SIDECARS`
  state (decision #8's N-way `MERGE_HEAD` generalization is what makes
  this correct without special-casing which command started it).
  Recomputes exactly what was written to the working tree via the same
  deterministic `computeMerge(head, mergeHeads...)` call `merge`/`apply`/
  `status` already make — a pure function of its roots, so it reproduces
  the prior write byte-for-byte without the abort path needing to have
  remembered it — then reverses it (`writeWorkingTree(r, merged,
  headIdx)`), removes any comparison sidecars, and clears the merge
  state. Like `git merge --abort`, discards any hand-editing done since
  the merge started — a full return to head, not a selective undo.
  Verified live: a real binary conflict (which writes a sidecar file)
  aborted cleanly, sidecar gone, working tree back to head's own content,
  `status` reporting clean immediately after.
- Rename detection: built (display-time inference only, see the
  "Unbounded rename-detection diff cost" writeup above for how it
  worked), then **removed (2026-08-25)** after a security-audit pass
  flagged its remaining gap — `detectRenames` was still O(deleted ×
  added) *candidate pairs*, each paying an unavoidable O(file size) hash
  even after `maxRenameDiffCells` bounded the per-pair diff cost; a
  changeset with many small deleted/added files (plausible from
  `import`/`reconcile` of a large foreign history) meant a plain
  `status`/`diff` could still pay a large, remotely-influenceable cost.
  The user's call: not worth carrying the ongoing cost/complexity for a
  purely cosmetic, display-time feature — a moved file now shows as a
  plain `D`/`A` pair again, exactly as it was stored before this feature
  existed and exactly as `record` always stored it regardless (this
  removal is pure deletion of `cmd/9vcs/rename.go` and its call sites in
  `status.go`/`diff.go` — no patch/graph/replay model ever depended on
  it).
- Identity extracted to a shared module (2026-08-28, closes #22): 9vcs's
  own `identity/` package is gone, replaced by `github.com/sandgorgon/9auth`
  v0.1.0 (package `auth`) — a pure-Go, zero-dependency sibling module, not
  a subpackage of `9p` (`sandgorgon/9p#2` proposed that but was closed:
  a TLS/identity/peer-trust layer never touches the 9P wire format and
  doesn't belong bolted onto the wire-protocol library). Purely
  mechanical `identity.` → `auth.` rename across every caller; API is
  identical. `auth.ConfigDir()` moved the shared identity/known-peers
  location from `~/.config/9vcs` to `~/.config/9` — `auth.Load()` copies
  an existing `~/.config/9vcs/identity.{key,cert}` forward automatically
  on first run, so 9vcs needed no migration code of its own for that.
  `cmd/9vcs/config.go`'s `globalConfigPath()` (the `user.name`/
  `user.email` file) deliberately did **not** follow that move — it has
  its own independent `userConfigDir()` pinned to `~/.config/9vcs`, since
  that file is 9vcs-specific preference data with no migration story.
  `go.mod` now requires `github.com/sandgorgon/9auth` alongside `9p`; no
  `replace` directive needed, `9auth` shipped a real tag.
- Working-tree-diff orchestration extracted to a library (2026-08-29,
  closes #26): `github.com/sandgorgon/9vcs/repo` is a new importable
  package — surfaced by the `9ed` editor project needing to open a repo,
  resolve a ref to a `patches.Index`, and compute a working-tree diff
  without shelling out to the `9vcs` binary and parsing its text output.
  Pure refactor, no CLI behavior change: `cmd/9vcs/repo.go` and the
  working-tree/ignore-matching logic in `cmd/9vcs/workingtree.go` and
  `cmd/9vcs/ignore.go` moved wholesale, with names exported (`repo.Repo`,
  `repo.Find`/`repo.Open`, `repo.ChangedFiles`, `repo.WriteWorkingTree`,
  `repo.SplitLines`/`JoinLines`, ref/HEAD/merge-state methods on `*Repo`,
  etc.); `cmd/9vcs` is now a thin consumer, importing `repo` the same way
  an external caller would. CLI-only bits (`author`/`signPatch`, which
  need `9vcs config` resolution and `9auth` signing) stayed behind in
  `cmd/9vcs`. One simplification fell out for free: renaming
  `setRefHashCAS` to `SetRefHash` made its signature match
  `vcsfs.RefWriter` exactly, so `*repo.Repo` now satisfies
  `vcsfs.RefReader`/`RefWriter` directly — the `refAdapter` wrapper type
  `cmd/9vcs/serve.go` used to need is gone, `vcsfs.FS{Refs: r}` just
  takes the repo value itself. Verified: `go build`/`go vet`/`go test
  -race ./...` all clean across every package, no skips. Verified live
  with the real binary, not just `go test`: init/record/log/status/
  branch/checkout -b/diff all produce identical output to before the
  refactor; a real three-way-observable merge conflict (two branches
  editing the same line) produced correct conflict markers, `status`
  showed `U`, and resolving + recording produced a clean merge commit
  with both dependencies listed. Also verified over real TLS+9P between
  two distinct identities (not just local): peer A `import`s peer B's
  signed history (exercising `RefReader.RefHash` and `fetchPatch`'s
  signature check over the wire against a `*repo.Repo`-backed `vcsfs.FS`
  with no adapter in front of it), and a `reconcile` push from A lands
  correctly on B (exercising the CAS `SetRefHash` write path both ways:
  refused when the target is B's own checked-out branch, and succeeding
  once B has a different branch checked out) — B's `log` shows A's patch,
  signature verified. Unblocks the companion "selective (partial) record,
  keyed by line identity" spec (#25), which depends on this library
  boundary existing.
- Selective (partial) record, keyed by line identity (2026-08-29, closes
  #25): `9vcs record` can now fold a subset of pending changes into a
  patch, leaving the rest still pending — `9vcs record -p` (darcs-style,
  prompts y/n/q per pending change, per-line for text) or
  `9vcs record --lines PATH:ID[,ID...]` / `--files PATH[,PATH...]`
  (programmatic, repeatable flags) for scripts or an external tool that
  already computed the diff itself via `repo.ChangedFiles`. Two open
  questions the issue left for the maintainer: disallowed entirely during
  an in-progress merge (same restriction whole-tree record already has —
  a partial record of an unresolved conflict has no well-defined
  meaning), and multi-file is allowed in one invocation, consistent with
  whole-tree record today.
  **Core primitive**: `repo.SelectOps(ops, selected)` narrows a
  `ChangedFiles`-produced op list down to the selected line IDs.
  A delete op needs no adjustment — `Diff` computes its Prev/Next from
  pre-existing line IDs, never from another op in the same batch, so
  `graph.go`'s `resolveAlive` chases a chain of dead nodes to whatever's
  still alive regardless of which subset of them actually died (verified
  by reasoning through `resolveAlive`'s recursion, not assumed). An
  insert is the real subtlety: consecutive new lines chain through each
  other's freshly-minted IDs (`Diff` sets each one's `Prev` to the
  previous insert's ID), so naively dropping one from the middle of that
  chain would leave a survivor pointing at an ID that was never created.
  Fixed by re-deriving each selected insert's `Prev` through a
  resolve-through-unselected map — exactly the same recurrence `Diff`
  itself uses to chain a run of inserts, just applied to the selected
  subsequence — so a non-contiguous selection (e.g. keep the 1st and 3rd
  of three new consecutive lines, drop the 2nd) still produces a valid,
  independently-replayable op list.
  **Why the unselected side needs no bookkeeping at all**: the only
  output selective record produces is the narrowed op list for the patch
  being recorded now. What's left pending is never separately computed
  or persisted — the next `ChangedFiles` call, run against the newly-
  advanced base, simply re-diffs the (untouched) working-tree content
  and regenerates exactly the still-pending ops on its own, with fresh
  IDs for any still-pending inserts (an insert's ID was never meaningful
  across two separate diffs to begin with). This is the direct payoff of
  #26's library extraction: `repo.ChangedFiles` was already the single
  source of truth for "what's pending," so selection only ever needs to
  filter its output once, not maintain a second copy of repo state.
  **Verified**: `repo.SelectOps`/`repo.Selection.Apply` unit tests cover
  independent edits, the non-contiguous insert-chain re-anchoring case
  (replayed through the real `Store`/`Materialize` pipeline, not just
  asserted on the op structs), and every rejection path (unknown path,
  lines selection on a non-text change, a path selected by both `Files`
  and `Lines`, an empty selection) — plus an end-to-end test proving the
  no-bookkeeping claim above: two independent edits to one file, only one
  selected, and a subsequent `ChangedFiles` against the new base reports
  exactly the other one as still pending, unprompted. Verified live with
  the real binary: an insert and an unrelated delete in one file, `record
  -p` answering y/n split them into two separate patches with correct
  per-patch diffs and a clean tree at the end; `--files` recorded a new
  binary file while leaving an unrelated new text file pending;
  combining `-p` with `--lines`/`--files` is refused; answering `n` to
  everything in `-p` is refused with "no changes selected" and touches
  nothing; and a real three-way merge conflict correctly refused both
  `-p` and `--files` with "selective record isn't supported during an
  in-progress merge," while ordinary whole-tree `record` after resolving
  it worked exactly as before.
- Per-path `restore` (2026-09-01): before this, discarding uncommitted
  edits had no dedicated command at all — `checkout` is whole-tree only
  and outright refuses to run while anything is dirty ("record or
  discard them first"), so the only way to get one file's original
  content back was hand-editing it back yourself. `9vcs restore
  <path>...` fixes that: each named path is written back to its state at
  head (or, mid-merge, the merged base — same `computeMerge`/materialize
  split `status`/`record`'s midMerge branch already use), independent of
  every other path's dirty state.
  **Not a design change**: reuses `r.Materialize` and
  `repo.WriteWorkingTree` as-is — no new object-model concept, no
  staging index (decision #2 still holds: nothing to "unstage"), no
  rename tracking added anywhere.
  **A path absent from base — an uncommitted addition, or one half of an
  uncommitted rename — restores to "doesn't exist" (deleted) rather than
  erroring.** This is the one place restore's behavior isn't just
  git's `restore` renamed: git distinguishes untracked-but-known
  (staged) from genuinely-untracked and refuses the latter, but 9vcs has
  no staging index for that distinction to live in — every non-ignored
  working-tree path is already implicitly pending, per decision #2. A
  path with no base entry at all (never recorded, and not ignored)
  still errors, since there's nothing — recorded or pending — to act on.
  This also makes reverting a rename fall out for free with no
  rename-specific code: naming both paths (`restore old.txt new.txt`)
  rewrites `old.txt` from base and deletes `new.txt`, exactly the same
  add/delete pair `WriteWorkingTree`'s old-vs-new diffing already handles
  for whole-tree checkout.
  **Verified**: unit tests cover a single-file edit, a two-path rename
  revert, an uncommitted addition with no base entry (deleted, not
  errored), a no-op on a path already matching base, and the error path
  for a name with neither history nor a pending change. Verified live
  with the real binary too: edit-and-restore round-tripped a file's
  exact original bytes; `mv b.txt c.txt` followed by `restore b.txt
  c.txt` left `b.txt` back with its original content and `c.txt` gone;
  restoring an unknown path printed a clean error instead of a panic or
  silent no-op.

## Open items to revisit

Every decision-#1–#8 design item this section used to track (bundle
export/import, `apply`, `/offers`, both tiers of `Patch.Author`) is
built — see Status for each. Real multi-version encoding dispatch was
deliberately *not* built — see "Release versioning — decided" above for
why (a policy commitment now, real dispatch machinery only once there's
a concrete format change to dispatch on). What's actually left:

- **iOS build: still genuinely unverified.** Checked from this Linux dev
  environment and it cannot be attempted here — not a gap in this
  codebase (neither it nor `sandgorgon/9p` uses cgo, confirmed by
  grepping both for `import "C"`), but a hard Go toolchain constraint:
  `GOOS=ios` always requires external (cgo) linking regardless of
  whether the program itself uses cgo, and satisfying that needs `clang`
  plus Apple's iOS SDK, which ships only via Xcode — no Linux-hosted iOS
  SDK exists to install. Needs an actual macOS host with Xcode to attempt
  `GOOS=ios GOARCH=arm64 CGO_ENABLED=1 go build`.
- **Windows: also unverified**, same shape of gap as iOS but lower risk —
  everything in this codebase and `sandgorgon/9p` is pure Go/stdlib-only
  with careful `filepath.FromSlash`/`ToSlash` use throughout, so it
  should build and run unmodified, but "should" isn't "verified." Needs
  an actual Windows host to confirm.
- **Required PR reviewer count is 0.** Branch protection requires a PR
  and a passing CI check, but not a second approval — reasonable while
  it's mostly one person, worth raising once there's more than one
  regular contributor so a change doesn't only ever get self-reviewed.
