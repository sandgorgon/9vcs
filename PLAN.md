# 9vcs — plan (as of 2026-09-04)

A version control system built on `github.com/sandgorgon/9p` (a pure-Go,
stdlib-only 9P2000 client/server library), leaning into Plan 9's actual
concepts (namespaces, union composition, synthetic file servers) rather
than treating 9P as just a transport. Implementation language: Go only,
throughout. No GitHub-shaped vocabulary (no clone/remote/push/pull/fork/PR).

This document holds the architecture decisions and the open backlog only.
For what shipped when, and the full detail behind every bug found and
fixed along the way, see [CHANGELOG.md](CHANGELOG.md).

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

**Author identity.** `Patch.Author` is a configurable `"Name <email>"`
string, resolved via `.9vcs/config` (repo-local) → `~/.config/9vcs/config`
(global) → OS-username fallback, set via `9vcs config [-global]
user.name|user.email`. Every patch is also opportunistically
Ed25519-signed (`AuthorFingerprint`/`AuthorSignature`, via
`github.com/sandgorgon/9auth`) since the project assumes real multi-user
adoption from the outset (see the project's multiuser-goal note) —
signing is what lets authorship survive being relayed through more than
one hop, verified independently of transport on every patch ingested from
outside the process (`import`/`reconcile`, a served write, a bundle
import). An unsigned patch (identity unavailable) is still accepted; a
forged signature is refused with the same severity as a hash mismatch.

**File mode and symlinks.** `FileChange`/`PathState` track an
`Executable` bit (meaningful for text/blob changes) and a `KindSymlink`
(target stored as a plain string, not blob-addressed) alongside
`KindText`/`KindBlob`/`KindDelete` — so a `chmod +x` or a symlink is a
real, trackable change.

**Patch format versioning (policy).** `Patch.Encode()` leads with a
version byte that `Decode` checks and refuses to misparse past. This is a
policy commitment, not a mechanism: single format until both a formal
release exists *and* there's a real need to change it again — real
multi-version dispatch is deliberately deferred until that trigger, not
built speculatively now. The permanent "format N stays decodable" promise
is a `v1.0.0` decision, not a `v0.x` one.

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

**Ignore patterns.** `.9vcsignore` at the repo root — deliberately *not*
under `.9vcs/` alongside `authorized-peers`/`known-peers`/`config`: those
are host-specific and never recorded, this one is meant to be recorded
and shared with the team, same as `.gitignore`. One gitignore-subset
pattern per line: a line with no `/` matches at any depth, a line
containing (or starting with) a `/` is anchored to the repo root, a
trailing `/` restricts a match to a directory. No `!`-negation and no
`**`, deliberately — plain single-segment globs cover the common cases
without the extra complexity. Only ever suppresses a genuinely *new*,
untracked path from being swept into `changedFiles`; never hides an
already-tracked file.

**No rename detection.** Considered, built, then removed — a moved file
records as a plain delete+add pair, exactly as it's stored. Display-time
rename inference wasn't worth its ongoing cost/complexity; see
CHANGELOG.md for the full reasoning.

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

### 9. 9sh-native root resolution: namespace-first, storage backend-agnostic

**Status: fully implemented** — patch/blob/ref reads and writes, and
working-tree materialization (checkout, including symlinks), all work
over a namespace-resolved (`p9fs`) root exactly as they do locally.

`9vcs`'s implicit, no-argument root resolution (`repo.Find()`,
`repo/repo.go`) is intentionally untouched: it stays exactly
`os.Getwd()` plus a walk-up looking for `.9vcs`, since 9sh's own `cd`
already threads a real OS `cmd.Dir` through to a launched subprocess
correctly (`9sh/kyu/eval/cd.go`, `9sh/job/job.go`) for anything that's a
real directory. What was missing was a way to hand `9vcs` a location
that *isn't* reachable as a real OS path at all — a peer's repo bound
under a future `/n/<host>`, e.g. — which is what this decision adds, as
a new, explicit `-C <path>` flag (`cmd/9vcs/main.go`,
`cmd/9vcs/rootflag.go`), the same generic-UNIX convention as git's own
`-C` (not GitHub-shaped vocabulary).

**Discovery signal.** `$_9SH_UNIX_SOCK` is the entire contract — when
`9sh` is started with `-listen-unix <path>`, it does a plain
`os.Setenv("_9SH_UNIX_SOCK", listenUnixPath)` after starting to serve
its whole namespace (`/jobs`, `/local`, `/env`, `/config`, `/session`,
future `/n/<host>`) over that socket as 9P2000, via
`github.com/sandgorgon/9p`'s `client`/`server` packages — the same
library `9vcs import`/`reconcile`/`serve` already use, not a second
protocol.

