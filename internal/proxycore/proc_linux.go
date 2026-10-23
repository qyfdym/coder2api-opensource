//go:build linux

package proxycore

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// sysProcAttr 网关进程退出（包括被宝塔 / systemd 直接 kill -9）时内核跟着退出。
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

func terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }

// ours 进程的命令行里带着我们的运行目录，才认定是上次留下的内核。
func ours(pid int, runDir string) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(runDir)
	if err != nil {
		abs = runDir
	}
	cmdline := strings.ReplaceAll(string(b), "\x00", " ")
	return strings.Contains(cmdline, abs) || strings.Contains(cmdline, runDir)
}
