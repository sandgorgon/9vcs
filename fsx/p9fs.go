package fsx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	p9 "github.com/sandgorgon/9p"
	"github.com/sandgorgon/9p/client"
)

// p9fs is FS backed by a 9P connection, rooted at a path already
// walked within the server's namespace (see repo.ResolveRoot).
type p9fs struct {
	c    *client.Client
	root string // namespace path this FS is rooted at, no leading/trailing slash
}

// NewP9 returns an FS backed by c, rooted at root within c's attached
// namespace.
func NewP9(c *client.Client, root string) FS {
	return &p9fs{c: c, root: strings.Trim(root, "/")}
}

func (f *p9fs) path(p string) string {
	p = strings.Trim(path.Clean("/"+p), "/")
	switch {
	case f.root == "":
		return p
	case p == "":
		return f.root
	default:
		return f.root + "/" + p
	}
}

func (f *p9fs) ReadFile(p string) ([]byte, error) {
	file, err := f.c.OpenContext(context.Background(), f.path(p), p9.OREAD)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func (f *p9fs) ReadDir(p string) ([]string, error) {
	file, err := f.c.OpenContext(context.Background(), f.path(p), p9.OREAD)
	if err != nil {
		return nil, nil // treat as absent — matches osfs's NotExist tolerance
	}
	defer file.Close()
	stats, err := file.ReadDirContext(context.Background())
	if err != nil {
		return nil, err
	}
	names := make([]string, len(stats))
	for i, st := range stats {
		names[i] = st.Name
	}
	return names, nil
}

// Stat opens p and reads its Stat back, then closes it — base 9P2000
// has no by-path stat call, only per-open-fid (see PLAN.md's library
// facts), so this costs a round trip a direct Stat call wouldn't.
// Any failure to open means "absent," same as Info's doc comment and
// every existing local Stat call site in this codebase.
func (f *p9fs) Stat(p string) (Info, error) { return f.statFull(f.path(p)) }

// statFull is Stat for a path already run through f.path — MkdirAll
// builds and checks each namespace-rooted prefix of its target
// directly, so it needs to stat those without re-rooting them a
// second time.
func (f *p9fs) statFull(full string) (Info, error) {
	file, err := f.c.OpenContext(context.Background(), full, p9.OREAD)
	if err != nil {
		return Info{}, nil
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		return Info{}, nil
	}
	return Info{Exists: true, IsDir: st.Mode.IsDir(), ModTime: time.Unix(int64(st.Mtime), 0)}, nil
}

// MkdirAll walks the namespace one segment at a time, creating
// whichever segments are missing as directories. Client.Create
// requires its parent to already exist (confirmed against
// CreateContext's implementation — no recursive create in the
// library), hence the segment-by-segment walk rather than one call.
//
// Checking existence via Stat before Create (rather than trying
// Create and inspecting the failure) sidesteps needing to recognize
// "already exists" from a 9P Rerror's plain-string message — base
// 9P2000 has no structured error code for that (see PLAN.md's library
// facts). This is check-then-act and therefore racy in the strict
// sense, but harmlessly so: the only way two callers collide here is
// racing to create the exact same not-yet-existing directory, which
// is self-healing (the loser's Create fails, but the directory it
// wanted now exists either way) and not a scenario this codebase's
// MkdirAll callers (openRaw, writeRefFileLocked, cmdInit) ever
// actually race on in practice.
func (f *p9fs) MkdirAll(p string) error { return f.mkdirAllFull(f.path(p)) }

// mkdirAllFull is MkdirAll for a path already run through f.path — Put
// and WriteAtomic need their target's parent directory to exist before
// creating a temp file in it, and already have that parent as a full,
// namespace-rooted path.
func (f *p9fs) mkdirAllFull(full string) error {
	if full == "" {
		return nil // namespace root, always already there
	}
	var built string
	for _, seg := range strings.Split(full, "/") {
		if built == "" {
			built = seg
		} else {
			built = built + "/" + seg
		}
		if info, err := f.statFull(built); err == nil && info.Exists {
			if !info.IsDir {
				return fmt.Errorf("fsx: %s: not a directory", built)
			}
			continue
		}
		file, err := f.c.CreateContext(context.Background(), built, p9.DMDIR|0o755, p9.OREAD)
		if err != nil {
			return err
		}
		file.Close()
	}
	return nil
}

// Lock creates p as an empty marker file, true exclusive-create.
//
// Every Create failure here is treated as contention (wrapping
// os.ErrExist), unconditionally — not just when a follow-up Stat
// happens to still find p present. An earlier version disambiguated
// that way, but it's racy against exactly how this method is used:
// withRefLock's critical sections are typically milliseconds, so by
// the time a follow-up Stat runs, the original holder may already have
// released the lock, leaving no way to tell "that failure was real
// contention, now resolved" apart from "that failure was something
// else" — base 9P2000 has no structured error code to fall back on
// either (see PLAN.md's library facts). Observed live: exactly this
// race turning legitimate, ordinary contention into a spurious hard
// failure under concurrent load (repo.TestConcurrentSetLocalRefCASOnlyOneWinsOverP9FS).
// Treating every failure as contention is safe because withRefLock's
// own deadline already bounds a truly stuck case; the only cost is a
// few retries within that window instead of an immediately precise
// error for a genuinely different failure (e.g. the connection itself
// dropping) — never a wrong answer, just a less specific one.
func (f *p9fs) Lock(p string) error {
	full := f.path(p)
	file, err := f.c.CreateContext(context.Background(), full, 0o644, p9.OWRITE)
	if err == nil {
		return file.Close()
	}
	return fmt.Errorf("%s: %w", full, os.ErrExist)
}

// Put writes data to a uniquely-named temp file in p's directory, then
// File.Rename publishes it over the final name — the same
// write-then-atomically-reveal pattern osfs.Put uses locally, now
// possible over 9P via github.com/sandgorgon/9p v0.8.0's
// client.File.Rename (see PLAN.md decision #9). Unlike osfs.Put's
// hard-link-based publish, WStat-driven rename overwrites rather than
// failing if the target already exists (confirmed against dirfs's
// WStat, which wraps os.Root.Rename — ordinary POSIX rename
// semantics) — harmless here specifically because content-addressed
// callers only ever call this with the correct content for path, by
// construction of having hashed it first.
func (f *p9fs) Put(p string, data []byte) error {
	ctx := context.Background()
	full := f.path(p)
	dir, base := path.Split(full)
	dir = strings.TrimSuffix(dir, "/")
	if err := f.mkdirAllFull(dir); err != nil {
		return err
	}
	tmpName, err := randomName(base)
	if err != nil {
		return err
	}
	tmpPath := path.Join(dir, tmpName)

	tmp, err := f.c.CreateContext(ctx, tmpPath, 0o444, p9.OWRITE)
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Remove()
		return err
	}
	if err := tmp.RenameContext(ctx, base); err != nil {
		tmp.Remove()
		return err
	}
	return tmp.Close()
}

