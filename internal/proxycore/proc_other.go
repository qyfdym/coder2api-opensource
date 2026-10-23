//go:build !linux

package proxycore

import (
	"os"
	"syscall"
)

func sysProcAttr() *syscall.SysProcAttr { return nil }

// terminate 非 Linux 平台没有可靠的优雅退出信号，直接结束。
func terminate(p *os.Process) error { return p.Kill() }

// ours 非 Linux 平台无法可靠确认 PID 属于上次的内核，不去动它（生产环境是 Linux）。
func ours(int, string) bool { return false }
