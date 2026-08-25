package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

// cmdApply is `merge`'s N-way sibling: integrate one or more specific
// patches (typically pulled in via `bundle import`, but any ref or hash
// already present locally works) into the current branch in a single
// merge, rather than one branch tip at a time. See PLAN.md decision #8's
// "apply — concrete scope": the underlying graph/conflict machinery
// (Linearize/Resolve) is already N-way, so this reuses computeMerge
// generalized to variadic roots instead of chaining separate two-way
// merges.
func cmdApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return fmt.Errorf("apply: expected at least one <patch-hash-or-ref>")
	}

	r, err := findRepo()
	if err != nil {
		return err
	}

	if heads, err := r.mergeHeads(); err != nil {
		return err
	} else if len(heads) > 0 {
		return fmt.Errorf("apply: a merge is already in progress; resolve conflicts and run record, or `9vcs merge -abort` to abandon it")
	}

	branch, err := r.currentBranch()
	if err != nil {
		return fmt.Errorf("reading HEAD: %w", err)
	}
	if branch == "" {
		return fmt.Errorf("apply: HEAD is detached; check out a branch first")
	}

	ours, _, err := r.headHash()
	if err != nil {
		return fmt.Errorf("reading head: %w", err)
	}
	oursClosure, err := patches.Closure(r.store, ours)
	if err != nil {
		return fmt.Errorf("replaying current history: %w", err)
	}

	// Resolve every target and drop anything already applied — a target
	// that's ours itself or already reachable from ours needs no work,
	// and isn't an error the way an unresolvable ref is.
	seen := map[patches.Hash]bool{}
	var targets []patches.Hash
	for _, arg := range rest {
		h, err := r.resolveRef(arg)
		if err != nil {
			return fmt.Errorf("apply: %w", err)
		}
		if h == ours || oursClosure[h] || seen[h] {
			fmt.Printf("%s already applied, skipping\n", arg)
			continue
		}
		seen[h] = true
		targets = append(targets, h)
	}
	if len(targets) == 0 {
		fmt.Println("already up to date (nothing to apply)")
		return nil
	}

	oursIdx, err := r.materialize(ours)
	if err != nil {
		return fmt.Errorf("replaying current history: %w", err)
	}
	dirty, err := changedFiles(r, oursIdx)
	if err != nil {
		return err
	}
	if len(dirty) > 0 {
		return fmt.Errorf("apply: uncommitted changes would be overwritten; record or discard them first")
	}

	// Fast-forward: exactly one target remains, and ours is already an
	// ancestor of it — same degenerate case cmdMerge handles for a
	// single target. With more than one target this essentially never
	// applies (nothing left to actually combine), so it's not worth
	// generalizing further than the single-target case.
	if len(targets) == 1 {
		targetClosure, err := patches.Closure(r.store, targets[0])
		if err != nil {
			return fmt.Errorf("replaying %s: %w", targets[0], err)
		}
		if targetClosure[ours] {
			targetIdx, err := r.materialize(targets[0])
			if err != nil {
				return fmt.Errorf("replaying %s: %w", targets[0], err)
			}
			if err := writeWorkingTree(r, oursIdx, targetIdx); err != nil {
				return fmt.Errorf("writing working tree: %w", err)
			}
			if err := r.setLocalRefCAS(branch, ours, targets[0]); err != nil {
				return err
			}
			fmt.Printf("fast-forwarded %s to %s\n", branch, targets[0].String()[:12])
			return nil
		}
	}

	roots := append([]patches.Hash{ours}, targets...)
	merged, conflicts, err := computeMerge(r, roots...)
	if err != nil {
		return err
	}

	// Binary conflicts get a comparison sidecar per target whose content
	// actually differs from ours (computeMerge already resolved to keep
	// ours) — unlike merge's single "theirs", apply can have more than
	// one differing side to show.
	var sidecars []string
	targetIdxs := make(map[patches.Hash]patches.Index, len(targets))
	for _, c := range conflicts {
		if c.Kind != "binary" {
			continue
		}
		for _, t := range targets {
			idx, ok := targetIdxs[t]
			if !ok {
				idx, err = r.materialize(t)
				if err != nil {
					return fmt.Errorf("replaying %s: %w", t, err)
				}
				targetIdxs[t] = idx
			}
			st, ok := idx[c.Path]
			if !ok || st.Kind != patches.KindBlob {
				continue
			}
			if oursSt, ok := oursIdx[c.Path]; ok && oursSt.Kind == patches.KindBlob && oursSt.Blob == st.Blob {
				continue // this target agrees with ours, nothing to compare
			}
			data, err := r.blobs.Get(st.Blob)
			if err != nil {
				return fmt.Errorf("reading blob for %s: %w", c.Path, err)
			}
			sidecar := binaryConflictSidecar(c.Path, t)
			if err := writeSidecarFile(r, sidecar, data); err != nil {
				return fmt.Errorf("writing %s: %w", sidecar, err)
			}
			sidecars = append(sidecars, sidecar)
		}
	}

	if err := writeWorkingTree(r, oursIdx, merged); err != nil {
		return fmt.Errorf("writing working tree: %w", err)
	}
	if err := r.setMergeHeads(targets); err != nil {
		return fmt.Errorf("recording merge state: %w", err)
	}
	if err := r.setMergeSidecars(sidecars); err != nil {
		return fmt.Errorf("recording merge state: %w", err)
	}

	if len(conflicts) == 0 {
		fmt.Println("applied cleanly; run `9vcs record` to finish")
		return nil
	}
	fmt.Println("automatic apply failed; fix conflicts, then run `9vcs record` to finish:")
	for _, c := range conflicts {
		switch c.Kind {
		case "binary":
			fmt.Printf("  CONFLICT (binary): %s — kept your version; see sidecar file(s) for comparison\n", c.Path)
		case "modify/delete":
			fmt.Printf("  CONFLICT (modify/delete): %s — deleted by %s, modified elsewhere; kept the modified version\n", c.Path, c.DeletedBy)
		case "symlink":
			fmt.Printf("  CONFLICT (symlink): %s — kept your target; other target(s): %s\n", c.Path, strings.Join(c.OtherTargets, ", "))
		case "type":
			fmt.Printf("  CONFLICT (type): %s — this path is a different kind of thing (text/binary/symlink) on each side; kept your version\n", c.Path)
		default:
			fmt.Printf("  CONFLICT: %s\n", c.Path)
		}
	}
	return nil
}
