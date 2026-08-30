package main

import (
	"crypto/ed25519"
	"flag"
	"fmt"

	"github.com/sandgorgon/9auth"
	"github.com/sandgorgon/9vcs/objstore/patches"
	"github.com/sandgorgon/9vcs/repo"
)

func cmdLog(args []string) error {
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) > 1 {
		return fmt.Errorf("log: too many arguments (expected [<ref>])")
	}

	r, err := repo.Find()
	if err != nil {
		return err
	}

	var (
		hash patches.Hash
		ok   bool
	)
	if len(rest) == 1 {
		hash, err = r.ResolveRef(rest[0])
		if err != nil {
			return fmt.Errorf("log: %w", err)
		}
		ok = true
	} else {
		hash, ok, err = r.HeadHash()
		if err != nil {
			return fmt.Errorf("reading head: %w", err)
		}
	}
	if !ok {
		fmt.Println("no patches recorded yet")
		return nil
	}

	entries, err := patches.History(r.Store, hash)
	if err != nil {
		return fmt.Errorf("reading history: %w", err)
	}

	for _, e := range entries {
		p := e.Patch
		fmt.Printf("patch %s\n", e.Hash)
		if len(p.Dependencies) > 1 {
			deps := make([]string, len(p.Dependencies))
			for i, d := range p.Dependencies {
				deps[i] = d.String()[:12]
			}
			fmt.Printf("Merge:  %v\n", deps)
		}
		fmt.Printf("Author: %s\n", p.Author)
		if p.AuthorFingerprint != ([32]byte{}) {
			status := "verified"
			if !p.VerifyAuthorSignature() {
				status = "INVALID SIGNATURE"
			}
			fmt.Printf("Fingerprint: %s (%s)\n", auth.Fingerprint(ed25519.PublicKey(p.AuthorFingerprint[:])), status)
		}
		fmt.Printf("Date:   %s\n", p.Time.Local().Format("Mon Jan 2 15:04:05 2006 -0700"))
		fmt.Printf("\n    %s\n\n", p.Message)
		for _, fc := range p.Changes {
			switch fc.Kind {
			case patches.KindDelete:
				fmt.Printf("    %s (deleted)\n", fc.Path)
			case patches.KindSymlink:
				fmt.Printf("    %s -> %s (symlink)\n", fc.Path, fc.SymlinkTarget)
			case patches.KindBlob:
				fmt.Printf("    %s (binary%s)\n", fc.Path, executableSuffix(fc.Executable))
			default:
				ins, del := 0, 0
				for _, op := range fc.Ops {
					switch op.Kind {
					case patches.OpInsert:
						ins++
					case patches.OpDelete:
						del++
					}
				}
				fmt.Printf("    %s (+%d -%d%s)\n", fc.Path, ins, del, executableSuffix(fc.Executable))
			}
		}
		fmt.Println()
	}
	return nil
}

func executableSuffix(executable bool) string {
	if executable {
		return ", executable"
	}
	return ""
}
