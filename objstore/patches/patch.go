// Package patches implements the patch object model: content-addressed,
// immutable graph operations over per-file line graphs (Pijul-style patch
// theory, deliberately simplified for now — see PLAN.md "Open items").
package patches

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// Hash is a SHA-256 content hash.
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
	// OpSever explicitly kills the edge Prev->Next, with no other effect —
	// ID/Content unused. Used only by merge-conflict resolution (see
	// linearize.go's Resolve) to retract a fork alternative's now-stale
	// edge once the resolved content no longer puts it there; ordinary
	// edits never need it, since Insert/Delete already sever whatever
	// edge they split or reconnect around.
	OpSever
	// OpLink explicitly adds the edge Prev->Next, with no other effect —
	// ID/Content unused, and both nodes must already exist. Used only by
	// merge-conflict resolution, to connect two surviving, content-wise
	// *unedited* fork alternatives that are now adjacent — Diff alone
	// can't discover this, since neither line's content changed, so it
	// emits no op that would otherwise create the edge between them.
	OpLink
)

// LineOp is a single graph operation on one file's line graph, expressed
// relative to the two neighbors it had in whatever base state this op was
// computed against.
//
// Insert: ID is the new line's stable id, Content is its text, Prev/Next
// are the ids immediately before/after it ("" = start/end of file). The op
// both adds edges Prev->ID and ID->Next, and severs the direct Prev->Next
// edge it's splitting.
//
// Delete: ID is the id of the line being removed, and Prev/Next are its
// same two neighbors — severing Prev->ID and ID->Next, and adding a new
// Prev->Next edge that reconnects around the gap. This reconnect is what
// lets a concurrent, unrelated edit downstream commute cleanly instead of
// silently forking; see graph.go.
type LineOp struct {
	Kind    OpKind
	ID      string
	Prev    string
	Next    string
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
	// KindSymlink: the path becomes a symbolic link pointing at
	// SymlinkTarget. Stored as a plain string, not content-addressed via
	// Blob — a symlink target is a handful of bytes, not file content, so
	// a separate blob-store entry would be pure overhead. Like KindBlob,
	// there's no line-level merge for a symlink target: two roots
	// pointing it at different targets is a conflict, not a fork (see
	// cmd/9vcs/mergeutil.go's computeMerge).
	KindSymlink
)

// validPath reports whether p is safe to join under a repo root and
// write to: repo-relative (no leading "/"), forward-slash, already in
// canonical form (no "..", no empty segments, no redundant "./"). A
// FileChange's Path ultimately reaches os.WriteFile/os.Symlink via a
// plain filepath.Join with the repo root (writeWorkingTree,
// cmd/9vcs/workingtree.go) — Join does not confine a ".."-containing
// path to stay under root, it just resolves straight through, so a
// patch carrying one would write completely outside the repo the
// moment it's applied or checked out. A genuine local `record` can
// never produce such a path (every path it sees comes from an actual
// filepath.WalkDir under the repo root — see workingFiles), so the only
// way one exists is a patch crafted by an adversarial or simply buggy
// peer and received via import/reconcile, a served write, or a bundle.
// Checked at both places a Patch can come to be persisted — Decode
// (every patch received from outside this process) and Store.Put
// (every patch that's ever stored, however it was built) — so there is
// no route to a stored Patch carrying an unsafe path.
func validPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") {
		return false
	}
	if path.Clean(p) != p {
		return false // catches empty segments, redundant "./", a trailing "/", "a/../b", etc.
	}
	// path.Clean's own doc comment: a ".." with no preceding non-".."
	// element to cancel against is *retained*, not an error — there's
	// nothing wrong, from Clean's own perspective, with "../etc/passwd"
	// or a bare "..", since neither has anything earlier in the path to
	// resolve it against. That's exactly the traversal case this exists
	// to catch, so it needs its own explicit check.
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// FileChange is the change to one path recorded by a single patch.
type FileChange struct {
	Path string
	Kind ChangeKind

	Ops             []LineOp // KindText only
	TrailingNewline bool     // KindText only: did the recorded content end in '\n'?
	// Executable applies to KindText and KindBlob (a symlink's own mode
	// bits are meaningless in POSIX — what matters is its target's — so
	// KindSymlink never sets this): whether the file's owner-execute bit
	// was set when recorded. Checked and restored alongside content, not
	// as a separate conflict category — an executable-bit-only
	// disagreement between two branches (content otherwise identical)
	// isn't flagged as a conflict, unlike a genuine content difference;
	// see mergeutil.go's computeMerge doc comment for the deliberate
	// scope cut.
	Executable bool

	Blob          Hash   // KindBlob only: hash of the whole file in the blob store
	SymlinkTarget string // KindSymlink only: the link's target, exactly as read via os.Readlink
}

