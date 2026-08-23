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

// TestDiffApplyRoundTrip checks the property Materialize depends on: Apply
// replaying Diff's own ops against the same starting point reproduces
// exactly what Diff itself computed as the new state.
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
			var old []Line
			for _, s := range c.old {
				old = append(old, Line{ID: newLineID(), Content: s})
			}
			ops, newIndex := Diff(old, c.new)
			if got := contents(newIndex); !sameStrings(got, c.new) {
				t.Fatalf("Diff newIndex = %v, want %v", got, c.new)
			}
			applied := Apply(old, ops)
			if got := contents(applied); !sameStrings(got, c.new) {
				t.Fatalf("Apply(old, ops) = %v, want %v", got, c.new)
			}
		})
	}
}

// TestMaterializeChain records two patches directly against a Store (no CLI
// involved) and checks that replaying reconstructs the right content at
// each point — including the earlier patch alone, i.e. a branch point.
func TestMaterializeChain(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	idx := Index{}

	ops1, newIdx1 := Diff(idx["f.txt"], []string{"one", "two"})
	p1 := &Patch{Message: "first", Changes: []FileChange{{Path: "f.txt", Ops: ops1}}}
	h1, err := store.Put(p1)
	if err != nil {
		t.Fatal(err)
	}
	idx["f.txt"] = newIdx1

	ops2, newIdx2 := Diff(idx["f.txt"], []string{"one", "TWO", "three"})
	p2 := &Patch{Parent: h1, Message: "second", Changes: []FileChange{{Path: "f.txt", Ops: ops2}}}
	h2, err := store.Put(p2)
	if err != nil {
		t.Fatal(err)
	}
	idx["f.txt"] = newIdx2

	got2, err := Materialize(store, h2)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"one", "TWO", "three"}; !sameStrings(contents(got2["f.txt"]), want) {
		t.Fatalf("Materialize(h2) = %v, want %v", contents(got2["f.txt"]), want)
	}

	got1, err := Materialize(store, h1)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"one", "two"}; !sameStrings(contents(got1["f.txt"]), want) {
		t.Fatalf("Materialize(h1) = %v, want %v", contents(got1["f.txt"]), want)
	}

	gotZero, err := Materialize(store, Hash{})
	if err != nil {
		t.Fatal(err)
	}
	if len(gotZero) != 0 {
		t.Fatalf("Materialize(zero) = %v, want empty", gotZero)
	}
}
