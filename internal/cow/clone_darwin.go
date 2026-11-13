package cow

import "golang.org/x/sys/unix"

// cloneTree clones the directory src to dst, which must not exist, with
// one APFS clonefile call. Modes and times are preserved.
func cloneTree(src, dst string) error { return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW) }
