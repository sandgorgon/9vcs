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

## Module layout (proposed, not yet scaffolded)

```
9vcs/
  go.mod                  # require github.com/sandgorgon/9p
  objstore/patches/        # patch graph encode+hash (SHA-256, stdlib), on-disk CAS, local-only, no network
  synth/                    # replay/materialization engine + in-memory cache, shared by
                             # checkout (write-once-to-disk) and serve --view (live over 9P)
  vcsfs/                    # server.FileSystem + server.File impl of the namespace above,
                             # including permission checks fed by peer identity
  identity/                 # Ed25519 keypair, self-signed cert, fingerprint, known-peers/
                             # authorized-peers file handling, TLS config construction
  merge/                    # conflict resolution patch construction (graph-fork ordering)
  cmd/9vcs/                 # single CLI binary: init, record, log, checkout, branch, diff,
                             # serve, import, reconcile, identity, bundle (export/import/show),
                             # apply, offer (post/list/apply)
```

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

## Open items to revisit

- Bundle export/import and `apply` (decision #8: signed, offline patch
  exchange, plus true N-way selective integration) are both now built —
  see Status. The previously-open question on `apply` (auto-record
  between pairwise merges, or a true N-way merge patch) is resolved: a
  true N-way merge patch, once it turned out the graph/conflict
  machinery (`Linearize`/`Resolve`) was already N-way and only the
  CLI-facing `computeMerge`/`MERGE_HEAD` layer needed generalizing from
  two sides to N. The optional live `/offers` variant (decision #8,
  `propose` permission tier) is now built and verified live too — see
  Status. That closes out decision #8 entirely: bundle export/import,
  `apply`, and `/offers` are all built.
- `Patch.Author` end to end is now fully built: Tier 1 (configurable
  `user.name`/`user.email`, no wire format change) and
  `AuthorFingerprint`/`AuthorSignature` (real per-patch signing verified
  on every ingestion path) — see Status for both. Signing was originally
  scoped as deferred/informational-only ("not worth it while solo"),
  then revised the same day once the user set real multi-user adoption
  as this project's explicit baseline assumption from here on — design
  going forward should assume patches are authored by multiple real,
  independent identities and shared/relayed between them, not default to
  solo-use simplifications; see decision #1's subsection for the full
  reasoning. Real multi-version patch-encoding dispatch was also built
  that same day and then deliberately removed again — pre-release, with
  one user and no data anywhere depending on the format staying stable,
  it protected a property nothing needed yet; a single `patchFormatVersion`
  tripwire byte was kept instead. Revisit real dispatch only once *both*
  a formal release exists *and* the encoding needs to change again after
  it — not either alone. `bundle.Bundle.Store` now also verifies each
  patch's own `AuthorFingerprint`/`AuthorSignature` before persisting
  anything (all-or-nothing — a forged claim anywhere in the bundle
  refuses the whole import), independent of the bundle's own signer
  signature checked by `Bundle.Verify` — this closed a real gap where
  bundle import had been built without it. The import/reconcile
  cross-check against the TLS-verified peer's own fingerprint (floated
  as deferred UX in decision #1's Author identity subsection) is now
  built too — see Status.
- iOS build: checked from this Linux dev environment, and it cannot be
  done here — not a gap in this codebase (neither it nor
  `sandgorgon/9p` uses cgo, confirmed by grepping both for `import
  "C"`), but a hard Go toolchain constraint: `GOOS=ios` always requires
  external (cgo) linking regardless of whether the program itself uses
  cgo, and satisfying that needs `clang` plus Apple's iOS SDK, which
  ships only via Xcode — there's no Linux-hosted iOS SDK to install.
  Still genuinely unverified; just needs an actual macOS host with
  Xcode to attempt `GOOS=ios GOARCH=arm64 CGO_ENABLED=1 go build`.
