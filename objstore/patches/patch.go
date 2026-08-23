// Package patches implements the patch object model: content-addressed,
// immutable graph operations over per-file line graphs (Pijul-style patch
// theory, deliberately simplified for now — see PLAN.md "Open items").
package patches

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"time"

	"lukechampine.com/blake3"
)

// Hash is a BLAKE3-256 content hash.
type Hash [32]byte

func (h Hash) String() string { return hex.EncodeToString(h[:]) }

// IsZero reports whether h is the zero hash, used as the parent of a root patch.
func (h Hash) IsZero() bool { return h == Hash{} }

func HashFromHex(s string) (Hash, error) {
	var h Hash
	b, err := hex.DecodeString(s)
	if err != nil {
		return h, err
	}
	copy(h[:], b)
	return h, nil
}

type OpKind uint8

const (
	OpInsert OpKind = iota
	OpDelete
)

// LineOp is a single graph operation on one file's line graph.
//
// Insert: ID is the new line's stable id, After is the id of the line it is
// inserted immediately after ("" = start of file), Content is its text.
// Delete: ID is the id of the line being removed; After/Content are unused.
type LineOp struct {
	Kind    OpKind
	ID      string
	After   string
	Content string
}

// ChangeKind selects which fields of a FileChange are meaningful. A path's
// history can switch kind over time (e.g. a text file overwritten with a
// binary one) — each FileChange simply replaces whatever the path was
// before with whatever kind it declares.
type ChangeKind uint8

const (
	// KindText: Ops (and TrailingNewline) describe a line-graph edit,
	// replayed against whatever line graph the path had before.
	KindText ChangeKind = iota
	// KindBlob: the path becomes the whole-file content addressed by Blob.
	// Used for content line-diffing doesn't make sense for — binary
	// formats — see PLAN.md "Open items: how the line-graph maps to
	// non-text files". No delta/commute benefit for these paths, same
	// tradeoff Git makes for binary blobs.
	KindBlob
	// KindDelete: the path is removed. Ops/TrailingNewline/Blob are unused.
	KindDelete
)

// FileChange is the change to one path recorded by a single patch.
type FileChange struct {
	Path string
	Kind ChangeKind

	Ops             []LineOp // KindText only
	TrailingNewline bool     // KindText only: did the recorded content end in '\n'?

	Blob Hash // KindBlob only: hash of the whole file in the blob store
}

// Patch is one immutable, content-addressed unit of history: a set of
// per-file graph operations plus the hash of the patch it was recorded on
// top of.
type Patch struct {
	Parent  Hash // zero for the first patch in a history
	Author  string
	Time    time.Time
	Message string
	Changes []FileChange
}

// Encode produces a canonical, deterministic byte representation of p,
// suitable for hashing. Changes and their Ops are expected to already be in
// a stable order (callers sort by Path; ops are emitted in graph order by
// the differ).
func (p *Patch) Encode() []byte {
	var buf bytes.Buffer
	buf.Write(p.Parent[:])
	writeString(&buf, p.Author)
	writeInt64(&buf, p.Time.UTC().UnixNano())
	writeString(&buf, p.Message)
	writeInt64(&buf, int64(len(p.Changes)))
	for _, fc := range p.Changes {
		writeString(&buf, fc.Path)
		buf.WriteByte(byte(fc.Kind))
		writeBool(&buf, fc.TrailingNewline)
		buf.Write(fc.Blob[:])
		writeInt64(&buf, int64(len(fc.Ops)))
		for _, op := range fc.Ops {
			buf.WriteByte(byte(op.Kind))
			writeString(&buf, op.ID)
			writeString(&buf, op.After)
			writeString(&buf, op.Content)
		}
	}
	return buf.Bytes()
}

// Hash returns the content hash of p's canonical encoding.
func (p *Patch) Hash() Hash {
	return blake3.Sum256(p.Encode())
}

// SortChanges puts Changes into the canonical path order Encode/Hash expect.
func (p *Patch) SortChanges() {
	sort.Slice(p.Changes, func(i, j int) bool { return p.Changes[i].Path < p.Changes[j].Path })
}

