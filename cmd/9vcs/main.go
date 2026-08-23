// Command 9vcs is the single CLI binary for the 9vcs version control
// system. This scaffold implements only the fully local, no-networking
// commands: init, record, log. See PLAN.md for the full design.
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
`)
}
