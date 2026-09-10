// Tree is a second, separate seam from FS — see this file's doc
// comments for why it isn't just more methods on FS.
package fsx

// TreeInfo is what Tree.Lstat/ReadDir report about one working-tree
// entry. Unlike Info (FS.Stat), it never follows a trailing symlink —
// working-tree materialization needs to tell a tracked symlink apart
// from a regular file, which nothing that uses FS.Stat (refs,
// patches) ever needs to.
type TreeInfo struct {
	Exists bool
	IsDir  bool

	IsSymlink bool
	// SymlinkTarget is populated only when IsSymlink is true, and only
	// when the target could actually be read. Empty with IsSymlink true
	// unambiguously means "target unavailable on this connection" —
	// see p9Tree's doc comment — never a real, literal empty target: no
	// symlink can be created pointing at "" in the first place, on
	// either backend.
	SymlinkTarget string

	// Executable is meaningful only when neither IsDir nor IsSymlink.
	Executable bool
}

// TreeDirEntry is one child reported by Tree.ReadDir.
type TreeDirEntry struct {
	Name string
	Info TreeInfo
}

// Tree is the working-tree materialization seam: confined to one root
// directory and symlink-aware, used by repo.WriteWorkingTree/
// ChangedFiles/WorkingFiles and nothing else in this codebase.
//
// osTree's confinement is a real OS-enforced boundary (os.Root),
// exactly at the given root. p9Tree's is weaker in one specific way,
// worth being explicit about rather than implying parity: its root
// prefix is a client-side naming convention, not a security boundary
// — the actual confinement is entirely dirfs's own os.Root, scoped to
// wherever 9sh bound the namespace region being walked (typically a
// whole launch directory via /local, not the specific repo within
// it). So an intermediate symlink inside one repo's tracked content
// could still redirect a p9Tree write to a different repo or path
// under that same bind — never outside it (that's what dirfs's fix
// actually closed), but not confined to just the repo being checked
// out either. Closing that fully would need either a client-side
// resolved-path check this package has no reliable way to make (9P's
// Qid is an opaque per-server identifier, not a path dirfs hands
// back), or 9sh binding namespace regions at repo granularity instead
// of a whole directory — out of scope here, flagged for whoever picks
// it up next.
//
// Kept separate from FS (objstore/patches and repo's ref/HEAD/lock
// storage) deliberately, not merged into it: FS's existing callers
// never need symlink awareness or root confinement — their paths are
// entirely 9vcs-internal (content-addressed hashes, validated ref
// names), never arbitrary tracked content a peer supplied. Working-
// tree paths are exactly that (an untrusted peer's patch can name any
// path, including one that plants a symlink at an intermediate
// component specifically to redirect a later write — see
// WriteWorkingTree's own doc comment for the live bug this already
// caused once), so this seam carries that confinement and awareness
// as first-class concerns instead of bolting them onto FS for every
// caller whether they need it or not.
type Tree interface {
	// Lstat reports path's own type, never following a trailing
	// symlink.
	Lstat(path string) (TreeInfo, error)
	// ReadDir returns each child of path, in no particular order, with
	// enough type information to avoid a second Lstat round trip per
	// entry during a recursive walk.
	ReadDir(path string) ([]TreeDirEntry, error)
	// ReadFile reads a regular file's content. Callers must already
	// know path isn't a symlink (via Lstat/ReadDir) — see the backend
	// doc comments for what happens otherwise.
	ReadFile(path string) ([]byte, error)
	// WriteFile creates or replaces path as a regular file, setting its
	// executable bit. If a symlink currently occupies path, callers
	// remove it first (WriteWorkingTree already does, matching its
	// existing convention) — WriteFile itself doesn't follow one.
	WriteFile(path string, data []byte, executable bool) error
	// Symlink creates path as a symlink pointing at target, replacing
	// whatever (if anything) was already there.
	Symlink(path, target string) error
	MkdirAll(path string) error
	// Remove removes path (file, symlink, or empty directory),
	// idempotent — absent is success, not an error.
	Remove(path string) error
	IsLocal() bool
}