**Root resolution algorithm** (`repo.ResolveRoot`, `repo/rootresolve.go`),
reached only when `-C` is given (`repo.FindAt`) — never by the implicit
no-argument case:

1. `$_9SH_UNIX_SOCK` unset, or dialing it fails → step 3.
2. Attach, then walk: a relative `-C` argument is rooted under `local`
   (9sh's real-launch-directory bind, `dirfs.New(cwd)`); an absolute
   argument is walked as given, which may resolve to anything else 9sh
   has bound. Walk succeeds → the `p9fs` backend, rooted at the walked
   path.
3. `-C`'s argument resolved as a literal OS path, exactly like today's
   `os.Getwd()`-relative behavior.

**Storage is backend-agnostic through one seam** (new package `fsx`,
`fsx/fsx.go`+`osfs.go`+`p9fs.go`): `objstore/patches` and `repo.Repo`
call `fsx.FS` (`ReadFile`/`ReadDir`/`Stat`/`MkdirAll`/`Lock`/`Put`/
`WriteAtomic`/`Remove`/`Join`/`Dir`/`IsLocal`) instead of `os`/
`filepath` directly, with two implementations: `osfs` (today's
behavior, unchanged — every existing test passes against it verbatim)
and `p9fs` (wraps a dialed `*client.Client`).

**Two real gaps in `github.com/sandgorgon/9p` turned up during
implementation, both filed and both fixed in v0.8.0:**
- [Issue #8](https://github.com/sandgorgon/9p/issues/8) — `client.File`
  (what `Client.Open`/`Create` actually return) exposed no way to
  rename or remove a file it held; `WStat`/`Remove` existed only on the
  lower-level `*client.Fid`, unreachable from `File`'s unexported `fid`
  field. Fixed: `File.Rename`/`RenameContext` and `File.Remove`/
  `RemoveContext`, wrapping `Fid.WStat`/`Fid.Remove`.
- [Issue #9](https://github.com/sandgorgon/9p/issues/9) — `examples/dirfs`
  (what backs 9sh's real `/local` binding in production, not just a
  docs example) confined paths via a string comparison
  (`within()`) on the *intended* path, not per-segment resolution — a
  symlink planted at an intermediate path component could still be
  followed out of the exported root at syscall time, the same bug
  class `9vcs`'s own `WriteSidecarFile`/`WriteWorkingTree` had already
  been fixed for using `os.Root`. Fixed: `dirfs` now resolves every
  path through an `os.Root` opened on the exported directory.

Re-derived while investigating Issue #8: `9vcs`'s actual ref-write CAS
guarantee never came from the server — it comes entirely from
`withRefLock`'s client-side exclusive-create lock file (pure mutual
exclusion; `fsx.FS.Lock`). Since `Lock`/`Put`/`WriteAtomic` all work
identically over `p9fs` post-v0.8.0 as they do over `osfs`, there was
never a "confirm the remote is actually `vcsfs`" trust question to
design — the earlier draft of this section assumed one and was wrong;
see `repo.TestConcurrentSetLocalRefCASOnlyOneWinsOverP9FS`
(`repo/rootresolve_test.go`) for the mutual-exclusion property verified
live over a real 9P connection.

**Working-tree materialization (Phase 3) needed a real wire-protocol
gap closed, not just a client-API one.** Base 9P2000 has no symlink
representation at all — no `QTSYMLINK` Qid type, no `DM` mode bit,
nothing — so a rewritten, `fsx`-based `WriteWorkingTree` still couldn't
create a symlink over `p9fs`, and worse, couldn't even safely *scan* an
existing tree: `dirfs`'s directory listing had no way to report an
existing symlink as one, and `Open`ing it (pre-fix) transparently
followed it, meaning a scan could silently misattribute a symlink
target's content to the tracked path. That's a correctness gap in what
a scan *reads*, not just a missing write capability, so work paused
here rather than shipping a partial fix.

Filed as a design proposal (not a specific fix, since it's a real
wire-format decision) — landed as
[9p v0.9.0](https://github.com/sandgorgon/9p/blob/master/CHANGELOG.md#090):
optional 9P2000.u support. `client.WithUnixExtensions()` negotiates it
at dial time, falling back to plain 9P2000 gracefully if the server
doesn't support it; `Qid`/`Mode`'s symlink bits are core 9P2000 fields
(sent regardless of negotiation), while `Stat.Extension` (the symlink's
target) is .u-only. `dirfs` (and `memfs`) implement the new
`server.SymlinkFile` interface and correctly reject `Open` on a
symlink itself (a well-behaved client `Stat`s/`Walk`s one, never opens
it) — closing the scan-side risk, not just the create-side one.

**A second, separate seam, not a `fsx.FS` extension:** `fsx.Tree`
(`fsx/tree.go`+`ostree.go`+`p9tree.go`) is confined to one working-tree
root and symlink-aware (`Lstat`/`ReadDir`/`ReadFile`/`WriteFile`/
`Symlink`/`MkdirAll`/`Remove`), kept deliberately apart from `fsx.FS`:
refs/patches paths are entirely 9vcs-internal (content hashes,
validated ref names) and never needed root confinement or symlink
awareness; working-tree paths are arbitrary, peer-supplied tracked
content, exactly the shape of thing that already caused one real,
live, fixed vulnerability (see `WriteWorkingTree`'s own doc comment).
Extending `FS` for everyone rather than adding a second, narrower seam
would have meant either changing `FS`'s existing, already-shipped
behavior (risking Phase 1/2's tested correctness) or leaving refs/
patches carrying complexity they never needed. `osTree` wraps `os.Root`
(as `WriteWorkingTree`/`WriteSidecarFile` already did directly, before
this); `p9Tree` walks via `Fid.Walk`+`Stat` rather than `Client.Open`,
specifically because `Open` now (correctly) refuses a symlink target.
`repo.Repo` gained a `Tree` field alongside `FS`; `WriteWorkingTree`/
`ChangedFiles`/`WorkingFiles`/`WriteSidecarFile`/`RemoveSidecarFile`
and `cmd/9vcs/record.go`'s modify/delete-conflict read path all now go
through it instead of `os`/`filepath`/`os.OpenRoot` directly — the
last of those (`record.go`) was a second, separately-discovered
instance of the same "an `r.Root` string isn't a real OS path for a
`p9fs`-backed repo" risk `WriteSidecarFile` was already fixed for.

**One residual limitation, worth being explicit about rather than
implying full parity:** `p9Tree`'s root is a client-side naming
convention, not an OS-enforced boundary — the actual confinement is
entirely `dirfs`'s own `os.Root`, scoped to wherever `9sh` bound the
namespace region being walked (typically a whole launch directory via
`/local`, not the specific repo within it). An intermediate symlink in
one repo's tracked content could still redirect a `p9Tree` write to a
*different* repo or path under that same bind — never outside it
(that's what the `dirfs` fix actually closed), but not confined to
just the repo being checked out either. Closing that fully needs
either a client-side resolved-path check this library has no reliable
way to make (9P's `Qid` is an opaque per-server identifier, not a path
the server hands back) or `9sh` binding namespace regions at repo
granularity instead of a whole directory — flagged here, not solved,
for whoever picks it up next.

Verified end-to-end, over a real 9P connection: `repo.TestFindAtResolvesThroughNamespace`
(`repo/rootresolve_test.go`) checks out a text file, an executable, and
a symlink through `p9fs`, confirms the real bytes/mode/symlink-target
on disk, and confirms `ChangedFiles` reads that same tree back
correctly (including recognizing the symlink and executable bit rather
than reading through them); `fsx.TestTreeRefusesIntermediateSymlinkEscape`
(`fsx/tree_test.go`) replays the original live vulnerability against
both `osTree` and `p9Tree` directly.

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
  bundle/                   # signed .9vp export/import (Export, Decode, Bundle.Verify/Store)
  repo/                     # importable working-tree/ref/diff library (repo.Repo, ChangedFiles,
                             # WriteWorkingTree, SelectOps, ...) — extracted so external tools
                             # (e.g. the 9ed editor) can open a repo without shelling out to the
                             # 9vcs binary
  cmd/9vcs/                 # single CLI binary: init, record, log, checkout, branch, diff, status,
                             # merge (+ -abort), apply, restore, serve, import, reconcile, identity,
                             # bundle (export/import/show), offer (post/list/apply/remove), config
                             # — conflict resolution (merge.go/mergeutil.go) and CLI-only bits
                             # (author/signPatch) live here, thin on top of repo/
```

Ed25519 keypair, self-signed cert, fingerprint, known-peers/authorized-peers
file handling, and TLS config construction used to live in 9vcs's own
`identity/` package; that's now `github.com/sandgorgon/9auth` (package
`auth`) instead — see CHANGELOG.md for the migration details.

(Single `9vcs` binary rather than a separate daemon binary — consistent
with "usable as a CLI in any environment" and "no default persistent
daemon.")

## Open items to revisit

Every decision-#1–#9 design item is built — see CHANGELOG.md for the
release-by-release history of how each landed. Patch/bundle format
versioning stays a policy commitment, not built dispatch machinery — see
decision #1's patch format versioning note above for why. What's
actually left:

- **Decision #9's `p9Tree` root-confinement caveat**: a `p9fs`-backed
  working-tree write is only confined to wherever `9sh` bound the
  namespace region (typically a whole launch directory), not to the
  specific repo within it — see decision #9's own writeup for the full
  detail and why it isn't fixable from this codebase alone.
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