// Decode parses the canonical encoding produced by Encode. The encoding is
// self-delimiting (length-prefixed fields in fixed order), so it doubles as
// the on-disk storage format — no separate serialization needed.
func Decode(data []byte) (*Patch, error) {
	r := bytes.NewReader(data)
	p := &Patch{}
	if _, err := io.ReadFull(r, p.Parent[:]); err != nil {
		return nil, fmt.Errorf("patch: decode parent: %w", err)
	}
	author, err := readString(r)
	if err != nil {
		return nil, fmt.Errorf("patch: decode author: %w", err)
	}
	p.Author = author
	nanos, err := readInt64(r)
	if err != nil {
		return nil, fmt.Errorf("patch: decode time: %w", err)
	}
	p.Time = time.Unix(0, nanos).UTC()
	msg, err := readString(r)
	if err != nil {
		return nil, fmt.Errorf("patch: decode message: %w", err)
	}
	p.Message = msg
	nChanges, err := readInt64(r)
	if err != nil {
		return nil, fmt.Errorf("patch: decode change count: %w", err)
	}
	p.Changes = make([]FileChange, 0, nChanges)
	for i := int64(0); i < nChanges; i++ {
		path, err := readString(r)
		if err != nil {
			return nil, fmt.Errorf("patch: decode change %d path: %w", i, err)
		}
		kindByte, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("patch: decode change %d kind: %w", i, err)
		}
		trailingNewline, err := readBool(r)
		if err != nil {
			return nil, fmt.Errorf("patch: decode change %d trailing newline: %w", i, err)
		}
		var blob Hash
		if _, err := io.ReadFull(r, blob[:]); err != nil {
			return nil, fmt.Errorf("patch: decode change %d blob: %w", i, err)
		}
		nOps, err := readInt64(r)
		if err != nil {
			return nil, fmt.Errorf("patch: decode change %d op count: %w", i, err)
		}
		ops := make([]LineOp, 0, nOps)
		for j := int64(0); j < nOps; j++ {
			kindByte, err := r.ReadByte()
			if err != nil {
				return nil, fmt.Errorf("patch: decode change %d op %d kind: %w", i, j, err)
			}
			id, err := readString(r)
			if err != nil {
				return nil, fmt.Errorf("patch: decode change %d op %d id: %w", i, j, err)
			}
			after, err := readString(r)
			if err != nil {
				return nil, fmt.Errorf("patch: decode change %d op %d after: %w", i, j, err)
			}
			content, err := readString(r)
			if err != nil {
				return nil, fmt.Errorf("patch: decode change %d op %d content: %w", i, j, err)
			}
			ops = append(ops, LineOp{Kind: OpKind(kindByte), ID: id, After: after, Content: content})
		}
		p.Changes = append(p.Changes, FileChange{
			Path:            path,
			Kind:            ChangeKind(kindByte),
			TrailingNewline: trailingNewline,
			Blob:            blob,
			Ops:             ops,
		})
	}
	return p, nil
}

func readString(r *bytes.Reader) (string, error) {
	n, err := readInt64(r)
	if err != nil {
		return "", err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func readBool(r *bytes.Reader) (bool, error) {
	b, err := r.ReadByte()
	return b != 0, err
}

func writeBool(buf *bytes.Buffer, v bool) {
	if v {
		buf.WriteByte(1)
	} else {
		buf.WriteByte(0)
	}
}

func readInt64(r *bytes.Reader) (int64, error) {
	var tmp [8]byte
	if _, err := io.ReadFull(r, tmp[:]); err != nil {
		return 0, err
	}
	return int64(binary.BigEndian.Uint64(tmp[:])), nil
}

func writeString(buf *bytes.Buffer, s string) {
	writeInt64(buf, int64(len(s)))
	buf.WriteString(s)
}

func writeInt64(buf *bytes.Buffer, v int64) {
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], uint64(v))
	buf.Write(tmp[:])
}
