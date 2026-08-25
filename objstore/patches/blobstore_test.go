package patches

import (
	"sort"
	"testing"
)

func TestBlobStoreListAndRemove(t *testing.T) {
	dir := t.TempDir()
	blobs, err := OpenBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}

	// A freshly-opened, empty store lists as empty, not an error.
	got, err := blobs.List()
	if err != nil {
		t.Fatalf("List on empty store: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List on empty store = %v, want empty", got)
	}

	h1, err := blobs.Put([]byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	h2, err := blobs.Put([]byte("second"))
	if err != nil {
		t.Fatal(err)
	}

	got, err = blobs.List()
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{h1.String(), h2.String()}
	var gotNames []string
	for _, h := range got {
		gotNames = append(gotNames, h.String())
	}
	sort.Strings(wantNames)
	sort.Strings(gotNames)
	if len(gotNames) != len(wantNames) || gotNames[0] != wantNames[0] || gotNames[1] != wantNames[1] {
		t.Fatalf("List() = %v, want %v", gotNames, wantNames)
	}

	if err := blobs.Remove(h1); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if blobs.Has(h1) {
		t.Error("h1 should be gone after Remove")
	}
	if !blobs.Has(h2) {
		t.Error("h2 should be unaffected by removing h1")
	}
	got, err = blobs.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != h2 {
		t.Fatalf("List() after Remove = %v, want [%s]", got, h2)
	}

	// Removing something never stored is a no-op, not an error.
	if err := blobs.Remove(h1); err != nil {
		t.Errorf("Remove of an already-absent hash should be a no-op, got: %v", err)
	}
}
