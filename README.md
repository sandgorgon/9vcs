# 9vcs

A version control system built on [9P](https://en.wikipedia.org/wiki/9P_(protocol))
(via [`github.com/sandgorgon/9p`](https://github.com/sandgorgon/9p)), for a
small trusted team that wants real version control without a hosting
platform. History is a set of content-addressed **patches**, not
snapshots — see [PLAN.md](PLAN.md) if you want the full design rationale.
This document is the practical "how do I actually use it" guide.

No GitHub-shaped vocabulary here: there's no clone/push/pull/fork/PR.
See [Vocabulary](#vocabulary-cheat-sheet) below for the equivalents.

## Requirements

- Go 1.26.5 or later.
- That's it — `9vcs` and the `9p` library it's built on are pure Go,
  stdlib-only. No daemon, no database, no external services.

## Build

```
go build -o 9vcs ./cmd/9vcs
```

Put the resulting binary somewhere on your `PATH`. Every command below
assumes it's just called `9vcs`.

## Quickstart: working alone

```
mkdir myproject && cd myproject
9vcs init
echo "hello" > main.go
9vcs status                    # A  main.go
9vcs record -m "initial commit"
9vcs status                    # nothing to record, working tree clean
```

- `9vcs status` — one line per changed path: `A` (new), `M` (modified),
  `D` (deleted), `U` (unresolved conflict), `R`/`R+` (renamed, plain or
  with an edit — see [Renames](#renames) below). Doesn't show the diff
  itself, just what's dirty.
- `9vcs diff` — the actual line-level diff of uncommitted changes.
  `9vcs diff <ref>` diffs against some other point; `9vcs diff <ref>
  <ref>` diffs two points directly, no working tree involved.
- `9vcs log [<ref>]` — recorded patches, most recent first, each with its
  author, fingerprint/signature status, and the paths it touched.

There's no separate staging/index step (no `git add`) — the working tree
itself *is* the staging area. `record` diffs it against the current head
and builds a patch from whatever's different.

### Branches

```
9vcs branch                    # list
9vcs branch feature-x          # create, starting from HEAD
9vcs branch feature-z main     # create, starting from another branch/hash
9vcs checkout feature-x        # switch
9vcs checkout -b feature-y     # create and switch in one step
```

### Merging

```
9vcs merge feature-x
```

Conflicts are real patch-graph conflicts (line-level forks, binary
conflicts with a comparison sidecar file, modify/delete races) — not a
shallow three-way text diff. A clean merge just needs `9vcs record` to
finish. A conflicted one leaves inline `<<<<<<<`/`=======`/`>>>>>>>`
markers in the affected files (binary conflicts get a
`path.<short-hash>` sidecar instead, for comparison) — edit them, then
`9vcs record` to finish.

Changed your mind mid-merge?

```
9vcs merge -abort
```

Restores the working tree to exactly what it was before the merge
started (discarding any hand-editing you'd done on the conflict
markers) and clears the merge state. Works the same whether `merge` or
`9vcs apply` (see below) started it.

### Renames

`9vcs status`/`9vcs diff` detect renames automatically — no `9vcs mv`
needed, just move the file with your normal tools (`mv`, your editor,
whatever) and record as usual:

```
mv old_name.go new_name.go
9vcs status                    # R  old_name.go -> new_name.go
9vcs record -m "rename old_name.go"
```

If the content also changed, it shows as `R+` in status and `diff`
prints the actual line-level diff against the old content (not a
misleading "whole file added"). This is display-only, matching how git
itself works: nothing about the underlying patch changes — it's still
stored as a delete plus a fresh insert, just recognized and presented as
a rename.

## Ignoring files

Create `.9vcsignore` at the repo root — same idea as `.gitignore`, and
(unlike your identity or peer files) it's meant to be recorded and
shared with the team:

```
# build artifacts and editor junk
*.log
node_modules/
/build
```

One pattern per line; blank lines and `#` comments are skipped. A
pattern with no `/` matches at any depth (`*.log` matches
`nested/dir/debug.log`); a pattern containing `/` is anchored to the
repo root; a trailing `/` restricts a match to a real directory (so
`node_modules/` won't accidentally hide a plain file literally named
`node_modules`). Not supported: `!`-negation and `**` — see
[PLAN.md](PLAN.md) if you want the reasoning.

Ignore patterns only ever keep a **new** file out of `record`/`diff` —
adding a pattern later never makes an already-recorded file look
deleted.

## Working with a team

Every install has its own long-lived identity (an Ed25519 keypair),
generated automatically on first use:

```
9vcs identity show
# 41e05085b9336336daf8f706de278d653c65a8028127e8e3d19070c9bfd87377
```

That fingerprint is what you hand a teammate (or they hand you) out of
band — however your team already talks — so they can authorize you, or
you them.

### Serving a repo

Whoever's hosting the shared history for now runs:

```
9vcs serve :4921
```

This reads `.9vcs/authorized-peers` in the current repo — one
`<fingerprint> <permission>` line per authorized teammate, permission
being `read`, `propose`, or `write`:

```
41e05085b9336336daf8f706de278d653c65a8028127e8e3d19070c9bfd87377 write
dc611bf0985a7df6680f83479f15244d91025f4392575b392d754f32cec5fdba propose
```

- `read` — pull history, nothing else.
- `propose` — `read`, plus post a signed bundle to `/offers` (see
  [Proposing a change without write access](#proposing-a-change-without-write-access)).
- `write` — everything, including moving a branch ref directly.

There's no separate "add a collaborator" command — it's just editing
this file. Removing a line revokes access immediately.

`serve` runs in the foreground, for as long as you want it reachable —
there's no background daemon. For a small team this usually means
running it in a terminal (or `tmux`/tmux-like session) whenever people
need to sync, not something that has to run 24/7.

### Onboarding a new teammate

They start from nothing:

```
mkdir myproject && cd myproject
9vcs init
9vcs import -peer-fingerprint <your-fingerprint> your-host:4921 main
```

That pulls the `main` branch and every patch/blob it transitively
depends on, and — since `main` is the branch a fresh `init` already has
checked out — writes the actual files to disk. `import` is
fast-forward-only; if their local branch has already diverged, it tells
you to use `reconcile` or a local `merge` instead.

### Staying in sync

```
9vcs reconcile your-host:4921 main
```

Pulls if the peer's ahead, pushes if you are, or — on genuine
divergence — fetches what's missing and tells you to `checkout` +
`merge` locally rather than trying to resolve anything over the wire.

Both `import` and `reconcile` take `-peer-fingerprint <hex>` to pin a
peer explicitly. Omit it and they fall back to a local trust-on-first-use
store (`~/.config/9vcs/known-peers`): a genuinely new address prompts you
to confirm its fingerprint once; a known address is checked silently
after that, and a fingerprint that suddenly changes is a loud refusal,
never a silent reconnect — that's what catches a real impersonation
attempt versus a peer that legitimately regenerated its identity.

### Offline change exchange (bundles)

No server needs to be running for this — hand a file over email, chat,
USB, whatever:

```
# sender
9vcs bundle export -m "fix the parser" -o fix.9vp mybranch

# recipient
9vcs bundle show fix.9vp        # inspect first — signer, message, patches
9vcs bundle import fix.9vp      # verify signature, store locally — touches no ref
9vcs diff <patch-hash>          # review
9vcs apply <patch-hash>         # actually bring it in, then `9vcs record`
```

`bundle import` never moves a ref by itself — nothing lands until you
explicitly review and `apply`.

### Proposing a change without write access

If you only have `propose` permission on a peer's served repo, you can
still submit something directly to them without a side channel:

```
9vcs offer -m "please take this" their-host:4921 mybranch
```

The maintainer checks what's pending, pulls one in to review, and clears
it once handled:

```
9vcs offer list their-own-served-addr        # what's pending
9vcs offer apply their-own-served-addr <id>   # fetch + verify + store locally
9vcs apply <patch-hash>                        # actually integrate it, same as a bundle
9vcs offer remove their-own-served-addr <id>  # clear it from the queue
```

Note the maintainer runs `offer list`/`apply`/`remove` *against their own
running `serve`* — offers only exist inside that live namespace, there's
no local-disk shortcut to inspect them directly.

## Recovering from a mistake

- **Merge/apply went sideways**: `9vcs merge -abort`.
- **Uncommitted changes are in the way** of a `checkout`/`merge`/`apply`
  you wanted to run: `9vcs record` them, or discard by hand
  (`9vcs diff` to see exactly what would be lost first).
- **A peer's fingerprint suddenly doesn't match**: don't blindly trust
  it. If you're sure the change is legitimate (they reinstalled, lost
  their key, etc.), re-pin with `-peer-fingerprint <new-hex>` on your
  next `import`/`reconcile` call.

## Vocabulary cheat sheet

| Instead of | Use |
|---|---|
| `git clone` | `9vcs import` |
| remote / origin | peer (a `host:port` you connect to directly) |
| `git push` / `git pull` | `9vcs reconcile` (or `import` for one-directional) |
| fork (GitHub sense) | branch + `import` against a diverged history |
| pull request | `9vcs bundle`/`9vcs offer` — see above |
| staging / `git add` | doesn't exist — the working tree is the staging area |
| `git commit` | `9vcs record` |

## Full command reference

Run `9vcs help` for the complete, current list with exact flags — this
document sticks to walkthroughs so it doesn't drift out of sync with
that.

## Learn more

[PLAN.md](PLAN.md) has the full design: why patches instead of
snapshots, the union-filesystem workspace model, the peer-to-peer trust
model, and the reasoning behind every scope decision (including what was
deliberately left out and why).
