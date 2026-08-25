package patches

import (
	"bytes"
	"errors"
	"sort"
)

// PathState is one path's materialized state at some point in history: a
// line graph (KindText), a whole-file blob hash (KindBlob), or a symlink
// target (KindSymlink). A path absent from an Index has never existed, or
// was deleted, at that point.
type PathState struct {
	Kind            ChangeKind
	Graph           *FileGraph // KindText only
	TrailingNewline bool       // KindText only
	Executable      bool       // KindText or KindBlob only — see FileChange.Executable
	Blob            Hash       // KindBlob only
	SymlinkTarget   string     // KindSymlink only
}

// Index is the materialized state of every path touched by some point in
// history, keyed by repo-relative path.
type Index map[string]PathState

// ErrCycle is returned when a set of patches' Dependencies form a cycle —
// not reachable through normal use, since a patch can only depend on
// patches that existed (and so were already acyclic) before it was made.
var ErrCycle = errors.New("patches: dependency cycle detected")

// closureOf collects every patch transitively reachable from roots via
// Dependencies, including the roots themselves. The zero hash (an empty
// branch) contributes nothing.
func closureOf(store *Store, roots ...Hash) (map[Hash]*Patch, error) {
	closure := map[Hash]*Patch{}
	var walk func(h Hash) error
	walk = func(h Hash) error {
		if h.IsZero() {
			return nil
		}
		if _, ok := closure[h]; ok {
			return nil
		}
		p, err := store.Get(h)
		if err != nil {
			return err
		}
		closure[h] = p
		for _, d := range p.Dependencies {
			if err := walk(d); err != nil {
				return err
			}
		}
		return nil
	}
	for _, r := range roots {
		if err := walk(r); err != nil {
			return nil, err
		}
	}
	return closure, nil
}

// Closure returns the set of every patch hash transitively reachable from
// roots (including the roots), for ancestor checks like "is theirs already
// included in ours" (up to date) or "is ours included in theirs"
// (fast-forward) — see cmd/9vcs's merge command.
func Closure(store *Store, roots ...Hash) (map[Hash]bool, error) {
	closure, err := closureOf(store, roots...)
	if err != nil {
		return nil, err
	}
	set := make(map[Hash]bool, len(closure))
	for h := range closure {
		set[h] = true
	}
	return set, nil
}

// topoOrder orders closure so every patch comes after all of its
// dependencies, breaking ties between independently-ready patches by hash
// so replay is reproducible regardless of what order they were discovered
// in — required for two peers to materialize the same bytes from the same
// patch set.
func topoOrder(closure map[Hash]*Patch) ([]Hash, error) {
	remaining := make(map[Hash]int, len(closure))
	dependents := map[Hash][]Hash{}
	var ready []Hash
	for h, p := range closure {
		remaining[h] = len(p.Dependencies)
		for _, d := range p.Dependencies {
			dependents[d] = append(dependents[d], h)
		}
	}
	for h, n := range remaining {
		if n == 0 {
			ready = append(ready, h)
		}
	}

	order := make([]Hash, 0, len(closure))
	for len(ready) > 0 {
		sort.Slice(ready, func(i, j int) bool { return bytes.Compare(ready[i][:], ready[j][:]) < 0 })
		h := ready[0]
		ready = ready[1:]
		order = append(order, h)
		for _, dep := range dependents[h] {
			remaining[dep]--
			if remaining[dep] == 0 {
				ready = append(ready, dep)
			}
		}
	}
	if len(order) != len(closure) {
		return nil, ErrCycle
	}
	return order, nil
}

// HistoryEntry pairs a patch with its own hash, since a Patch doesn't
// carry its own content-address.
type HistoryEntry struct {
	Hash  Hash
	Patch *Patch
}

// History returns every patch transitively reachable from roots, ordered
// so each patch comes before everything that depends on it — a reverse
// topological order, most-recent-ish first. There is no single "current"
// order in a DAG with merges; this is a reproducible one, not the only
// valid one.
func History(store *Store, roots ...Hash) ([]HistoryEntry, error) {
	closure, err := closureOf(store, roots...)
	if err != nil {
		return nil, err
	}
	order, err := topoOrder(closure)
	if err != nil {
		return nil, err
	}
	out := make([]HistoryEntry, len(order))
	for i, h := range order {
		out[len(order)-1-i] = HistoryEntry{Hash: h, Patch: closure[h]}
	}
	return out, nil
}

// UniqueChanges scans every patch reachable from hash but not in exclude
// (typically the other merge side's closure — i.e. "what did this branch
// do on its own that the other side never saw"), returning the paths it
// explicitly deleted and the paths it otherwise changed (text or blob).
//
// This is what a modify/delete conflict needs and Materialize's plain
// union can't surface on its own: a delete and an edit to the same path
// don't fork the way two competing inserts do (there's no graph-level
// mechanism for it), so whichever patch happens to apply later in the
// deterministic topological order just silently wins. A caller detects
// the race directly: if this branch deleted a path the other branch's own
// UniqueChanges shows as modified (and the other branch's materialized
// state still has it), that's the conflict.
func UniqueChanges(store *Store, hash Hash, exclude map[Hash]bool) (deleted, modified map[string]bool, err error) {
	closure, err := closureOf(store, hash)
	if err != nil {
		return nil, nil, err
	}
	deleted = map[string]bool{}
	modified = map[string]bool{}
	for h, p := range closure {
		if exclude[h] {
			continue
		}
		for _, fc := range p.Changes {
			if fc.Kind == KindDelete {
				deleted[fc.Path] = true
			} else {
				modified[fc.Path] = true
			}
		}
	}
	return deleted, modified, nil
}

// Materialize replays every patch transitively reachable from roots, in
// dependency order, reconstructing the state of every path touched along
// the way. Passing more than one root is how a merge is materialized: the
// two branches' histories are simply unioned and replayed together, and
// wherever they independently extended the same graph node, that node
// naturally ends up with more than one alive successor — a fork, found by
// Linearize, not computed separately here.
//
// No patches (all-zero roots) materializes to an empty Index. There is no
// caching here — see PLAN.md's synth/ package for where that belongs once
// it's needed.
func Materialize(store *Store, roots ...Hash) (Index, error) {
	closure, err := closureOf(store, roots...)
	if err != nil {
		return nil, err
	}
	order, err := topoOrder(closure)
	if err != nil {
		return nil, err
	}

	idx := Index{}
	graphs := map[string]*FileGraph{}
	for _, h := range order {
		for _, fc := range closure[h].Changes {
			switch fc.Kind {
			case KindDelete:
				delete(idx, fc.Path)
				delete(graphs, fc.Path)
			case KindBlob:
				idx[fc.Path] = PathState{Kind: KindBlob, Blob: fc.Blob, Executable: fc.Executable}
				delete(graphs, fc.Path)
			case KindSymlink:
				idx[fc.Path] = PathState{Kind: KindSymlink, SymlinkTarget: fc.SymlinkTarget}
				delete(graphs, fc.Path)
			default: // KindText
				g, ok := graphs[fc.Path]
				if !ok {
					g = newFileGraph()
					graphs[fc.Path] = g
				}
				g.apply(fc.Ops)
				idx[fc.Path] = PathState{Kind: KindText, Graph: g, TrailingNewline: fc.TrailingNewline, Executable: fc.Executable}
			}
		}
	}
	return idx, nil
}
