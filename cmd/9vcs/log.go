package main

import (
	"crypto/ed25519"
	"flag"
	"fmt"

	"github.com/sandgorgon/9vcs/identity"
	"github.com/sandgorgon/9vcs/objstore/patches"
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

	r, err := findRepo()
	if err != nil {
		return err
	}

	var (
		hash patches.Hash
		ok   bool
	)
	if len(rest) == 1 {
		hash, err = r.resolveRef(rest[0])
		if err != nil {
			return fmt.Errorf("log: %w", err)
		}
		ok = true
	} else {
		hash, ok, err = r.headHash()
		if err != nil {
			return fmt.Errorf("reading head: %w", err)
		}
	}
	if !ok {
		fmt.Println("no patches recorded yet")
		return nil
	}

	entries, err := patches.History(r.store, hash)
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
			fmt.Printf("Fingerprint: %s (%s)\n", identity.Fingerprint(ed25519.PublicKey(p.AuthorFingerprint[:])), status)
		}
		fmt.Printf("Date:   %s\n", p.Time.Local().Format("Mon Jan 2 15:04:05 2006 -0700"))
		fmt.Printf("\n    %s\n\n", p.Message)
		for _, fc := range p.Changes {
			switch fc.Kind {
			case patches.KindDelete:
				fmt.Printf("    %s (deleted)\n", fc.Path)
			case patches.KindBlob:
				fmt.Printf("    %s (binary)\n", fc.Path)
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
				fmt.Printf("    %s (+%d -%d)\n", fc.Path, ins, del)
			}
		}
		fmt.Println()
	}
	return nil
}
