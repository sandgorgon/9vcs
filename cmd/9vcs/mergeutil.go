package main

import (
	"fmt"
	"sort"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

// mergeConflict is one path computeMerge couldn't resolve automatically.
type mergeConflict struct {
	Path      string
	Kind      string // "text", "binary", "modify/delete"
	DeletedBy string // "modify/delete" only: "ours" or "theirs"
}

// computeMerge unions ours and theirs, resolving what Materialize's plain
// union can't on its own:
//
//   - a binary path both sides changed to a different blob — no
//     line-graph fork mechanism applies to whole-file content, so this is
//     checked directly, keeping ours' content in the result;
//   - a modify/delete race, one side deleting a path the other genuinely
//     changed — a delete and an edit to the same path don't fork the way
//     two competing inserts do, so Materialize's union would otherwise
//     just silently pick whichever patch applies later in the
//     deterministic topological order. Detected via UniqueChanges,
//     resolved by keeping the modified side's content.
//
// merge and record both need the exact same resolution — merge to decide
// what to write to the working tree, record to compute a base that
// actually matches what's on disk mid-merge — so this is the one place it
// happens, called by both.
func computeMerge(r *repo, ours, theirs patches.Hash) (patches.Index, []mergeConflict, error) {
	merged, err := patches.Materialize(r.store, ours, theirs)
	if err != nil {
		return nil, nil, fmt.Errorf("replaying merged history: %w", err)
	}
	oursIdx, err := patches.Materialize(r.store, ours)
	if err != nil {
		return nil, nil, fmt.Errorf("replaying current history: %w", err)
	}
	theirsIdx, err := patches.Materialize(r.store, theirs)
	if err != nil {
		return nil, nil, fmt.Errorf("replaying target history: %w", err)
	}
	oursClosure, err := patches.Closure(r.store, ours)
	if err != nil {
		return nil, nil, fmt.Errorf("replaying current history: %w", err)
	}
	theirsClosure, err := patches.Closure(r.store, theirs)
	if err != nil {
		return nil, nil, fmt.Errorf("replaying target history: %w", err)
	}

	byPath := map[string]mergeConflict{}
	add := func(c mergeConflict) { byPath[c.Path] = c }

	for p, ourSt := range oursIdx {
		if ourSt.Kind != patches.KindBlob {
			continue
		}
		theirSt, ok := theirsIdx[p]
		if !ok || theirSt.Kind != patches.KindBlob || theirSt.Blob == ourSt.Blob {
			continue
		}
		add(mergeConflict{Path: p, Kind: "binary"})
		merged[p] = ourSt
	}

	for p, st := range merged {
		if st.Kind != patches.KindText || st.Graph == nil {
			continue
		}
		if _, forks := patches.Linearize(st.Graph); len(forks) > 0 {
			add(mergeConflict{Path: p, Kind: "text"})
		}
	}

	theirsDeleted, theirsModified, err := patches.UniqueChanges(r.store, theirs, oursClosure)
	if err != nil {
		return nil, nil, err
	}
	oursDeleted, oursModified, err := patches.UniqueChanges(r.store, ours, theirsClosure)
	if err != nil {
		return nil, nil, err
	}
	for p := range theirsDeleted {
		if st, ok := oursIdx[p]; ok && oursModified[p] {
			add(mergeConflict{Path: p, Kind: "modify/delete", DeletedBy: "theirs"})
			merged[p] = st
		}
	}
	for p := range oursDeleted {
		if st, ok := theirsIdx[p]; ok && theirsModified[p] {
			add(mergeConflict{Path: p, Kind: "modify/delete", DeletedBy: "ours"})
			merged[p] = st
		}
	}

	conflicts := make([]mergeConflict, 0, len(byPath))
	for _, c := range byPath {
		conflicts = append(conflicts, c)
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Path < conflicts[j].Path })

	return merged, conflicts, nil
}
