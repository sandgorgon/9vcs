package repo

import (
	"fmt"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

// SelectOps returns the subset of ops whose ID is in selected, adjusting
// each selected insert's Prev to chain through other selected inserts
// only — skipping past any unselected one — so the result stands on its
// own exactly as if Diff had run against just the selected content.
//
// A delete op needs no such adjustment: unlike an insert, its Prev/Next
// always name line ids that existed before this diff ran (Diff computes
// them from old, never from another op in the same batch), so selecting
// any subset of deletes is already independent by construction —
// graph.go's resolveAlive chases a chain of dead nodes to whatever's
// still alive regardless of which subset of them died, in whichever
// order. An insert has no such luck: consecutive new lines chain through
// each other's freshly-minted IDs (Diff sets each one's Prev to the
// previous insert's ID), so dropping one from the middle of that chain
// without re-pointing its neighbor would reference an ID that was never
// created.
//
// ops not selected are simply omitted — they aren't lost. A subsequent
// ChangedFiles call, run against the new, partially-advanced base, will
// re-diff the (unchanged) working-tree content and regenerate exactly
// the still-pending ops on its own (with fresh IDs for any still-pending
// inserts, since an insert's ID is never meaningful across two separate
// diffs anyway) — so there is nothing else selective record needs to
// construct or persist for the unselected side.
func SelectOps(ops []patches.LineOp, selected map[string]bool) []patches.LineOp {
	replace := map[string]string{}
	resolve := func(id string) string {
		for {
			r, ok := replace[id]
			if !ok {
				return id
			}
			id = r
		}
	}

	var out []patches.LineOp
	for _, op := range ops {
		if op.Kind != patches.OpInsert {
			if selected[op.ID] {
				out = append(out, op)
			}
			continue
		}
		if !selected[op.ID] {
			// Whatever references this insert's ID as a Prev later in the
			// chain should instead resolve through to whatever came
			// before it.
			replace[op.ID] = resolve(op.Prev)
			continue
		}
		adjusted := op
		adjusted.Prev = resolve(op.Prev)
		out = append(out, adjusted)
	}
	return out
}

// Selection specifies which parts of a ChangedFiles result to fold into a
// patch. A path named in neither field is left entirely pending — not
// included at all, and untouched on disk, exactly as if record had never
// run for it.
type Selection struct {
	// Files selects an entire FileChange, of any Kind, wholesale by path
	// — the only option for a non-text change (blob, symlink, delete),
	// and a convenience for a text one the caller wants in full.
	Files map[string]bool
	// Lines selects specific line IDs within a KindText FileChange's Ops,
	// keyed by path. Each path here must be KindText in changes.
	Lines map[string]map[string]bool
}

// Apply narrows changes (as returned by ChangedFiles) down to just what
// sel selects, returning a new map suitable for folding into a patch —
// the FileChange for a Lines-selected path has its Ops narrowed via
// SelectOps; every Files-selected path is included as-is. It is an error
// to select a path that isn't present, isn't KindText for Lines, or is
// named by both fields, and an error to select nothing at all.
func (sel Selection) Apply(changes map[string]patches.FileChange) (map[string]patches.FileChange, error) {
	out := map[string]patches.FileChange{}
	for path, ids := range sel.Lines {
		fc, ok := changes[path]
		if !ok {
			return nil, fmt.Errorf("%s: no pending changes", path)
		}
		if fc.Kind != patches.KindText {
			return nil, fmt.Errorf("%s: line selection only applies to text changes", path)
		}
		ops := SelectOps(fc.Ops, ids)
		if len(ops) == 0 {
			continue // every named id ended up unselected once resolved (or none matched)
		}
		fc.Ops = ops
		out[path] = fc
	}
	for path := range sel.Files {
		if _, already := out[path]; already {
			return nil, fmt.Errorf("%s: selected by both line and whole-file selection", path)
		}
		fc, ok := changes[path]
		if !ok {
			return nil, fmt.Errorf("%s: no pending changes", path)
		}
		out[path] = fc
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no changes selected")
	}
	return out, nil
}
