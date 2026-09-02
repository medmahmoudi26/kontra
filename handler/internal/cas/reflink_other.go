//go:build !linux && !darwin

package cas

// The appliance ships linux and macOS (ADR 0031 §6). Anywhere else, materialization starts
// at the hardlink rung and the store still works — slower on the first install, identical
// afterwards.
func reflinkFile(string, string) error { return errNoReflink }
