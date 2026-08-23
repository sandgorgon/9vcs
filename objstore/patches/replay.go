package patches

// PathState is one path's materialized state at some point in history:
// either a line graph (KindText) or a whole-file blob hash (KindBlob). A
// path absent from an Index has never existed, or was deleted, at that
// point.
type PathState struct {
	Kind            ChangeKind
	Lines           []Line // KindText only
	TrailingNewline bool   // KindText only
	Blob            Hash   // KindBlob only
}

// Index is the materialized state of every path touched by some point in
// history, keyed by repo-relative path.
type Index map[string]PathState

// Materialize replays every patch from the root up to and including hash,
// oldest first, reconstructing the state of every path touched along the
// way. The zero hash (no patches yet) materializes to an empty Index.
//
// This is the one primitive branch/diff/checkout are all built on: a
// branch is just a name for a hash, a diff is two Materialize calls fed to
// Diff, and a checkout writes one Materialize's result to disk. There is
// no separate cache here — see PLAN.md's synth/ package for where that
// belongs once it's needed.
func Materialize(store *Store, hash Hash) (Index, error) {
	idx := Index{}
	if hash.IsZero() {
		return idx, nil
	}

	var chain []*Patch
	for !hash.IsZero() {
		p, err := store.Get(hash)
		if err != nil {
			return nil, err
		}
		chain = append(chain, p)
		hash = p.Parent
	}

	for i := len(chain) - 1; i >= 0; i-- {
		for _, fc := range chain[i].Changes {
			switch fc.Kind {
			case KindDelete:
				delete(idx, fc.Path)
			case KindBlob:
				idx[fc.Path] = PathState{Kind: KindBlob, Blob: fc.Blob}
			default: // KindText
				idx[fc.Path] = PathState{
					Kind:            KindText,
					Lines:           Apply(idx[fc.Path].Lines, fc.Ops),
					TrailingNewline: fc.TrailingNewline,
				}
			}
		}
	}
	return idx, nil
}
