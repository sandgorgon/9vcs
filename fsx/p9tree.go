package fsx

import (
	"context"
	"io"
	"path"
	"strings"

	p9 "github.com/sandgorgon/9p"
	"github.com/sandgorgon/9p/client"
)

// p9Tree is Tree backed by a 9P connection negotiated with
// client.WithUnixExtensions() (9P2000.u) — see repo.ResolveRoot,
// which dials with that option and passes the resulting Client and
// its attached root Fid here.
//
// Symlink read/write needs the .u extension; a connection that fell
// back to plain 9P2000 (server doesn't support .u) can still detect a
// symlink's presence — Qid.Type/Mode's DMSYMLINK bit are core 9P2000
// fields, not .u-gated — but can't read its target (Stat's Extension
// field is only sent when .u negotiated) or create one
// (Client.Symlink fails outright without .u). Lstat/ReadDir report
// IsSymlink correctly either way; SymlinkTarget comes back empty when
// unavailable, which is never ambiguous with a real target (see
// TreeInfo's doc comment), and Symlink's own error surfaces plainly
// when .u wasn't negotiated.
//
// Reads a path's own metadata via Fid.Walk+Stat, deliberately not
// Client.Open — dirfs (correctly) rejects Topen on a symlink itself
// (a well-behaved client Stats/Walks one, never opens it), so an
// Open-based stat would simply fail on every symlink instead of
// reporting it.
type p9Tree struct {
	c    *client.Client
	root *client.Fid // this connection's attached root, from Client.Attach
	base string      // namespace path this Tree is rooted at, no leading/trailing slash
}

// NewP9Tree returns a Tree backed by c, rooted at base within the
// namespace root reaches (root must be c's own attached root Fid, or
// a Fid walked from it).
func NewP9Tree(c *client.Client, root *client.Fid, base string) Tree {
	return &p9Tree{c: c, root: root, base: strings.Trim(base, "/")}
}

func (t *p9Tree) path(p string) string {
	p = strings.Trim(path.Clean("/"+p), "/")
	switch {
	case t.base == "":
		return p
	case p == "":
		return t.base
	default:
		return t.base + "/" + p
	}
}

// walkTo returns a Fid positioned at full (root-relative, "" meaning
// the root itself), or an error if any element doesn't exist — same
// contract as Client.Open's internal walk, but without the follow-up
// Topen that would reject a symlink.
func (t *p9Tree) walkTo(full string) (*client.Fid, error) {
	if full == "" {
		return t.root.WalkContext(context.Background())
	}
	return t.root.WalkContext(context.Background(), strings.Split(full, "/")...)
}

func infoFromStat(st p9.Stat) TreeInfo {
	info := TreeInfo{
		Exists:    true,
		IsDir:     st.Mode.IsDir(),
		IsSymlink: st.Mode.IsSymlink(),
	}
	if info.IsSymlink {
		info.SymlinkTarget = st.Extension // "" means unavailable, not a real empty target — see TreeInfo's doc comment
	} else if !info.IsDir {
		info.Executable = st.Mode&0o111 != 0
	}
	return info
}

func (t *p9Tree) Lstat(p string) (TreeInfo, error) {
	fid, err := t.walkTo(t.path(p))
	if err != nil {
		return TreeInfo{}, nil // absent
	}
	defer fid.ClunkContext(context.Background())
	st, err := fid.StatContext(context.Background())
	if err != nil {
		return TreeInfo{}, nil
	}
	return infoFromStat(st), nil
}

// ReadDir lists p's children via Open+Read on the directory itself
// (safe — Topen only rejects a symlink target, and a directory is
// never one), with each entry's own Stat already carrying everything
// Lstat would otherwise need a second round trip for.
func (t *p9Tree) ReadDir(p string) ([]TreeDirEntry, error) {
	file, err := t.c.OpenContext(context.Background(), t.path(p), p9.OREAD)
	if err != nil {
		return nil, nil // absent
	}
	defer file.Close()
	stats, err := file.ReadDirContext(context.Background())
	if err != nil {
		return nil, err
	}
	out := make([]TreeDirEntry, len(stats))
	for i, st := range stats {
		out[i] = TreeDirEntry{Name: st.Name, Info: infoFromStat(st)}
	}
	return out, nil
}