// patchFormatVersion tags Encode's output so a genuinely incompatible
// future format change fails Decode loudly instead of misparsing. This
// project is pre-release with exactly one person using it and no data
// anywhere that needs to keep decoding across a format change, so this
// is deliberately just a cheap tripwire, not real multi-version support:
// there's only ever one recognized value, Decode refuses anything else,
// and the format is free to change in place as often as needed. Real
// version dispatch (multiple recognized values, each with its own
// decode path) is a decision to make once there's a formal release and
// therefore real data whose compatibility actually matters — see
// PLAN.md decision #1.
//
// Bumped 1 -> 2 to add FileChange.Executable and KindSymlink/
// SymlinkTarget (see PLAN.md's "File mode and symlinks — concrete
// scope"): still pre-release at the time of the bump, so this is an
// in-place change, not a new dispatched version — the old value 1 is
// simply no longer recognized.
const patchFormatVersion byte = 2

// Patch is one immutable, content-addressed unit of history: a set of
// per-file graph operations, plus the patches whose effects those ops
// assume are already applied. A root patch has no dependencies. A merge
// patch depends on more than one prior patch — this (not a special "merge"
// flag) is what lets two branches' histories combine: repo state is
// whatever's reachable by following Dependencies, not a single chain.
type Patch struct {
	Dependencies []Hash
	Author       string
	Time         time.Time
	Message      string
	Changes      []FileChange

	// AuthorFingerprint and AuthorSignature are the recording install's
	// raw Ed25519 public key and its signature over SignablePayload().
	// Both all-zero means unsigned — a legitimate state (e.g. the
	// recorder had no usable identity at record time), not an error; see
	// VerifyAuthorSignature.
	AuthorFingerprint [32]byte
	AuthorSignature   [64]byte
}

// SignablePayload is everything Encode writes except AuthorSignature
// itself: the format byte, Dependencies/Author/Time/Message/Changes, and
// AuthorFingerprint. cmd/9vcs/record.go signs this when it first builds
// a Patch; VerifyAuthorSignature checks against it. Splitting this out
// of Encode keeps Encode a pure serializer with no key-material
// dependency — signing happens exactly once, explicitly, at record time.
// Changes and their Ops are expected to already be in a stable order
// (callers sort by Path; ops are emitted in graph order by the differ).
func (p *Patch) SignablePayload() []byte {
	var buf bytes.Buffer
	buf.WriteByte(patchFormatVersion)
	writeInt64(&buf, int64(len(p.Dependencies)))
	for _, d := range p.Dependencies {
		buf.Write(d[:])
	}
	writeString(&buf, p.Author)
	writeInt64(&buf, p.Time.UTC().UnixNano())
	writeString(&buf, p.Message)
	writeInt64(&buf, int64(len(p.Changes)))
	for _, fc := range p.Changes {
		writeString(&buf, fc.Path)
		buf.WriteByte(byte(fc.Kind))
		writeBool(&buf, fc.TrailingNewline)
		writeBool(&buf, fc.Executable)
		buf.Write(fc.Blob[:])
		writeString(&buf, fc.SymlinkTarget)
		writeInt64(&buf, int64(len(fc.Ops)))
		for _, op := range fc.Ops {
			buf.WriteByte(byte(op.Kind))
			writeString(&buf, op.ID)
			writeString(&buf, op.Prev)
			writeString(&buf, op.Next)
			writeString(&buf, op.Content)
		}
	}
	buf.Write(p.AuthorFingerprint[:])
	return buf.Bytes()
}

// Encode produces a canonical, deterministic byte representation of p,
// suitable for hashing and storage — the on-disk/wire format.
func (p *Patch) Encode() []byte {
	buf := bytes.NewBuffer(p.SignablePayload())
	buf.Write(p.AuthorSignature[:])
	return buf.Bytes()
}

// Hash returns the content hash of p's canonical encoding.
func (p *Patch) Hash() Hash {
	return sha256.Sum256(p.Encode())
}

// VerifyAuthorSignature reports whether p's authorship claim is
// internally consistent: true if p is unsigned (AuthorFingerprint is
// all-zero — no claim made, nothing to check), true if it's signed and
// the signature genuinely matches, false only when a fingerprint is
// present but the signature doesn't verify against it — a forged or
// corrupted authorship claim. Callers that ingest a patch from outside
// this process (sync.go's fetchPatch, vcsfs.go's patch-write path) treat
// false the same as a Hash mismatch: refuse the transfer.
func (p *Patch) VerifyAuthorSignature() bool {
	if p.AuthorFingerprint == ([32]byte{}) {
		return true
	}
	return ed25519.Verify(p.AuthorFingerprint[:], p.SignablePayload(), p.AuthorSignature[:])
}

// Normalize puts Changes and Dependencies into the canonical order
// Encode/Hash expect, so semantically-identical patches always hash the
// same regardless of the order callers built them in.
func (p *Patch) Normalize() {
	sort.Slice(p.Changes, func(i, j int) bool { return p.Changes[i].Path < p.Changes[j].Path })
	sort.Slice(p.Dependencies, func(i, j int) bool {
		return bytes.Compare(p.Dependencies[i][:], p.Dependencies[j][:]) < 0
	})
}

