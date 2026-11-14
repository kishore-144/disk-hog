//go:build linux

package fsutil

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// realFS are the filesystem types worth showing; everything else in
// mountinfo is a pseudo filesystem (proc, cgroup, tmpfs, squashfs snaps...).
var realFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true, "f2fs": true,
	"bcachefs": true, "jfs": true, "reiserfs": true, "vfat": true, "exfat": true, "ntfs": true, "ntfs3": true,
	"fuseblk": true, "nfs": true, "nfs4": true, "cifs": true, "smb3": true, "9p": true, "virtiofs": true,
	"fuse.sshfs": true, "fuse.rclone": true, "hfsplus": true, "apfs": true, "nilfs2": true,
}

// Disks lists mounted real filesystems, one entry per device.
func Disks() []Disk {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return statOnly("/")
	}
	defer f.Close()
	seen := map[string]bool{}
	var out []Disk
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		// id parent maj:min root mountpoint opts [optional...] - fstype source superopts
		line := sc.Text()
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}
		pre, post := strings.Fields(line[:sep]), strings.Fields(line[sep+3:])
		if len(pre) < 5 || len(post) < 2 {
			continue
		}
		mount, fstype, source := unescape(pre[4]), post[0], post[1]
		if !realFS[fstype] && !(fstype == "overlay" && mount == "/") {
			continue
		}
		key := source
		if !strings.HasPrefix(source, "/dev/") {
			key = source + "@" + mount
		}
		if seen[key] {
			continue
		}
		var st syscall.Statfs_t
		if syscall.Statfs(mount, &st) != nil || st.Blocks == 0 {
			continue
		}
		seen[key] = true
		out = append(out, fromStatfs(mount, source, fstype, &st))
	}
	if len(out) == 0 {
		return statOnly("/")
	}
	return out
}

func statOnly(p string) []Disk {
	var st syscall.Statfs_t
	if syscall.Statfs(p, &st) != nil {
		return nil
	}
	return []Disk{fromStatfs(p, "", "", &st)}
}

func fromStatfs(mount, dev, fstype string, st *syscall.Statfs_t) Disk {
	bs := int64(st.Bsize)
	d := Disk{Mount: mount, Device: dev, FSType: fstype,
		Total: int64(st.Blocks) * bs, Free: int64(st.Bfree) * bs, Avail: int64(st.Bavail) * bs}
	d.Used = d.Total - d.Free
	return d
}

// unescape decodes mountinfo's octal escapes (\040 for a space).
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
