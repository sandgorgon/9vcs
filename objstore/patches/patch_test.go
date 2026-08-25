package patches

import "testing"

func contents(lines []Line) []string {
	if len(lines) == 0 {
		return nil
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Content
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// record builds a patch out of a plain-text edit against base's current
// content for path, applies it to a fresh graph seeded from base, and
// returns the new patch's hash plus the updated Index.
func record(t *testing.T, store *Store, deps []Hash, path string, content []string, base Index) (Hash, Index) {
	t.Helper()
	var oldLines []Line
	if st, ok := base[path]; ok && st.Kind == KindText {
		oldLines, _ = Linearize(st.Graph)
	}
	ops, _ := Diff(oldLines, content)
	p := &Patch{Dependencies: deps, Message: "x", Changes: []FileChange{{Path: path, Kind: KindText, Ops: ops, TrailingNewline: true}}}
	h, err := store.Put(p)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := Materialize(store, h)
	if err != nil {
		t.Fatal(err)
	}
	return h, idx
}

// TestDiffApplyRoundTrip checks the property Materialize depends on:
// applying Diff's own ops to a fresh graph and linearizing it reproduces
// exactly what Diff itself computed as the new content.
func TestDiffApplyRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		old  []string
		new  []string
	}{
		{"insert into empty", nil, []string{"a", "b", "c"}},
		{"no change", []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"delete everything", []string{"a", "b", "c"}, nil},
		{"edit middle, append", []string{"a", "b", "c"}, []string{"a", "X", "c", "d"}},
		{"duplicate lines collapse", []string{"a", "b", "b", "c"}, []string{"a", "b", "c"}},
		{"prepend", []string{"b", "c"}, []string{"a", "b", "c"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Seed the graph the way the real system does: a real Insert
			// patch against empty, not bare nodes with no edges between
			// them (which would spuriously read as an unpositioned orphan
			// as soon as any line is left untouched).
			seedOps, old := Diff(nil, c.old)
			g := newFileGraph()
			g.apply(seedOps)

			ops, newIndex := Diff(old, c.new)
			if got := contents(newIndex); !sameStrings(got, c.new) {
				t.Fatalf("Diff newIndex = %v, want %v", got, c.new)
			}

			g.apply(ops)
			rendered, forks := Linearize(g)
			if len(forks) > 0 {
				t.Fatalf("unexpected conflict linearizing a single linear edit")
			}
			if got := contents(rendered); !sameStrings(got, c.new) {
				t.Fatalf("Linearize(apply(old, ops)) = %v, want %v", got, c.new)
			}
		})
	}
}

// TestMaterializeChain records two patches directly against a Store (no
// CLI involved) and checks that replaying reconstructs the right content
// at each point — including the earlier patch alone, i.e. a branch point.
func TestMaterializeChain(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	h1, idx := record(t, store, nil, "f.txt", []string{"one", "two"}, Index{})
	h2, idx := record(t, store, []Hash{h1}, "f.txt", []string{"one", "TWO", "three"}, idx)
	_ = idx

	got2, err := Materialize(store, h2)
	if err != nil {
		t.Fatal(err)
	}
	lines2, forks2 := Linearize(got2["f.txt"].Graph)
	if len(forks2) > 0 {
		t.Fatal("unexpected conflict")
	}
	if want := []string{"one", "TWO", "three"}; !sameStrings(contents(lines2), want) {
		t.Fatalf("Materialize(h2) = %v, want %v", contents(lines2), want)
	}

	got1, err := Materialize(store, h1)
	if err != nil {
		t.Fatal(err)
	}
	lines1, _ := Linearize(got1["f.txt"].Graph)
	if want := []string{"one", "two"}; !sameStrings(contents(lines1), want) {
		t.Fatalf("Materialize(h1) = %v, want %v", contents(lines1), want)
	}

	gotZero, err := Materialize(store, Hash{})
	if err != nil {
		t.Fatal(err)
	}
	if len(gotZero) != 0 {
		t.Fatalf("Materialize(zero) = %v, want empty", gotZero)
	}
}

