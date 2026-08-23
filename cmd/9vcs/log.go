package main

import (
	"flag"
	"fmt"

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

	for {
		p, err := r.store.Get(hash)
		if err != nil {
			return fmt.Errorf("reading patch %s: %w", hash, err)
		}

		fmt.Printf("patch %s\n", hash)
		fmt.Printf("Author: %s\n", p.Author)
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
					if op.Kind == patches.OpInsert {
						ins++
					} else {
						del++
					}
				}
				fmt.Printf("    %s (+%d -%d)\n", fc.Path, ins, del)
			}
		}
		fmt.Println()

		if p.Parent.IsZero() {
			break
		}
		hash = p.Parent
	}
	return nil
}
