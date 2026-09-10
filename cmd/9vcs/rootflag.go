package main

import (
	"os"

	"github.com/sandgorgon/9vcs/fsx"
	"github.com/sandgorgon/9vcs/repo"
)

// rootFlag/rootFlagSet hold a leading "-C <path>" consumed from the
// command line by parseRootFlag, before the command name — the same
// convention git's own -C uses (a generic UNIX idiom, not GitHub-
// specific vocabulary; see PLAN.md's vocabulary table). Package-level
// because every command needs it and there's exactly one per process
// invocation, parsed once in main.
var (
	rootFlag    string
	rootFlagSet bool
)

// parseRootFlag consumes a leading "-C <path>" from args, returning
// the remainder (still starting with the command name). Absent -C,
// args is returned unchanged and every command keeps resolving its
// repo exactly as before this feature existed — see PLAN.md decision
// #9: only an explicit -C reaches the namespace-first resolution
// algorithm at all.
func parseRootFlag(args []string) []string {
	if len(args) >= 2 && args[0] == "-C" {
		rootFlag = args[1]
		rootFlagSet = true
		return args[2:]
	}
	return args
}

// findRepo resolves the repo the same way every 9vcs command does:
// repo.FindAt(rootFlag) (namespace-first, see PLAN.md decision #9)
// when -C was given, repo.Find() — today's unmodified os.Getwd()-based
// walk-up, untouched — otherwise.
func findRepo() (*repo.Repo, error) {
	if rootFlagSet {
		return repo.FindAt(rootFlag)
	}
	return repo.Find()
}

// resolveRoot is findRepo's counterpart for cmdInit, which needs a
// place to create .9vcs before repo.Find(At) has anything to walk up
// to yet.
func resolveRoot() (fsx.FS, string, error) {
	if rootFlagSet {
		return repo.ResolveRoot(rootFlag)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, "", err
	}
	return fsx.NewOS(""), cwd, nil
}
