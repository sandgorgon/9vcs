package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

func cmdRecord(args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	message := fs.String("m", "", "patch message")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *message == "" {
		return fmt.Errorf("record: -m MESSAGE is required")
	}

	r, err := findRepo()
	if err != nil {
		return err
	}

	branch, err := r.currentBranch()
	if err != nil {
		return fmt.Errorf("reading HEAD: %w", err)
	}
	if branch == "" {
		return fmt.Errorf("record: HEAD is detached; check out a branch first")
	}

	head, _, err := r.headHash()
	if err != nil {
		return fmt.Errorf("reading head: %w", err)
	}
	mergeHeads, err := r.mergeHeads()
	if err != nil {
		return fmt.Errorf("reading merge state: %w", err)
	}
	midMerge := len(mergeHeads) > 0
	if midMerge {
		// Remove merge's comparison sidecars before diffing the working
		// tree — they're tooling, not content, and must never end up
		// picked up as a newly-tracked file in the merge's own patch.
		sidecars, err := r.mergeSidecars()
		if err != nil {
			return fmt.Errorf("reading merge state: %w", err)
		}
		for _, s := range sidecars {
			if err := removeSidecarFile(r, s); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing %s: %w", s, err)
			}
		}
	}

	var base patches.Index
	var mergeConflicts []mergeConflict
	if midMerge {
		// The same resolution merge used to decide what to write to the
		// working tree, not a raw Materialize — a raw union wouldn't
		// know to prefer a modified path over one that lost a
		// modify/delete race, and would disagree with what's actually on
		// disk for that path, corrupting line identity on the next edit.
		base, mergeConflicts, err = computeMerge(r, append([]patches.Hash{head}, mergeHeads...)...)
	} else {
		base, err = r.materialize(head)
	}
	if err != nil {
		return fmt.Errorf("replaying history: %w", err)
	}

	changes, err := changedFiles(r, base)
	if err != nil {
		return err
	}

	// A modify/delete conflict resolved by keeping the content (as
	// opposed to honoring the deletion, which changedFiles already
	// handles fine — a delete is unambiguous regardless of graph state)
	// needs an explicit, self-contained FileChange here, even when
	// nothing textually differs from base — see modifyDeleteKeptTextOps
	// for why, and for a real order-dependent bug found and fixed the
	// same day this comment was last touched.
	//
	// Reads go through an os.Root confined to r.root, not a plain
	// filepath.Join + bare os.* call — same reasoning as
	// writeWorkingTree's rewrite (see its doc comment): c.Path is
	// already a validated string (no ".."), but what it actually
	// resolves to *on disk* could still traverse through an
	// intermediate symlink component if one happens to be sitting in
	// the working tree, and this is the one other place in this
	// codebase that reads working-tree content by path outside
	// changedFiles (which is safe by construction — see
	// workingtree.go's changedFiles doc comment).
	workRoot, err := os.OpenRoot(r.root)
	if err != nil {
		return fmt.Errorf("opening working tree root: %w", err)
	}
	defer workRoot.Close()

	for _, c := range mergeConflicts {
		if c.Kind != "modify/delete" {
			continue
		}
		rel := filepath.FromSlash(c.Path)
		info, err := workRoot.Lstat(rel)
		if os.IsNotExist(err) {
			continue // honoring the deletion; changedFiles' KindDelete already covers it
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", c.Path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := workRoot.Readlink(rel)
			if err != nil {
				return fmt.Errorf("reading symlink %s: %w", c.Path, err)
			}
			changes[c.Path] = patches.FileChange{Path: c.Path, Kind: patches.KindSymlink, SymlinkTarget: target}
			continue
		}
		executable := info.Mode()&0o111 != 0
		content, err := workRoot.ReadFile(rel)
		if err != nil {
			return fmt.Errorf("reading %s: %w", c.Path, err)
		}
		if isBinary(content) {
			hash, err := r.blobs.Put(content)
			if err != nil {
				return fmt.Errorf("storing blob for %s: %w", c.Path, err)
			}
			changes[c.Path] = patches.FileChange{Path: c.Path, Kind: patches.KindBlob, Blob: hash, Executable: executable}
			continue
		}
		ops := modifyDeleteKeptTextOps(base, c.Path, content)
		changes[c.Path] = patches.FileChange{Path: c.Path, Kind: patches.KindText, Ops: ops, TrailingNewline: hasTrailingNewline(content), Executable: executable}
	}

	if len(changes) == 0 && !midMerge {
		fmt.Println("nothing to record")
		return nil
	}
	for _, fc := range changes {
		if fc.Kind != patches.KindText {
			continue
		}
		for _, op := range fc.Ops {
			if op.Kind == patches.OpInsert && patches.IsMarker(op.Content) {
				return fmt.Errorf("%s: still has unresolved conflict markers; resolve them before recording", fc.Path)
			}
		}
	}

	var deps []patches.Hash
	if !head.IsZero() {
		deps = append(deps, head)
	}
	if midMerge {
		deps = append(deps, mergeHeads...)
	}

	authorStr, err := author(r)
	if err != nil {
		return fmt.Errorf("record: %w", err)
	}
	patch := &patches.Patch{Dependencies: deps, Author: authorStr, Time: time.Now(), Message: *message}
	for _, fc := range changes {
		patch.Changes = append(patch.Changes, fc)
	}
	signPatch(patch)

	hash, err := r.store.Put(patch)
	if err != nil {
		return fmt.Errorf("writing patch: %w", err)
	}
	if err := r.setLocalRefCAS(branch, head, hash); err != nil {
		return fmt.Errorf("updating ref: %w", err)
	}
	if midMerge {
		if err := r.clearMergeHeads(); err != nil {
			return fmt.Errorf("clearing merge state: %w", err)
		}
		if err := r.clearMergeSidecars(); err != nil {
			return fmt.Errorf("clearing merge state: %w", err)
		}
	}

	fmt.Printf("recorded %s: %s\n", hash.String()[:12], *message)
	return nil
}

