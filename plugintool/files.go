package plugintool

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// The engine's names and limits for a plugin folder.
const (
	// ManifestFile is the manifest's name in a plugin folder.
	ManifestFile = "basalt-plugin.json"
	// SignatureFile is the signature's name, next to the manifest.
	SignatureFile = "basalt-plugin.json.sig"

	maxManifestBytes  = 1 << 20
	maxRelativePath   = 255
	signatureFileMode = 0o644
)

// relativePathPattern is the manifest's rule for a path in files: slash
// separated, no segment starting with a dot.
var relativePathPattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*$`)

func validRelativePath(p string) bool {
	return len(p) <= maxRelativePath && relativePathPattern.MatchString(p)
}

// readRegular reads a regular file of at most limit bytes without following a
// final symlink.
func readRegular(p string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(p, noFollowFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("%s cannot be opened, or is a symlink", p)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	if fi.Size() > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", p, limit)
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%s cannot be read", p)
	}
	return b, nil
}

// hashFile is the manifest's form of a file's hash: sha256:<lowercase hex>.
func hashFile(p string) (string, error) {
	f, err := os.OpenFile(p, noFollowFlags, 0)
	if err != nil {
		return "", fmt.Errorf("%s cannot be opened, or is a symlink", p)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", p)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("%s cannot be read", p)
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}

// folderFiles lists every file the folder ships, as the slash-separated
// paths the manifest's files uses, sorted. The manifest and its signature are
// not shipped files. A symlink, a special file, an empty directory and a path
// the manifest cannot name are refused, because the engine refuses the same
// folder: it takes only the listed files and the directories that lead to them.
func folderFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("%s cannot be read", p)
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return fmt.Errorf("%s cannot be read", p)
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestFile || rel == SignatureFile {
			return nil
		}
		if d.IsDir() {
			return checkDirNotEmpty(p)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is a symlink or not a regular file; a plugin folder holds regular files only", p)
		}
		if !validRelativePath(rel) {
			return fmt.Errorf("%s cannot be listed in the manifest (letters, digits, '.', '_' and '-', no segment starting with a dot, at most %d characters); remove it from the folder", p, maxRelativePath)
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

func checkDirNotEmpty(p string) error {
	des, err := os.ReadDir(p)
	if err != nil {
		return fmt.Errorf("%s cannot be read", p)
	}
	if len(des) == 0 {
		return fmt.Errorf("%s is an empty directory; the engine refuses a folder with a directory no listed file needs", p)
	}
	return nil
}

// writeFileAtomic writes dir/name through a temporary file and a rename.
func writeFileAtomic(dir, name string, b []byte, mode os.FileMode) error {
	path := filepath.Join(dir, name)
	f, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return fmt.Errorf("%s cannot be written", path)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("%s cannot be written", path)
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return fmt.Errorf("%s cannot be written", path)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("%s cannot be written", path)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%s cannot be written", path)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("%s cannot be written", path)
	}
	ok = true
	return nil
}