// TestMergeCleanDisjoint: two branches edit different, unrelated lines of
// the same file. Merging should combine both edits with no conflict.
func TestMergeCleanDisjoint(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	base, idx := record(t, store, nil, "f.txt", []string{"one", "two", "three"}, Index{})
	ours, _ := record(t, store, []Hash{base}, "f.txt", []string{"ONE", "two", "three"}, idx)
	theirs, _ := record(t, store, []Hash{base}, "f.txt", []string{"one", "two", "THREE"}, idx)

	merged, err := Materialize(store, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	lines, forks := Linearize(merged["f.txt"].Graph)
	if len(forks) > 0 {
		t.Fatalf("unexpected conflict merging disjoint edits: %v", contents(lines))
	}
	want := []string{"ONE", "two", "THREE"}
	if got := contents(lines); !sameStrings(got, want) {
		t.Fatalf("merged content = %v, want %v", got, want)
	}
}

// TestMergeConflictAndResolve: two branches edit the SAME line differently.
// That must show up as a real fork; resolving it by hand and recording
// through Resolve's extra ops must leave no trace of the fork behind.
func TestMergeConflictAndResolve(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	base, idx := record(t, store, nil, "f.txt", []string{"one", "two", "three"}, Index{})
	ours, _ := record(t, store, []Hash{base}, "f.txt", []string{"one", "TWO-ours", "three"}, idx)
	theirs, _ := record(t, store, []Hash{base}, "f.txt", []string{"one", "TWO-theirs", "three"}, idx)

	merged, err := Materialize(store, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	presented, forks := Linearize(merged["f.txt"].Graph)
	if len(forks) == 0 {
		t.Fatalf("expected a conflict, got clean merge: %v", contents(presented))
	}
	text := contents(presented)
	if !containsAll(text, "one", "TWO-ours", "TWO-theirs", "three") {
		t.Fatalf("conflict rendering missing expected content: %v", text)
	}

	// Resolve by hand the way a real user would: keep both alternatives,
	// in whatever order they were actually rendered (fork order is by
	// node id, not "ours"/"theirs" — not something to assume), just drop
	// the marker lines.
	var target []string
	for _, l := range presented {
		if l.Content == conflictOpen || l.Content == conflictSep || l.Content == conflictShut {
			continue
		}
		target = append(target, l.Content)
	}
	resolveOps, finalLines := Diff(StripMarkers(presented), target)
	resolveOps = append(resolveOps, Resolve(forks, finalLines)...)
	resolution := &Patch{
		Dependencies: []Hash{ours, theirs},
		Message:      "resolve",
		Changes:      []FileChange{{Path: "f.txt", Kind: KindText, Ops: resolveOps, TrailingNewline: true}},
	}
	resHash, err := store.Put(resolution)
	if err != nil {
		t.Fatal(err)
	}

	final, err := Materialize(store, resHash)
	if err != nil {
		t.Fatal(err)
	}
	finalRendered, finalForks := Linearize(final["f.txt"].Graph)
	if len(finalForks) > 0 {
		t.Fatalf("resolution patch left a conflict behind: %v", contents(finalRendered))
	}
	if got := contents(finalRendered); !sameStrings(got, target) {
		t.Fatalf("resolved content = %v, want %v", got, target)
	}
}

// TestMergeConflictResolveDiscardOneSide: same conflict, but the user
// discards one alternative entirely rather than keeping both. The
// discarded side's original edges must not leave any trace either.
func TestMergeConflictResolveDiscardOneSide(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	base, idx := record(t, store, nil, "f.txt", []string{"one", "two", "three"}, Index{})
	ours, _ := record(t, store, []Hash{base}, "f.txt", []string{"one", "TWO-ours", "three"}, idx)
	theirs, _ := record(t, store, []Hash{base}, "f.txt", []string{"one", "TWO-theirs", "three"}, idx)

	merged, err := Materialize(store, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	presented, forks := Linearize(merged["f.txt"].Graph)
	if len(forks) == 0 {
		t.Fatalf("expected a conflict, got clean merge: %v", contents(presented))
	}

	// Keep only "one", "TWO-ours" (whichever alternative it is), "three",
	// plus a brand-new line — mirroring a real resolution that edits
	// while resolving, not just deletes markers. Diff against the
	// marker-stripped base, not the marker-included presented rendering:
	// a deleted marker's own reconnect can otherwise splice a live path
	// through what should be a fully-dead discarded alternative — see
	// record.go's mergeBase for the real (non-test) version of this.
	target := []string{"one", "TWO-ours", "brand new line", "three"}
	resolveOps, finalLines := Diff(StripMarkers(presented), target)
	resolveOps = append(resolveOps, Resolve(forks, finalLines)...)
	resolution := &Patch{
		Dependencies: []Hash{ours, theirs},
		Message:      "resolve",
		Changes:      []FileChange{{Path: "f.txt", Kind: KindText, Ops: resolveOps, TrailingNewline: true}},
	}
	resHash, err := store.Put(resolution)
	if err != nil {
		t.Fatal(err)
	}

	final, err := Materialize(store, resHash)
	if err != nil {
		t.Fatal(err)
	}
	finalRendered, finalForks := Linearize(final["f.txt"].Graph)
	if len(finalForks) > 0 {
		t.Fatalf("resolution patch left a conflict behind: %v", contents(finalRendered))
	}
	if got := contents(finalRendered); !sameStrings(got, target) {
		t.Fatalf("resolved content = %v, want %v", got, target)
	}
}

// TestMaterializeSymlinkAndExecutable checks Materialize's handling of
// KindSymlink and FileChange.Executable directly against a Store — no
// CLI involved, mirroring TestMaterializeChain's shape.
func TestMaterializeSymlinkAndExecutable(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	h1, err := store.Put(&Patch{Message: "add an executable script and a symlink", Changes: []FileChange{
		{Path: "run.sh", Kind: KindText, TrailingNewline: true, Executable: true,
			Ops: []LineOp{{Kind: OpInsert, ID: "a", Content: "#!/bin/sh"}}},
		{Path: "current", Kind: KindSymlink, SymlinkTarget: "run.sh"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	idx, err := Materialize(store, h1)
	if err != nil {
		t.Fatal(err)
	}
	if !idx["run.sh"].Executable {
		t.Error("run.sh should be executable after materializing")
	}
	if idx["current"].Kind != KindSymlink {
		t.Fatalf("current: Kind = %v, want KindSymlink", idx["current"].Kind)
	}
	if idx["current"].SymlinkTarget != "run.sh" {
		t.Errorf("current: SymlinkTarget = %q, want %q", idx["current"].SymlinkTarget, "run.sh")
	}

	// A later patch can flip the bit back off, and retarget the symlink,
	// without anything about the line content changing.
	h2, err := store.Put(&Patch{Dependencies: []Hash{h1}, Message: "no longer executable, retarget the symlink", Changes: []FileChange{
		{Path: "run.sh", Kind: KindText, TrailingNewline: true, Executable: false,
			Ops: []LineOp{{Kind: OpInsert, ID: "b", Content: "#!/bin/sh"}}},
		{Path: "current", Kind: KindSymlink, SymlinkTarget: "run.sh.new"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	idx2, err := Materialize(store, h2)
	if err != nil {
		t.Fatal(err)
	}
	if idx2["run.sh"].Executable {
		t.Error("run.sh should no longer be executable after the second patch")
	}
	if idx2["current"].SymlinkTarget != "run.sh.new" {
		t.Errorf("current: SymlinkTarget = %q, want %q", idx2["current"].SymlinkTarget, "run.sh.new")
	}
}

// TestUniqueChanges: two branches diverge from a shared base — one
// deletes a path, the other edits it. UniqueChanges must report that
// divergence precisely: each side's own delete/modify, not anything
// inherited from the shared history, and not the other side's actions.
func TestUniqueChanges(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	base, idx := record(t, store, nil, "keep.txt", []string{"a"}, Index{})
	base, idx = record(t, store, []Hash{base}, "victim.txt", []string{"one", "two"}, idx)

	// ours: deletes victim.txt, leaves keep.txt untouched.
	oursDel := &Patch{
		Dependencies: []Hash{base},
		Message:      "ours deletes victim.txt",
		Changes:      []FileChange{{Path: "victim.txt", Kind: KindDelete}},
	}
	ours, err := store.Put(oursDel)
	if err != nil {
		t.Fatal(err)
	}

	// theirs: actually edits victim.txt.
	theirs, _ := record(t, store, []Hash{base}, "victim.txt", []string{"one", "TWO-edited"}, idx)

	oursClosure, err := Closure(store, ours)
	if err != nil {
		t.Fatal(err)
	}
	theirsClosure, err := Closure(store, theirs)
	if err != nil {
		t.Fatal(err)
	}

	oursDeleted, oursModified, err := UniqueChanges(store, ours, theirsClosure)
	if err != nil {
		t.Fatal(err)
	}
	if !oursDeleted["victim.txt"] {
		t.Errorf("ours' unique changes should show victim.txt deleted")
	}
	if oursModified["victim.txt"] || oursModified["keep.txt"] {
		t.Errorf("ours didn't modify anything: got modified=%v", oursModified)
	}

	theirsDeleted, theirsModified, err := UniqueChanges(store, theirs, oursClosure)
	if err != nil {
		t.Fatal(err)
	}
	if len(theirsDeleted) != 0 {
		t.Errorf("theirs deleted nothing: got %v", theirsDeleted)
	}
	if !theirsModified["victim.txt"] {
		t.Errorf("theirs' unique changes should show victim.txt modified")
	}
	if theirsModified["keep.txt"] {
		t.Errorf("keep.txt is shared history, not unique to theirs: got %v", theirsModified)
	}
}

func containsAll(hay []string, needles ...string) bool {
	set := map[string]bool{}
	for _, h := range hay {
		set[h] = true
	}
	for _, n := range needles {
		if !set[n] {
			return false
		}
	}
	return true
}