// modifyDeleteKeptTextOps builds the Ops for a modify/delete conflict's
// kept text content (content is the winning side's raw file bytes, read
// from the working tree). Without this needing to pin anything, a
// future replay would still have to pick between the deleting patch and
// the modifying patch by the same deterministic topological tiebreak
// that created the conflict in the first place, with no guarantee it
// resolves the way base (and the working tree, right now) shows.
//
// A plain fresh insert (Diff(nil, ...), under brand-new line IDs
// unrelated to any existing history) is NOT actually safe regardless of
// that tiebreak, despite once being implemented as exactly that — a
// real, order-dependent bug, found and fixed the same day: Materialize
// (objstore/patches/replay.go) wipes a path's *entire* graph object on
// a KindDelete, not just the one node the deleting side targeted.
// Whether that wipe lands before or after the modifying side's own
// insert — which depends on the same hash-based topological tiebreak,
// since both are direct dependents of the same base — decides whether
// the modifying side's real node survives to still be alive when this
// patch's own change applies (it always applies last, being the
// merge). If it survives, a second fresh-ID insert for the identical
// content creates a genuine, order-triggered fork — two alive nodes,
// same content, both reachable — instead of the single clean line
// intended. Confirmed live, and reproduced deterministically by
// sampling both hash orderings directly against objstore/patches.
//
// Fixed by first emitting an explicit delete for every node base's own
// graph already reports alive for this path — the *correct* existing
// state computeMerge already resolved to (the modifying side's isolated
// materialize, mergeutil.go's idxs[j][p]) — before the fresh insert.
// Deleting an already-wiped, nonexistent node is a harmless no-op
// (graph.go's Delete case creates a dead placeholder rather than
// erroring); deleting a still-alive one neutralizes it. Either way the
// outcome is the same single surviving node, regardless of which order
// the wipe and the insert actually happened in.
//
// Scoped to KindText only — the caller's KindBlob/KindSymlink branches
// need no equivalent, since Materialize treats both as plain value
// overwrites, not additive graph operations, so they were never
// susceptible to this in the first place.
func modifyDeleteKeptTextOps(base patches.Index, path string, content []byte) []patches.LineOp {
	ops, _ := patches.Diff(nil, splitLines(string(content)))
	prior, ok := base[path]
	if !ok || prior.Kind != patches.KindText || prior.Graph == nil {
		return ops
	}
	existing, _ := patches.Linearize(prior.Graph)
	kill := make([]patches.LineOp, len(existing))
	for i, l := range existing {
		kill[i] = patches.LineOp{Kind: patches.OpDelete, ID: l.ID}
	}
	return append(kill, ops...)
}
