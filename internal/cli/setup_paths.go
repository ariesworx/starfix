package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/proto"
)

// Setup reads and rewrites files a repository may have planted. A
// committed link could make it copy a secret into a file the person then
// commits (.mcp.json -> ~/.docker/config.json), or write their global
// hook without --global (.claude -> ~/.claude). So:
//
//   - In the project, no path component below the repository root, the
//     file included, may be a symbolic link (or, on Windows, a junction):
//     setup refuses rather than follow one.
//   - The person's own files (--global, a desktop app's config) may be
//     links, as a dotfiles repository makes them, but only to a file or
//     directory inside their home directory that they own. Setup then
//     reads and replaces the link's target and keeps the link.
//
// New files are 0644 in the project, which is committed and holds no
// secret, with directories 0755; the person's are 0600 in directories
// 0700, because harnesses keep other servers' tokens in them. An
// existing file keeps its mode.

// fileBase is the directory setup's files are under, and how they may be
// reached.
type fileBase struct {
	dir string
	// user marks the person's own files: links into home are followed,
	// and new files are private.
	user bool
	home string
}

// newMode is the mode of a file setup creates, and of its directories.
func (b fileBase) newMode() (file, dir fs.FileMode) {
	if b.user {
		return 0o600, 0o700
	}
	return 0o644, 0o755
}

const byHand = "or make the edit by hand: run the same setup without --write to print it"

// resolve checks path, which is under b.dir, and returns the path to
// read and write: path itself, or for the person's files the target of
// any link on the way.
func (b fileBase) resolve(path string) (string, error) {
	rel, err := filepath.Rel(b.dir, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("setup: %s is not under %s", path, b.dir)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	cur := b.dir
	for i, part := range parts {
		next := filepath.Join(cur, part)
		fi, err := os.Lstat(next)
		if errors.Is(err, fs.ErrNotExist) {
			cur = filepath.Join(append([]string{cur}, parts[i:]...)...)
			break
		}
		if err != nil {
			return "", proto.Errf(proto.CodeInvalid, "check the file's permissions", fmt.Sprintf("stat %s: %v", next, err))
		}
		last := i == len(parts)-1
		if fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			if next, err = b.follow(next); err != nil {
				return "", err
			}
			if fi, err = os.Stat(next); err != nil {
				return "", proto.Errf(proto.CodeInvalid, "check the file's permissions", fmt.Sprintf("stat %s: %v", next, err))
			}
		}
		switch {
		case last && !fi.Mode().IsRegular():
			return "", proto.Errf(proto.CodeInvalid, "move it aside, "+byHand, next+" is not a regular file")
		case !last && !fi.IsDir():
			return "", proto.Errf(proto.CodeInvalid, "move it aside, "+byHand, next+" is not a directory")
		}
		cur = next
	}
	if !b.user {
		if err := b.noIndirection(path, cur); err != nil {
			return "", err
		}
	}
	return cur, nil
}

// follow resolves a link among the person's own files: it must lead to
// something of theirs inside their home directory.
func (b fileBase) follow(link string) (string, error) {
	if !b.user {
		return "", proto.Errf(proto.CodeInvalid, "replace the link with a regular file or directory, "+byHand,
			fmt.Sprintf("refusing %s: it is a symbolic link, and setup never reads or writes a project file through one", link))
	}
	target, err := filepath.EvalSymlinks(link)
	if err != nil {
		return "", proto.Errf(proto.CodeInvalid, "repair or remove the link, "+byHand,
			fmt.Sprintf("refusing %s: it is a symbolic link that does not resolve: %v", link, err))
	}
	home := b.home
	if h, err := filepath.EvalSymlinks(home); err == nil {
		home = h
	}
	if rel, err := filepath.Rel(home, target); home == "" || err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", proto.Errf(proto.CodeInvalid, "point the link at a file of yours inside your home directory, "+byHand,
			fmt.Sprintf("refusing %s: it is a symbolic link to %s, outside your home directory", link, target))
	}
	fi, err := os.Stat(target)
	if err != nil {
		return "", proto.Errf(proto.CodeInvalid, "check the file's permissions", fmt.Sprintf("stat %s: %v", target, err))
	}
	if !client.OwnedByUser(fi) {
		return "", proto.Errf(proto.CodeInvalid, "point the link at a file you own, "+byHand,
			fmt.Sprintf("refusing %s: it is a symbolic link to %s, which another user owns", link, target))
	}
	return target, nil
}