// randomName returns "."+base+".<hex>.tmp" — a per-call-unique name in
// the same directory base lives in, so two concurrent Put calls (even
// for the same final content) never collide on the temp file the way
// a shared fixed name would (see rawStore.put's original local
// version of this same fix for the live bug that motivated it).
func randomName(base string) (string, error) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return "." + base + "." + hex.EncodeToString(nonce[:]) + ".tmp", nil
}

// WriteAtomic mirrors osfs.WriteAtomic: write full content to a temp
// name, then rename over the target, so a lock-free reader never
// observes a torn write. No exclusivity of its own — casWriteRef
// already gets compare-and-swap from withRefLock before ever calling
// this, matching osfs's WriteAtomic exactly.
func (f *p9fs) WriteAtomic(p string, data []byte) error {
	ctx := context.Background()
	full := f.path(p)
	dir, base := path.Split(full)
	tmpName, err := randomName(base)
	if err != nil {
		return err
	}
	tmpPath := dir + tmpName

	tmp, err := f.c.CreateContext(ctx, tmpPath, 0o644, p9.OWRITE)
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Remove()
		return err
	}
	if err := tmp.RenameContext(ctx, base); err != nil {
		tmp.Remove()
		return err
	}
	return tmp.Close()
}

// Remove deletes p if present, idempotent like osfs.Remove: absent is
// success, not an error. Existence is checked via Stat rather than
// matching the Rerror from a failed Remove, for the same reason as
// MkdirAll/Lock.
func (f *p9fs) Remove(p string) error {
	full := f.path(p)
	// OREAD, not OWRITE: removal is a metadata operation, not a content
	// one, and a Put-written file is intentionally read-only (osfs.Put
	// chmods it 0o444 the same way) — opening OWRITE here would fail
	// with a permission error on exactly the files Remove most needs to
	// reach (rawStore.remove's content-addressed objects).
	file, err := f.c.OpenContext(context.Background(), full, p9.OREAD)
	if err != nil {
		if info, statErr := f.Stat(p); statErr == nil && !info.Exists {
			return nil
		}
		return err
	}
	return file.Remove()
}

func (f *p9fs) Join(elem ...string) string { return path.Join(elem...) }
func (f *p9fs) Dir(p string) string        { return path.Dir(p) }
func (f *p9fs) IsLocal() bool              { return false }
