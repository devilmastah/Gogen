package codeindex

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// skipDir reports whether a directory name is left out of the walk.
func skipDir(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") {
		return true
	}
	switch strings.ToLower(name) {
	case "node_modules", "vendor":
		return true
	}
	return false
}

// skipFile reports whether a file name is left out of the index.
func skipFile(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case ".env", ".git-credentials", ".netrc", ".npmrc", ".pgpass", ".pypirc",
		"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519",
		"gogen.conf", "credentials", "credentials.json", "secrets", "secrets.json":
		return true
	}
	if strings.HasPrefix(lower, ".env.") {
		return true
	}
	if strings.HasSuffix(lower, ".tfstate") || strings.HasSuffix(lower, ".tfstate.backup") {
		return true
	}
	ext := ""
	if i := strings.LastIndex(lower, "."); i >= 0 {
		ext = lower[i+1:]
	}
	switch ext {
	case "pem", "key", "p12", "pfx", "keystore", "jks", "ppk", "kdbx", "ovpn",
		"png", "jpg", "jpeg", "gif", "webp", "pdf", "zip", "gz",
		"exe", "dll", "so", "dylib", "wasm", "mp4", "mp3", "ico", "woff", "woff2":
		return true
	}
	return false
}

// fileRec is one file the walk decided to index. Chunk hashes are computed
// per chunk, so the walk does not hash the file itself.
type fileRec struct {
	Rel  string
	Text string
}

// walkFiles lists the indexable files under root. A path outside root is not
// followed. Symlinks are skipped. sub, when set, limits the walk to that
// relative directory.
func walkFiles(root string) ([]fileRec, error) {
	root = filepath.Clean(root)
	if root == "" {
		root = "."
	}
	// A working directory can itself be a symlink. Walk the resolved path so
	// WalkDir sees a directory, not a link it refuses to follow.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	// WalkDir does not follow symlinks, and the entry check below skips a
	// symlinked file or directory, so a walked path is below a real
	// directory. The Root makes the read itself safe against a swap.
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []fileRec
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && skipDir(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if skipFile(name) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || !within(root, path) {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !safeChunkPath(rel) {
			return nil
		}
		rec, ok := readIndexable(r, rel)
		if ok {
			out = append(out, rec)
		}
		return nil
	})
	return out, err
}

func readIndexable(r *os.Root, rel string) (fileRec, bool) {
	name := filepath.FromSlash(rel)
	info, err := r.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxFileBytes {
		return fileRec{}, false
	}
	f, err := r.Open(name)
	if err != nil {
		return fileRec{}, false
	}
	defer f.Close()
	probe := make([]byte, ProbeBytes)
	n, err := io.ReadFull(f, probe)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return fileRec{}, false
	}
	probe = probe[:n]
	if bytes.IndexByte(probe, 0) >= 0 {
		return fileRec{}, false
	}
	rest, err := io.ReadAll(io.LimitReader(f, int64(MaxFileBytes)+1-int64(n)))
	if err != nil || int64(n)+int64(len(rest)) > MaxFileBytes {
		return fileRec{}, false
	}
	body := append(probe, rest...)
	return fileRec{Rel: rel, Text: string(body)}, true
}

// outsideLink reports whether target, or any component between root and
// target, resolves to a location outside root. EvalSymlinks follows a whole
// chain, so a link to a link that leaves root is caught too. A target that
// does not exist is not an escape; callers handle a missing path as absent.
func outsideLink(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	if rel == "." {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err == nil {
		resolvedTarget, terr := filepath.EvalSymlinks(target)
		if terr != nil {
			// A watch event can name a file that is already gone. Resolve
			// the parent so a link in the path is still checked.
			if parent, perr := filepath.EvalSymlinks(filepath.Dir(target)); perr == nil {
				resolvedTarget = filepath.Join(parent, filepath.Base(target))
				terr = nil
			}
		}
		if terr == nil && !within(resolvedRoot, resolvedTarget) {
			return true
		}
	}
	// EvalSymlinks does not resolve every reparse point (a Windows junction,
	// for example), so also inspect each component the target path names.
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		dest, err := os.Readlink(cur)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(filepath.Dir(cur), dest)
		}
		if !within(root, filepath.Clean(dest)) {
			return true
		}
	}
	return false
}

// indexWithin reports whether root is lexically inside dir and is not
// reached through a link that leaves dir.
func indexWithin(dir, root string) bool {
	return within(dir, root) && !outsideLink(dir, root)
}

// indexEscapes reports whether root is reached through a link that leaves
// its grandparent. The index directory is working/.gogen/index, so that
// grandparent is the working directory.
func indexEscapes(root string) bool {
	return outsideLink(filepath.Dir(filepath.Dir(root)), root)
}

// within reports whether path is root or a descendant of root.
func within(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// relInside resolves userPath against root and returns a slash-separated
// relative path. An empty userPath is the whole root. A path that leaves
// root returns outside=true.
func relInside(root, userPath string) (rel string, outside bool) {
	userPath = strings.TrimSpace(userPath)
	if userPath == "" {
		return "", false
	}
	var abs string
	if filepath.IsAbs(userPath) {
		abs = filepath.Clean(userPath)
	} else {
		abs = filepath.Clean(filepath.Join(root, userPath))
	}
	if !within(root, abs) {
		return "", true
	}
	r, err := filepath.Rel(root, abs)
	if err != nil || r == "." {
		return "", false
	}
	return filepath.ToSlash(r), false
}