// Decode parses the canonical encoding Encode produces. The leading byte
// must be patchFormatVersion — pre-release, a mismatch means stale data
// from before an in-place format change, not a different peer version to
// dispatch on (see patchFormatVersion's doc comment). Everything after
// it is self-delimiting (length-prefixed fields in fixed order), so it
// doubles as the on-disk storage format — no separate serialization
// needed.
func Decode(data []byte) (*Patch, error) {
	r := bytes.NewReader(data)
	v, err := r.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("patch: decode format byte: %w", err)
	}
	if v != patchFormatVersion {
		return nil, fmt.Errorf("patch: unrecognized format byte %d (want %d) — pre-release, so this is almost certainly stale data from before a format change, not a version to support", v, patchFormatVersion)
	}
	p := &Patch{}
	nDeps, err := readCount(r, "patch: decode dependency count")
	if err != nil {
		return nil, err
	}
	p.Dependencies = make([]Hash, nDeps)
	for i := range p.Dependencies {
		if _, err := io.ReadFull(r, p.Dependencies[i][:]); err != nil {
			return nil, fmt.Errorf("patch: decode dependency %d: %w", i, err)
		}
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
	nChanges, err := readCount(r, "patch: decode change count")
	if err != nil {
		return nil, err
	}
	p.Changes = make([]FileChange, 0, nChanges)
	for i := int64(0); i < nChanges; i++ {
		changePath, err := readString(r)
		if err != nil {
			return nil, fmt.Errorf("patch: decode change %d path: %w", i, err)
		}
		if !validPath(changePath) {
			return nil, fmt.Errorf("patch: decode change %d: unsafe path %q (escapes the repo root or isn't in canonical form)", i, changePath)
		}
		kindByte, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("patch: decode change %d kind: %w", i, err)
		}
		trailingNewline, err := readBool(r)
		if err != nil {
			return nil, fmt.Errorf("patch: decode change %d trailing newline: %w", i, err)
		}
		executable, err := readBool(r)
		if err != nil {
			return nil, fmt.Errorf("patch: decode change %d executable: %w", i, err)
		}
		var blob Hash
		if _, err := io.ReadFull(r, blob[:]); err != nil {
			return nil, fmt.Errorf("patch: decode change %d blob: %w", i, err)
		}
		symlinkTarget, err := readString(r)
		if err != nil {
			return nil, fmt.Errorf("patch: decode change %d symlink target: %w", i, err)
		}
		nOps, err := readCount(r, fmt.Sprintf("patch: decode change %d op count", i))
		if err != nil {
			return nil, err
		}
		ops := make([]LineOp, 0, nOps)
		for j := int64(0); j < nOps; j++ {
			opKindByte, err := r.ReadByte()
			if err != nil {
				return nil, fmt.Errorf("patch: decode change %d op %d kind: %w", i, j, err)
			}
			id, err := readString(r)
			if err != nil {
				return nil, fmt.Errorf("patch: decode change %d op %d id: %w", i, j, err)
			}
			prev, err := readString(r)
			if err != nil {
				return nil, fmt.Errorf("patch: decode change %d op %d prev: %w", i, j, err)
			}
			next, err := readString(r)
			if err != nil {
				return nil, fmt.Errorf("patch: decode change %d op %d next: %w", i, j, err)
			}
			content, err := readString(r)
			if err != nil {
				return nil, fmt.Errorf("patch: decode change %d op %d content: %w", i, j, err)
			}
			ops = append(ops, LineOp{Kind: OpKind(opKindByte), ID: id, Prev: prev, Next: next, Content: content})
		}
		p.Changes = append(p.Changes, FileChange{
			Path:            changePath,
			Kind:            ChangeKind(kindByte),
			TrailingNewline: trailingNewline,
			Executable:      executable,
			Blob:            blob,
			SymlinkTarget:   symlinkTarget,
			Ops:             ops,
		})
	}
	if _, err := io.ReadFull(r, p.AuthorFingerprint[:]); err != nil {
		return nil, fmt.Errorf("patch: decode author fingerprint: %w", err)
	}
	if _, err := io.ReadFull(r, p.AuthorSignature[:]); err != nil {
		return nil, fmt.Errorf("patch: decode author signature: %w", err)
	}
	return p, nil
}

func readString(r *bytes.Reader) (string, error) {
	n, err := readCount(r, "patch: decode string length")
	if err != nil {
		return "", err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// readCount reads a length-prefixed count or size and validates it
// against how many bytes actually remain in r before returning. Every
// count that goes on to size a make() — a slice length/capacity, or
// readString's buffer — is read through this, not readInt64 directly,
// so corrupted or adversarial input (e.g. a patch fetched over the
// network, or decoded from a shared bundle file) produces a clean
// decode error instead of an out-of-range allocation panic. This is a
// loose bound (it doesn't account for each element's real size, just
// caps the count at the raw byte count remaining), which is enough:
// it's what stands between a corrupted length field and make() trying
// to allocate an absurd amount, not a precise validity check — a count
// that passes this but is still too large for what follows fails
// cleanly at the next read instead.
func readCount(r *bytes.Reader, what string) (int64, error) {
	n, err := readInt64(r)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	if n < 0 || n > int64(r.Len()) {
		return 0, fmt.Errorf("%s: implausible length %d (%d bytes remain)", what, n, r.Len())
	}
	return n, nil
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
