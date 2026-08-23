package patches

import (
	"bytes"
	"errors"
	"os"
	"sort"
)

// Index is the last-recorded line graph for every tracked file, keyed by
// repo-relative path. It is the "lower layer" state that a working-tree
// diff is taken against — see PLAN.md "Workspace = private namespace".
type Index map[string][]Line

// LoadIndex reads the index from disk, returning an empty Index if the file
// doesn't exist yet (a freshly-initialized repo).
func LoadIndex(path string) (Index, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Index{}, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeIndex(data)
}

// Save writes idx to disk using the same length-prefixed encoding as
// Patch.Encode, for one format to reason about across the package.
func (idx Index) Save(path string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, idx.encode(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (idx Index) encode() []byte {
	paths := make([]string, 0, len(idx))
	for p := range idx {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var buf bytes.Buffer
	writeString(&buf, "9vcs-index-v1")
	writeInt64(&buf, int64(len(paths)))
	for _, p := range paths {
		writeString(&buf, p)
		lines := idx[p]
		writeInt64(&buf, int64(len(lines)))
		for _, l := range lines {
			writeString(&buf, l.ID)
			writeString(&buf, l.Content)
		}
	}
	return buf.Bytes()
}

func decodeIndex(data []byte) (Index, error) {
	r := bytes.NewReader(data)
	magic, err := readString(r)
	if err != nil {
		return nil, err
	}
	if magic != "9vcs-index-v1" {
		return nil, errors.New("patches: unrecognized index format")
	}
	nPaths, err := readInt64(r)
	if err != nil {
		return nil, err
	}
	idx := make(Index, nPaths)
	for i := int64(0); i < nPaths; i++ {
		p, err := readString(r)
		if err != nil {
			return nil, err
		}
		nLines, err := readInt64(r)
		if err != nil {
			return nil, err
		}
		lines := make([]Line, 0, nLines)
		for j := int64(0); j < nLines; j++ {
			id, err := readString(r)
			if err != nil {
				return nil, err
			}
			content, err := readString(r)
			if err != nil {
				return nil, err
			}
			lines = append(lines, Line{ID: id, Content: content})
		}
		idx[p] = lines
	}
	return idx, nil
}
