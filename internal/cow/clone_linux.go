package cow

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// cloneTree copies the directory src to dst, which must not exist,
// reflinking each file with FICLONE and copying where the filesystem has
// no reflinks, as on ext4. File modes and modification times are kept, so
// a clone's stat cache stays valid; directories are created owner-writable
// so a read-only source can be copied.
func cloneTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return cloneFile(src, dst, info)
	}
	if err := os.Mkdir(dst, info.Mode().Perm()|0o700); err != nil {
		return err
	}
	from, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer func() { _ = from.Close() }()
	to, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer func() { _ = to.Close() }()
	return fs.WalkDir(from.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return to.Mkdir(p, info.Mode().Perm()|0o700)
		case d.Type()&fs.ModeSymlink != 0:
			target, err := from.Readlink(p)
			if err != nil {
				return err
			}
			return to.Symlink(target, p)
		case !d.Type().IsRegular():
			return nil
		}
		if err := copyFile(from, to, p, p, info.Mode().Perm()); err != nil {
			return err
		}
		return to.Chtimes(p, info.ModTime(), info.ModTime())
	})
}

// cloneFile clones the single file or symlink src to dst.
func cloneFile(src, dst string, info fs.FileInfo) error {
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	}
	from, err := os.OpenRoot(filepath.Dir(src))
	if err != nil {
		return err
	}
	defer func() { _ = from.Close() }()
	to, err := os.OpenRoot(filepath.Dir(dst))
	if err != nil {
		return err
	}
	defer func() { _ = to.Close() }()
	name := filepath.Base(dst)
	if err := copyFile(from, to, filepath.Base(src), name, info.Mode().Perm()); err != nil {
		return err
	}
	return to.Chtimes(name, info.ModTime(), info.ModTime())
}

func copyFile(from, to *os.Root, src, dst string, perm fs.FileMode) error {
	in, err := from.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := to.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm|0o200)
	if err != nil {
		return err
	}
	if err = unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		if !errors.Is(err, unix.EOPNOTSUPP) && !errors.Is(err, unix.EXDEV) && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOTTY) {
			_ = out.Close()
			return err
		}
		_, err = io.Copy(out, in)
	}
	if e := out.Close(); err == nil {
		err = e
	}
	if err == nil && perm&0o200 == 0 {
		err = to.Chmod(dst, perm)
	}
	return err
}
