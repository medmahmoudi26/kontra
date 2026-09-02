//go:build !unix

package appliance

import (
	"errors"
	"syscall"
)

// The appliance ships linux and macOS (ADR 0031 §6). These exist so the package still COMPILES
// elsewhere, and they are deliberately honest about what they cannot do: without process groups
// there is no way to reach a grandchild, so `Child.Stop` can only signal the child itself and
// `groupAlive` cannot answer the orphan question at all.
func childProcAttr() *syscall.SysProcAttr { return nil }

func signalGroup(int, syscall.Signal) error {
	return errors.New("process groups are not available on this platform; the appliance targets linux and macOS")
}

func groupAlive(int) bool { return false }

func isGone(error) bool { return false }
