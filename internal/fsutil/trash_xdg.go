//go:build unix && !darwin

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Trash moves p to the freedesktop.org trash, so it shows up in Nautilus,
// Nemo, Dolphin and friends and can be restored from there.
//
// Items on the home filesystem go to $XDG_DATA_HOME/Trash. Items on other
// filesystems go to that filesystem's own $topdir/.Trash-$uid, as the spec
// requires, because moving across filesystems would mean a full copy.
func Trash(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		data = filepath.Join(home, ".local", "share")
	}
	home := filepath.Join(data, "Trash")
	if err := ensureTrash(home); err != nil {
		return err
	}
	if sameDev(fi, home) {
		return trashInto(home, p, p)
	}
	top := mountPoint(p, fi)
	dir := filepath.Join(top, ".Trash-"+strconv.Itoa(os.Getuid()))
	if err := ensureTrash(dir); err != nil {
		return fmt.Errorf("no trash on %s: %w (use delete instead)", top, err)
	}
	rel, err := filepath.Rel(top, p)
	if err != nil {
		return err
	}
	return trashInto(dir, p, rel)
}

func ensureTrash(dir string) error {
	for _, sub := range []string{"files", "info"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return err
		}
	}
	return nil
}

func sameDev(fi os.FileInfo, dir string) bool {
	di, err := os.Stat(dir)
	if err != nil {
		return false
	}
	a, ok1 := fi.Sys().(*syscall.Stat_t)
	b, ok2 := di.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && a.Dev == b.Dev
}

// mountPoint walks up from p while the device stays the same.
func mountPoint(p string, fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "/"
	}
	cur := filepath.Dir(p)
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return cur
		}
		pi, err := os.Lstat(parent)
		if err != nil {
			return cur
		}
		ps, ok := pi.Sys().(*syscall.Stat_t)
		if !ok || ps.Dev != st.Dev {
			return cur
		}
		cur = parent
	}
}

// trashInto reserves a unique name by creating the .trashinfo file with
// O_EXCL, as the spec describes, then renames p into files/.
func trashInto(dir, p, infoPath string) error {
	base := filepath.Base(p)
	name := base
	var info *os.File
	for i := 1; ; i++ {
		f, err := os.OpenFile(filepath.Join(dir, "info", name+".trashinfo"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			info = f
			break
		}
		if !errors.Is(err, os.ErrExist) || i > 10000 {
			return err
		}
		name = base + "." + strconv.Itoa(i)
	}
	_, werr := fmt.Fprintf(info, "[Trash Info]\nPath=%s\nDeletionDate=%s\n",
		EscapePath(infoPath), time.Now().Format("2006-01-02T15:04:05"))
	cerr := info.Close()
	infoFile := filepath.Join(dir, "info", name+".trashinfo")
	if werr != nil || cerr != nil {
		os.Remove(infoFile)
		return errors.Join(werr, cerr)
	}
	if err := os.Rename(p, filepath.Join(dir, "files", name)); err != nil {
		os.Remove(infoFile)
		return err
	}
	return nil
}
