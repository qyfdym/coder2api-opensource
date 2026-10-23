package proxycore

// 内核进程：启动、就绪检测、日志收集、停止、退出监视。

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	readyTimeout  = 15 * time.Second
	readyInterval = 150 * time.Millisecond
	stopGrace     = 3 * time.Second
)

// ring 最近 n 行内核日志。
type ring struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newRing(n int) *ring { return &ring{max: n} }

func (r *ring) add(line string) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return
	}
	if len(line) > 600 {
		line = line[:600] + "…"
	}
	r.mu.Lock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = append([]string(nil), r.lines[len(r.lines)-r.max:]...)
	}
	r.mu.Unlock()
}

func (r *ring) tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.lines) {
		n = len(r.lines)
	}
	return append([]string(nil), r.lines[len(r.lines)-n:]...)
}

// coreProc 一个运行中的内核进程。
type coreProc struct {
	cmd       *exec.Cmd
	done      chan struct{} // 进程退出后关闭
	exitErr   error
	started   time.Time
	stopping  atomic.Bool // 由我们主动停止（不触发自动重启）
	ctrl      string
	secret    string
	pidFile   string
	waitGroup sync.WaitGroup // 日志读取 goroutine
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// freeLocalPort 在 127.0.0.1 上找一个当前空闲的端口。
func freeLocalPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// portFree 端口当前能否在 127.0.0.1 上绑定。
func portFree(port int) bool {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// validateConfig 用内核自己的 -t 检查配置文件，失败时返回内核输出的最后几行。
func validateConfig(ctx context.Context, bin, runDir, cfgFile string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-t", "-d", runDir, "-f", cfgFile).CombinedOutput()
	if err == nil && strings.Contains(string(out), "test is successful") {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	msg := strings.TrimSpace(strings.Join(lines, " | "))
	if msg == "" && err != nil {
		msg = err.Error()
	}
	return fmt.Errorf("内核拒绝了生成的配置：%s", truncateStr(msg, 400))
}

// startProc 启动内核并等它的控制接口就绪。
func startProc(ctx context.Context, bin, runDir, cfgFile, ctrl, secret string, logs *ring) (*coreProc, error) {
	cmd := exec.Command(bin, "-d", runDir, "-f", cfgFile)
	cmd.Dir = runDir
	cmd.SysProcAttr = sysProcAttr()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动内核失败：%v", err)
	}
	p := &coreProc{cmd: cmd, done: make(chan struct{}), started: time.Now(), ctrl: ctrl, secret: secret,
		pidFile: filepath.Join(runDir, "mihomo.pid")}
	_ = os.WriteFile(p.pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	p.waitGroup.Add(1)
	go func() {
		defer p.waitGroup.Done()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			logs.add(sc.Text())
		}
		_, _ = io.Copy(io.Discard, stdout)
	}()
	go func() {
		p.waitGroup.Wait()
		p.exitErr = cmd.Wait()
		_ = os.Remove(p.pidFile)
		close(p.done)
	}()

	api := &apiClient{base: "http://" + ctrl, secret: secret, hc: &http.Client{Timeout: 2 * time.Second}}
	deadline := time.Now().Add(readyTimeout)
	for {
		select {
		case <-p.done:
			return nil, fmt.Errorf("内核启动后马上退出了（%v）：%s", p.exitErr, strings.Join(logs.tail(3), " | "))
		case <-ctx.Done():
			p.stop()
			return nil, ctx.Err()
		case <-time.After(readyInterval):
		}
		if _, err := api.version(ctx); err == nil {
			return p, nil
		}
		if time.Now().After(deadline) {
			p.stop()
			return nil, fmt.Errorf("内核 %s 内没有就绪：%s", readyTimeout, strings.Join(logs.tail(3), " | "))
		}
	}
}

// stop 结束进程（先礼后兵）并等它退出。
func (p *coreProc) stop() {
	if p == nil {
		return
	}
	p.stopping.Store(true)
	select {
	case <-p.done:
		return
	default:
	}
	if err := terminate(p.cmd.Process); err != nil {
		_ = p.cmd.Process.Kill()
	}
	select {
	case <-p.done:
	case <-time.After(stopGrace):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func (p *coreProc) alive() bool {
	if p == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// killStale 清掉上次网关异常退出时留下的内核进程（只在能确认是我们启动的那个时才动手）。
func killStale(runDir string) {
	pidFile := filepath.Join(runDir, "mihomo.pid")
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	_ = os.Remove(pidFile)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return
	}
	if ours(pid, runDir) {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Kill()
			time.Sleep(200 * time.Millisecond)
		}
	}
}

var errNotRunning = errors.New("内核没在运行")