func (t *p9Tree) ReadFile(p string) ([]byte, error) {
	file, err := t.c.OpenContext(context.Background(), t.path(p), p9.OREAD)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

// WriteFile always creates p fresh — removing any existing entry
// first — rather than truncating and rewriting one in place: there's
// no way to change an existing file's permission bits over 9P from
// this client (only Rename/Remove are exposed on *client.File, no
// general WStat — see PLAN.md decision #9), so this is how the
// executable bit gets applied correctly even when overwriting what's
// already there, matching osTree's explicit-Chmod approach in spirit.
func (t *p9Tree) WriteFile(p string, data []byte, executable bool) error {
	ctx := context.Background()
	full := t.path(p)
	dir, _ := path.Split(full)
	dir = strings.TrimSuffix(dir, "/")
	if err := t.mkdirAllFull(dir); err != nil {
		return err
	}
	if err := t.removeIfPresent(full); err != nil {
		return err
	}
	perm := p9.Mode(0o644)
	if executable {
		perm = 0o755
	}
	file, err := t.c.CreateContext(ctx, full, perm, p9.OWRITE)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// Symlink clears whatever currently sits at p first — Client.Symlink
// fails outright if a regular file (or a stale symlink to something
// else) is already there, matching osTree/WriteWorkingTree's existing
// remove-then-create convention.
func (t *p9Tree) Symlink(p, target string) error {
	full := t.path(p)
	dir, _ := path.Split(full)
	dir = strings.TrimSuffix(dir, "/")
	if err := t.mkdirAllFull(dir); err != nil {
		return err
	}
	if err := t.removeIfPresent(full); err != nil {
		return err
	}
	_, err := t.c.SymlinkContext(context.Background(), full, target)
	return err
}

func (t *p9Tree) MkdirAll(p string) error { return t.mkdirAllFull(t.path(p)) }

// mkdirAllFull walks full one segment at a time, creating whichever
// segments are missing as directories — Client.Create requires its
// parent to already exist (no recursive create in the library), hence
// the segment-by-segment walk rather than one call. See fsx.p9fs's
// identically-shaped mkdirAllFull for the same reasoning in more
// detail; kept as a separate copy here rather than shared plumbing
// since Tree and FS are deliberately independent seams (see Tree's
// doc comment).
func (t *p9Tree) mkdirAllFull(full string) error {
	if full == "" {
		return nil
	}
	var built string
	for _, seg := range strings.Split(full, "/") {
		if built == "" {
			built = seg
		} else {
			built = built + "/" + seg
		}
		if fid, err := t.walkTo(built); err == nil {
			st, statErr := fid.StatContext(context.Background())
			fid.ClunkContext(context.Background())
			if statErr == nil && st.Mode.IsDir() {
				continue
			}
		}
		file, err := t.c.CreateContext(context.Background(), built, p9.DMDIR|0o755, p9.OREAD)
		if err != nil {
			return err
		}
		file.Close()
	}
	return nil
}

// removeIfPresent removes full if something is there, via Fid.Walk+
// Remove rather than Client.Open+Remove — the existing entry may
// itself be a symlink, which Topen rejects.
func (t *p9Tree) removeIfPresent(full string) error {
	fid, err := t.walkTo(full)
	if err != nil {
		return nil // absent, nothing to clear
	}
	return fid.RemoveContext(context.Background())
}

func (t *p9Tree) Remove(p string) error { return t.removeIfPresent(t.path(p)) }

func (t *p9Tree) IsLocal() bool { return false }
