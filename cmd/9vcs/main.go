// Command 9vcs is the single CLI binary for the 9vcs version control
// system. See PLAN.md for the full design.
package main

import (
	"fmt"
	"os"
)

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
	case "serve":
		err = cmdServe(os.Args[2:])
	case "import":
		err = cmdImport(os.Args[2:])
	case "reconcile":
		err = cmdReconcile(os.Args[2:])
	case "help", "-h", "--help":
		usage()
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
	fmt.Fprint(os.Stderr, `usage: 9vcs <command> [arguments]

commands:
  init                        initialize a repository in the current directory
  record -m MSG               record a patch from the current working tree changes
  log [<ref>]                 show recorded patches, most recent first
  branch [<name> [<start>]]   list branches, or create one
  checkout [-b] <name-or-hash> switch the working tree to a branch or patch
  diff [<ref>] [<ref>]        show uncommitted changes, or the difference between two points
  merge <name-or-hash>        merge a branch or patch into the current branch
  identity show               print this install's fingerprint, for out-of-band exchange
  serve <addr>                serve this repo over 9P+TLS to peers in .9vcs/authorized-peers
  import -peer-fingerprint <hex> <addr> <ref-name> [<local-name>]
                               pull a ref and its missing patches/blobs from a peer (fast-forward only)
  reconcile -peer-fingerprint <hex> <addr> <ref-name> [<local-name>]
                               sync a ref with a peer: pull if they're ahead, push if you are,
                               or fetch and defer to merge on real divergence
`)
}
