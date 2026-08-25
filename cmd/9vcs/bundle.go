package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/sandgorgon/9vcs/bundle"
	"github.com/sandgorgon/9vcs/identity"
	"github.com/sandgorgon/9vcs/objstore/patches"
)

func cmdBundle(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("bundle: usage: 9vcs bundle export|import|show ...")
	}
	switch args[0] {
	case "export":
		return cmdBundleExport(args[1:])
	case "import":
		return cmdBundleImport(args[1:])
	case "show":
		return cmdBundleShow(args[1:])
	default:
		return fmt.Errorf("bundle: unknown subcommand %q (want export, import, or show)", args[0])
	}
}

func cmdBundleExport(args []string) error {
	fs := flag.NewFlagSet("bundle export", flag.ExitOnError)
	out := fs.String("o", "", "output bundle file (required)")
	message := fs.String("m", "", "bundle message")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return fmt.Errorf("bundle export: expected at least one <ref-or-hash>")
	}
	if *out == "" {
		return fmt.Errorf("bundle export: -o <file> is required")
	}

	r, err := findRepo()
	if err != nil {
		return err
	}
	roots := make([]patches.Hash, 0, len(rest))
	for _, arg := range rest {
		h, err := r.resolveRef(arg)
		if err != nil {
			return fmt.Errorf("bundle export: %w", err)
		}
		roots = append(roots, h)
	}

	id, err := identity.Load()
	if err != nil {
		return fmt.Errorf("bundle export: loading identity: %w", err)
	}

	data, n, err := bundle.Export(r.store, r.blobs, roots, *message, id.Key)
	if err != nil {
		return fmt.Errorf("bundle export: %w", err)
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return fmt.Errorf("bundle export: writing %s: %w", *out, err)
	}
	fmt.Printf("exported %d patch(es) to %s, signed as %s\n", n, *out, id.Fingerprint())
	return nil
}

func cmdBundleShow(args []string) error {
	fs := flag.NewFlagSet("bundle show", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("bundle show: usage: 9vcs bundle show <file>")
	}
	b, err := loadBundleFile(rest[0])
	if err != nil {
		return err
	}
	printBundleSummary(b)
	return nil
}

func cmdBundleImport(args []string) error {
	fs := flag.NewFlagSet("bundle import", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("bundle import: usage: 9vcs bundle import <file>")
	}

	r, err := findRepo()
	if err != nil {
		return err
	}
	b, err := loadBundleFile(rest[0])
	if err != nil {
		return err
	}
	if err := b.Store(r.store, r.blobs); err != nil {
		return fmt.Errorf("bundle import: %w", err)
	}
	printBundleSummary(b)
	fmt.Println("\nnothing integrated yet — inspect with `9vcs diff <hash>`, then `9vcs merge <hash>` to bring one in")
	return nil
}

// loadBundleFile reads, decodes, and verifies a bundle file — the
// decode-then-verify-then-decide-whether-to-trust-it separation every
// bundle command needs before doing anything else with its contents.
func loadBundleFile(path string) (*bundle.Bundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bundle: reading %s: %w", path, err)
	}
	b, err := bundle.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("bundle: decoding %s: %w", path, err)
	}
	if !b.Verify() {
		return nil, fmt.Errorf("bundle: %s has an invalid signature — corrupted or tampered with", path)
	}
	return b, nil
}

func printBundleSummary(b *bundle.Bundle) {
	fmt.Printf("signer: %s\n", identity.Fingerprint(b.SignerPub))
	if b.Message != "" {
		fmt.Printf("message: %s\n", b.Message)
	}
	fmt.Printf("%d patch(es):\n", len(b.Patches))
	for _, p := range b.Patches {
		fmt.Printf("  %s  %s\n", p.Hash().String()[:12], p.Message)
	}
}
