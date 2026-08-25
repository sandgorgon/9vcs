// Command 9vcs is the single CLI binary for the 9vcs version control
// system. See PLAN.md for the full design.
package main

import (
	"fmt"
	"os"
)

// version is overridden at build time via
// -ldflags "-X main.version=vX.Y.Z" (see .github/workflows/release.yml).
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit(os.Args[2:])
	case "record":
		err = cmdRecord(os.Args[2:])
	case "log":
		err = cmdLog(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "branch":
		err = cmdBranch(os.Args[2:])
	case "checkout":
		err = cmdCheckout(os.Args[2:])
	case "diff":
		err = cmdDiff(os.Args[2:])
	case "merge":
		err = cmdMerge(os.Args[2:])
	case "identity":
		err = cmdIdentity(os.Args[2:])
	case "config":
		err = cmdConfig(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "import":
		err = cmdImport(os.Args[2:])
	case "reconcile":
		err = cmdReconcile(os.Args[2:])
	case "bundle":
		err = cmdBundle(os.Args[2:])
	case "apply":
		err = cmdApply(os.Args[2:])
	case "offer":
		err = cmdOffer(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	case "version", "-v", "--version":
		fmt.Println("9vcs " + version)
		return
	default:
		fmt.Fprintf(os.Stderr, "9vcs: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "9vcs: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: 9vcs <command> [arguments]  (9vcs %s)\n", version)
	fmt.Fprint(os.Stderr, `
commands:
  init                        initialize a repository in the current directory
  record -m MSG               record a patch from the current working tree changes
  log [<ref>]                 show recorded patches, most recent first
  status                       one line per changed path (A/M/D/U) — what's dirty, not the diff itself
  branch [<name> [<start>]]   list branches, or create one
  checkout [-b] <name-or-hash> switch the working tree to a branch or patch
  diff [<ref>] [<ref>]        show uncommitted changes, or the difference between two points
  merge <name-or-hash>        merge a branch or patch into the current branch
  merge -abort                 abandon a merge or apply in progress, restoring the working tree to head
  identity show               print this install's fingerprint, for out-of-band exchange
  config [-global] <key> [<value>]
                               get or set user.name / user.email, used as the patch Author;
                               writes to this repo by default, or -global for every repo here
  serve <addr>                serve this repo over 9P+TLS to peers in .9vcs/authorized-peers
  import [-peer-fingerprint <hex>] <addr> <ref-name> [<local-name>]
                               pull a ref and its missing patches/blobs from a peer (fast-forward only)
  reconcile [-peer-fingerprint <hex>] <addr> <ref-name> [<local-name>]
                               sync a ref with a peer: pull if they're ahead, push if you are,
                               or fetch and defer to merge on real divergence
  bundle export [-m MSG] -o FILE <ref-or-hash>...
                               export patches (and their full dependency closure) to a signed,
                               offline .9vp file — flags before the ref/hash arguments
  bundle show <file>           inspect a bundle's signer, message, and patches without storing anything
  bundle import <file>         verify a bundle's signature and add its patches/blobs locally;
                               touches no ref — review with diff/merge before it's integrated
  apply <patch-hash-or-ref>... integrate one or more specific patches into the current branch
                               in a single merge (merge's N-way sibling) — run record to finish
  offer [-m MSG] <addr> <ref-or-hash>...
                               post a signed bundle to a live peer's /offers (needs propose
                               permission there) — flags before the addr/ref-or-hash arguments
  offer list <addr>            show a peer's pending offers: signer, message, patch count
  offer apply <addr> <id>      fetch + verify + store one offer locally (needs write permission
                               on the peer); touches no ref — review, then run 9vcs apply <hash>
  offer remove <addr> <id>     clear a handled offer from a peer's queue (needs write permission)
  version                      print the 9vcs version
  help                         show this message

-peer-fingerprint pins the expected peer explicitly for one call; omit it to use
this install's known-peers store instead (~/.config/9vcs/known-peers on Linux),
which prompts to trust a peer the first time and refuses silently thereafter if
its fingerprint ever changes. Either path remembers the peer for next time.
`)
}
