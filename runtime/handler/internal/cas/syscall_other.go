//go:build !unix

package cas

// The appliance ships linux and macOS (ADR 0031 §6). Everywhere else the store still works;
// it just cannot tell a full disk from any other write failure, which is worth knowing when
// reading a bug report from such a platform.
func isNoSpace(error) bool { return false }

func freeBytes(string) (uint64, bool) { return 0, false }
