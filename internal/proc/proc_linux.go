//go:build linux

// Package proc carries platform helpers for child-process hygiene.
package proc

import "syscall"

// ChildProcAttr returns SysProcAttr that SIGKILLs the child when the
// Vigilant process dies — even on SIGKILL/panic — so recorder/detector
// ffmpegs can never be orphaned into writing segments forever.
func ChildProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
