package codeindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	case ".env", "id_rsa", "gogen.conf", "credentials.json", "secrets.json":
		return true
	}
	if strings.HasPrefix(lower, ".env.") {
		return true
	}
	ext := ""
	if i := strings.LastIndex(lower, "."); i >= 0 {
		ext = lower[i+1:]
	}
	switch ext {
	case "pem", "key", "png", "jpg", "jpeg", "gif", "webp", "pdf", "zip", "gz",
		"exe", "dll", "so", "dylib", "wasm", "mp4", "mp3", "ico", "woff", "woff2":
		return true
	}
	return false
}

// fileRec is one file the walk decided to index.
type fileRec struct {
	Rel  string
	Abs  string
	Hash string
	Text string
}

// walkFiles lists the indexable files under root. A path outside root is not
// followed. Symlinks are skipped. sub, when set, limits the walk to that
// relative directory.
func walkFiles(root, sub string) ([]fileRec, error) {
	root = filepath.Clean(root)
	start := root
	if sub != "" {
		start = filepath.Join(root, filepath.FromSlash(sub))
	}
	var out []fileRec
	err := filepath.WalkDir(start, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		name := d.Name()
		if outsideLink(root, path) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path != start && skipDir(name) {
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
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > MaxFileBytes {
			return nil
		}
		rec, ok := readIndexable(path, rel)
		if ok {
			out = append(out, rec)
		}
		return nil
	})
	return out, err
}

func readIndexable(path, rel string) (fileRec, bool) {
	f, err := os.Open(path)
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
	rest, err := io.ReadAll(f)
	if err != nil {
		return fileRec{}, false
	}
	body := append(probe, rest...)
	sum := sha256.Sum256(body)
	return fileRec{
		Rel:  rel,
		Abs:  path,
		Hash: hex.EncodeToString(sum[:]),
		Text: string(body),
	}, true
}

// outsideLink reports whether an existing component between root and target
// is a link whose target leaves root. A Windows junction is not a symlink
// to EvalSymlinks, and Readlink still returns its target.
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
