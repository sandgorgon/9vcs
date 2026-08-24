package main

import (
	"flag"
	"fmt"
)

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	fingerprint := fs.String("peer-fingerprint", "", "expected fingerprint of the peer, as an explicit one-off pin; omit to use the known-peers store, prompting on first connection")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 || len(rest) > 3 {
		return fmt.Errorf("import: expected -peer-fingerprint <hex> <addr> <ref-name> [<local-branch-name>]")
	}
	addr, refName := rest[0], rest[1]
	localName := refName
	if len(rest) == 3 {
		localName = rest[2]
	}

	r, err := findRepo()
	if err != nil {
		return err
	}
	c, err := dialPeer(*fingerprint, addr)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}
	defer c.Close()

	remoteHash, err := fetchRefHash(c, refName)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}

	if err := importClosure(c, r.store, r.blobs, remoteHash); err != nil {
		return fmt.Errorf("import: %w", err)
	}

	localHash, _, err := r.refHash(localName)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}

	dir, err := classify(r, localHash, remoteHash)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}
	switch dir {
	case dirUpToDate:
		fmt.Println("already up to date")
		return nil
	case dirPush:
		fmt.Printf("local branch %q is already ahead of %s's %q; nothing imported\n", localName, addr, refName)
		return nil
	case dirDiverged:
		return fmt.Errorf("import: local branch %q has diverged from %s's %q; already fetched the missing history — check out %q and run `9vcs merge %s`, or use `9vcs reconcile` instead of import", localName, addr, refName, localName, remoteHash.String()[:12])
	}

	if err := pullRef(r, localName, localHash, remoteHash); err != nil {
		return fmt.Errorf("import: %w", err)
	}
	fmt.Printf("imported %s's %q (%s) into local branch %q\n", addr, refName, remoteHash.String()[:12], localName)
	return nil
}