// noIndirection is a second check for a project file: the deepest part
// of path that exists must resolve to itself below the resolved root. It
// catches what Lstat does not report as a link, such as a Windows
// junction.
func (b fileBase) noIndirection(path, cur string) error {
	exists := cur
	for {
		if _, err := os.Lstat(exists); err == nil || exists == b.dir {
			break
		}
		exists = filepath.Dir(exists)
	}
	rel, err := filepath.Rel(b.dir, exists)
	if err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	root, err1 := filepath.EvalSymlinks(b.dir)
	got, err2 := filepath.EvalSymlinks(exists)
	if err := errors.Join(err1, err2); err != nil {
		return proto.Errf(proto.CodeInvalid, "check the file's permissions", fmt.Sprintf("resolve %s: %v", path, err))
	}
	want := filepath.Join(root, rel)
	if got == want || runtime.GOOS == "windows" && strings.EqualFold(got, want) {
		return nil
	}
	return proto.Errf(proto.CodeInvalid, "replace the link with a regular file or directory, "+byHand,
		fmt.Sprintf("refusing %s: it leads to %s through a link, and setup never reads or writes a project file through one", path, got))
}

// readConfig reads the file at path, which resolve returned; a missing
// one is empty, with the mode a new file gets.
func (b fileBase) readConfig(path string) ([]byte, fs.FileMode, error) {
	newFile, _ := b.newMode()
	f, err := os.OpenFile(path, os.O_RDONLY|noFollow, 0) //nolint:gosec // checked by resolve; links are not followed
	if errors.Is(err, fs.ErrNotExist) {
		return nil, newFile, nil
	}
	if err != nil {
		return nil, 0, proto.Errf(proto.CodeInvalid, "check the file's permissions, "+byHand, fmt.Sprintf("read %s: %v", path, err))
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err == nil && !st.Mode().IsRegular() {
		err = errors.New("not a regular file")
	}
	var data []byte
	if err == nil {
		data, err = readAll(f)
	}
	if err != nil {
		return nil, 0, proto.Errf(proto.CodeInvalid, "check the file's permissions, "+byHand, fmt.Sprintf("read %s: %v", path, err))
	}
	return data, st.Mode().Perm(), nil
}

// maxConfig caps a config file setup reads.
const maxConfig = 16 << 20

func readAll(f *os.File) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(f, maxConfig+1))
	if err == nil && len(b) > maxConfig {
		err = errors.New("larger than 16 MiB")
	}
	return b, err
}

// removeConfig deletes a file setup emptied.
func removeConfig(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return proto.Errf(proto.CodeInvalid, "check the directory's permissions", fmt.Sprintf("remove %s: %v", path, err))
	}
	return nil
}

// writeConfig replaces dest, which resolve returned for path, atomically
// with data, in mode. It checks path again once the directories exist, so a
// link put in place meanwhile is not written through.
func (b fileBase) writeConfig(path, dest string, data []byte, mode fs.FileMode) error {
	fail := func(err error) error {
		return proto.Errf(proto.CodeInvalid, "check the directory's permissions", fmt.Sprintf("write %s: %v", path, err))
	}
	dir := filepath.Dir(dest)
	_, dirMode := b.newMode()
	if err := os.MkdirAll(dir, dirMode); err != nil { //nolint:gosec // checked by resolve
		return fail(err)
	}
	if again, err := b.resolve(path); err != nil || again != dest {
		if err == nil {
			err = proto.Errf(proto.CodeInvalid, "run setup again", fmt.Sprintf("%s changed while setup ran", path))
		}
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(dest)+".tmp-*")
	if err != nil {
		return fail(err)
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp, mode)); err != nil {
		_ = os.Remove(tmp)
		return fail(err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return fail(err)
	}
	return nil
}
