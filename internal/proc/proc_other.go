//go:build !linux

package proc

import "syscall"

// ChildProcAttr returns no special attributes on non-Linux platforms.
func ChildProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }
