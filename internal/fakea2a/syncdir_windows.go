package fakea2a

// syncDir is a no-op on Windows, which cannot flush a directory handle
// (FlushFileBuffers on a directory is "Access is denied"). NTFS journals
// directory entries, and the log itself is synced after creation.
func syncDir(string) error { return nil }
