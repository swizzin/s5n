package setup

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// LinuxPathHost is the real host: stat, statfs, /proc/self/mountinfo, mkdir and
// chown. It is the only implementation that touches a machine, and the flow
// behind PathHost never calls it directly, so every branch of init is tested
// against a fixture instead.
type LinuxPathHost struct{}

func (LinuxPathHost) Stat(path string) (FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return FileInfo{}, err
	}
	out := FileInfo{IsDir: info.IsDir(), Mode: info.Mode().Perm()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		out.UID = int(st.Uid)
		out.GID = int(st.Gid)
	}
	return out, nil
}

func (LinuxPathHost) StatFS(path string) (FSInfo, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return FSInfo{}, err
	}
	block := int64(st.Bsize)
	return FSInfo{Free: int64(st.Bavail) * block, Total: int64(st.Blocks) * block}, nil
}

func (LinuxPathHost) MountInfo() ([]Mount, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseMountInfo(f)
}

func (LinuxPathHost) ReadDir(path string, max int) ([]DirEntry, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	// ReadDir(n>0) reports the end of the directory as io.EOF, the way Read
	// does; an empty directory is not a failure.
	entries, err := f.ReadDir(max + 1)
	if err != nil && !errors.Is(err, io.EOF) && len(entries) == 0 {
		return nil, false, err
	}
	more := len(entries) > max
	if more {
		entries = entries[:max]
	}
	out := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		entry := DirEntry{Name: e.Name(), IsDir: e.IsDir()}
		if !entry.IsDir {
			if info, err := e.Info(); err == nil {
				entry.Size = info.Size()
			}
		}
		out = append(out, entry)
	}
	return out, more, nil
}

func (LinuxPathHost) MkdirAll(path string, perm fs.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (LinuxPathHost) Chown(path string, uid, gid int) error {
	return os.Chown(path, uid, gid)
}

var _ PathHost = LinuxPathHost{}

// parseMountInfo reads /proc/self/mountinfo. Each line is
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
//
// and only the device, the mount point, the filesystem type and the source are
// kept, because those are what the report names. The fields box needs are all
// reachable without parsing the optional ones: everything before the "-"
// separator is fixed, and the type and source follow it.
func parseMountInfo(r io.Reader) ([]Mount, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var out []Mount
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 7 {
			continue
		}
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if sep < 6 || sep+2 >= len(fields) {
			continue
		}
		major, minor, ok := splitDevice(fields[2])
		if !ok {
			continue
		}
		out = append(out, Mount{
			Major:  major,
			Minor:  minor,
			Point:  unescapeMount(fields[4]),
			FSType: fields[sep+1],
			Source: unescapeMount(fields[sep+2]),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func splitDevice(s string) (int, int, bool) {
	maj, min, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, false
	}
	major, err := strconv.Atoi(maj)
	if err != nil {
		return 0, 0, false
	}
	minor, err := strconv.Atoi(min)
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// unescapeMount decodes the octal escapes mountinfo uses for the characters a
// path cannot carry literally: \040 is a space, and so on.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
