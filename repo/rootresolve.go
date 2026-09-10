package repo

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"

	p9 "github.com/sandgorgon/9p"
	"github.com/sandgorgon/9p/client"

	"github.com/sandgorgon/9vcs/fsx"
)

// namespaceConn is the raw dialed state ResolveRoot returns for the
// p9fs case, so OpenFS can build fsx.Tree from the exact same
// connection/root Fid fsx.FS already uses — one dial, not two.
type namespaceConn struct {
	c    *client.Client
	root *client.Fid
}

// ResolveRoot implements PLAN.md decision #9's namespace-first root
// resolution for an explicit directory argument (from the CLI's -C
// flag):
//
//  1. $_9SH_UNIX_SOCK unset, or dialing it fails → step 3.
//  2. Attach (negotiating 9P2000.u, for symlink support — see
//     fsx.Tree's doc comment; a server that doesn't understand it
//     falls back to plain 9P2000 gracefully), then walk: a relative
//     explicit is rooted at "local" (9sh's real-launch-directory
//     bind); an absolute explicit is walked as given, which may
//     resolve to anything else 9sh has bound (a future /n/<host>
//     repo, e.g.). Walk succeeds → the p9fs backend, rooted at the
//     walked namespace path.
//  3. explicit resolved as a literal OS path, exactly like today's
//     os.Getwd()-relative behavior.
//
// repo.Find (the implicit, no-argument case) does not call this at
// all — see PLAN.md decision #9: it stays on today's os.Getwd()
// behavior, untouched.
func ResolveRoot(explicit string) (fsx.FS, string, error) {
	fs, _, dir, err := resolveRootFull(explicit)
	return fs, dir, err
}

// resolveRootFull is ResolveRoot plus the raw namespaceConn (nil for
// the OS-path case) OpenFS needs to also build a Tree.
func resolveRootFull(explicit string) (fsx.FS, *namespaceConn, string, error) {
	if sock := os.Getenv("_9SH_UNIX_SOCK"); sock != "" {
		if fs, nc, dir, ok := resolveNamespace(sock, explicit); ok {
			return fs, nc, dir, nil
		}
	}
	fs, dir, err := resolveOS(explicit)
	return fs, nil, dir, err
}

func resolveNamespace(sock, explicit string) (fsx.FS, *namespaceConn, string, bool) {
	c, err := client.Dial("unix", sock, client.WithUnixExtensions())
	if err != nil {
		return nil, nil, "", false
	}
	root, err := c.AttachContext(context.Background(), "9vcs", "")
	if err != nil {
		c.Close()
		return nil, nil, "", false
	}
	nsPath := namespacePath(explicit)
	f, err := c.OpenContext(context.Background(), nsPath, p9.OREAD)
	if err != nil {
		c.Close()
		return nil, nil, "", false
	}
	f.Close()
	return fsx.NewP9(c, ""), &namespaceConn{c: c, root: root}, nsPath, true
}

// namespacePath maps explicit onto the path ResolveRoot walks: a
// relative explicit is rooted under "local" (9sh's real-launch-
// directory bind — see cmd/9sh/main.go's bootstrap); an absolute
// explicit is walked as given, trimmed of its leading slash to match
// fsx.FS's forward-slash, root-relative path convention.
func namespacePath(explicit string) string {
	if explicit == "" {
		return "local"
	}
	if path.IsAbs(explicit) {
		return strings.TrimPrefix(path.Clean(explicit), "/")
	}
	return path.Join("local", explicit)
}

func resolveOS(explicit string) (fsx.FS, string, error) {
	dir := explicit
	if dir == "" || !filepath.IsAbs(dir) {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, "", err
		}
		dir = filepath.Join(cwd, explicit)
	}
	return fsx.NewOS(""), dir, nil
}

// FindAt is repo.Find's -C-driven counterpart: root is resolved via
// ResolveRoot (namespace-first when $_9SH_UNIX_SOCK is set and explicit
// resolves there, an OS path otherwise), then walked up looking for
// .9vcs the same way Find does — using fs.Join/fs.Dir so the walk-up
// works identically against either backend. Tree (working-tree
// materialization) is built from the same resolved connection/root
// once the actual repo root is known — see OpenFSAt.
func FindAt(explicit string) (*Repo, error) {
	fs, nc, dir, err := resolveRootFull(explicit)
	if err != nil {
		return nil, err
	}
	for {
		candidate := fs.Join(dir, DotDir)
		if info, err := fs.Stat(candidate); err == nil && info.Exists && info.IsDir {
			return openFSAt(fs, nc, dir)
		}
		parent := fs.Dir(dir)
		if parent == dir {
			return nil, ErrNotARepo
		}
		dir = parent
	}
}
