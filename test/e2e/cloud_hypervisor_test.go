//go:build e2e

package e2e

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	controlPort = "1024"
	sshPort     = "1025"
)

func TestCloudHypervisorAgentE2E(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cloud-hypervisor e2e requires linux")
	}

	repo := repoRoot(t)
	work := workDir(t)
	logDir := filepath.Join(work, "logs")
	must(t, os.MkdirAll(logDir, 0o755))
	t.Logf("e2e artifacts: %s", work)

	guestAgent := filepath.Join(work, "guest-agent")
	vsockConnect := filepath.Join(work, "vsock-connect")
	echoServer := filepath.Join(work, "echo-server")

	buildBinary(t, repo, "./cmd/guest-agent", guestAgent, "linux", runtime.GOARCH)
	buildBinary(t, repo, "./cmd/vsock-connect", vsockConnect, runtime.GOOS, runtime.GOARCH)
	buildBinary(t, repo, "./test/e2e/echo", echoServer, "linux", runtime.GOARCH)

	busybox := requireBusybox(t)
	requireTool(t, "cpio")
	requireTool(t, "cloud-hypervisor")
	kernel := findKernel(t)
	requireReadableWritableKVM(t)

	initramfs := filepath.Join(work, "initramfs.cpio.gz")
	buildInitramfs(t, work, initramfs, busybox, guestAgent, echoServer, kernel)

	vsockPath := filepath.Join(work, "agent.vsock")
	serialLog := filepath.Join(logDir, "serial.log")
	vmmLog := filepath.Join(logDir, "cloud-hypervisor.log")
	dumpLogsOnFailure(t, serialLog, vmmLog)
	vmCtx, cancelVM := context.WithCancel(context.Background())
	defer cancelVM()

	vmm := launchCloudHypervisor(t, vmCtx, kernel, initramfs, vsockPath, serialLog, vmmLog)
	defer func() {
		cancelVM()
		done := make(chan error, 1)
		go func() { done <- vmm.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = vmm.Process.Kill()
			<-done
		}
	}()

	waitForAgent(t, vsockConnect, vsockPath)

	t.Run("ping", func(t *testing.T) {
		runVsock(t, vsockConnect, nil, "ping", "--uds", vsockPath, "--port", controlPort)
	})

	t.Run("info", func(t *testing.T) {
		res := runVsock(t, vsockConnect, nil, "info", "--uds", vsockPath, "--port", controlPort)
		var info struct {
			Version string `json:"version"`
			Uname   string `json:"uname"`
			Uptime  string `json:"uptime"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(res.stdout), &info); err != nil {
			t.Fatalf("info json: %v\nstdout=%s\nstderr=%s", err, res.stdout, res.stderr)
		}
		if info.Version == "" || info.Uname == "" || info.Uptime == "" {
			t.Fatalf("incomplete info response: %+v", info)
		}
	})

	t.Run("exec stdout", func(t *testing.T) {
		res := runVsock(t, vsockConnect, nil, "exec", "--uds", vsockPath, "--port", controlPort, "--", "echo", "hello-e2e")
		if got, want := string(res.stdout), "hello-e2e\n"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	})

	t.Run("exec shell", func(t *testing.T) {
		res := runVsock(t, vsockConnect, nil, "exec", "--uds", vsockPath, "--port", controlPort, "--shell", "--", "echo shell-$((2+3))")
		if got, want := string(res.stdout), "shell-5\n"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	})

	t.Run("exec stdin", func(t *testing.T) {
		res := runVsock(t, vsockConnect, []byte("streamed-input"), "exec", "--uds", vsockPath, "--port", controlPort, "--stdin", "--", "cat")
		if got, want := string(res.stdout), "streamed-input"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	})

	t.Run("exec exit code", func(t *testing.T) {
		res := runVsockAllowExit(t, vsockConnect, nil, "exec", "--uds", vsockPath, "--port", controlPort, "--", "false")
		if res.code != 1 {
			t.Fatalf("exit code = %d, want 1\nstdout=%s\nstderr=%s", res.code, res.stdout, res.stderr)
		}
	})

	t.Run("exec timeout", func(t *testing.T) {
		start := time.Now()
		res := runVsockAllowExit(t, vsockConnect, nil, "exec", "--uds", vsockPath, "--port", controlPort, "--timeout", "1", "--", "sleep", "30")
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("timeout command took %s", elapsed)
		}
		if res.code != 143 {
			t.Fatalf("exit code = %d, want 143\nstdout=%s\nstderr=%s", res.code, res.stdout, res.stderr)
		}
	})

	t.Run("ssh proxy raw bytes", func(t *testing.T) {
		runRawProxy(t, vsockConnect, vsockPath, []byte("raw-proxy-check"))
	})
}

type commandResult struct {
	stdout []byte
	stderr []byte
	code   int
}

func workDir(t *testing.T) string {
	t.Helper()
	base := os.Getenv("E2E_ARTIFACT_DIR")
	if base == "" {
		return t.TempDir()
	}
	must(t, os.MkdirAll(base, 0o755))
	dir, err := os.MkdirTemp(base, "cloud-hypervisor-")
	if err != nil {
		t.Fatalf("create artifact dir: %v", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("resolve artifact dir: %v", err)
	}
	return abs
}

func repoRoot(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func buildBinary(t *testing.T, repo, pkg, out, goos, goarch string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-ldflags", "-s -w", "-o", out, pkg)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	runCommand(t, cmd, nil)
}

func requireTool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("missing required tool %q: %v", name, err)
	}
	return path
}

func requireBusybox(t *testing.T) string {
	t.Helper()
	if path, err := exec.LookPath("busybox"); err == nil {
		return path
	}
	for _, path := range []string{"/bin/busybox", "/usr/bin/busybox"} {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path
		}
	}
	t.Fatal("missing required tool \"busybox\"; install busybox-static")
	return ""
}

func findKernel(t *testing.T) string {
	t.Helper()
	candidates, err := filepath.Glob("/boot/vmlinuz-*")
	if err != nil {
		t.Fatalf("glob kernels: %v", err)
	}
	for i := len(candidates) - 1; i >= 0; i-- {
		if info, err := os.Stat(candidates[i]); err == nil && info.Mode().IsRegular() {
			return candidates[i]
		}
	}
	t.Fatal("no /boot/vmlinuz-* kernel found; install linux-image-kvm or another bootable kernel")
	return ""
}

func requireReadableWritableKVM(t *testing.T) {
	t.Helper()
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("/dev/kvm is not readable/writable by this user: %v", err)
	}
	_ = f.Close()
}

func buildInitramfs(t *testing.T, work, out, busybox, guestAgent, echoServer, kernel string) {
	t.Helper()
	root := filepath.Join(work, "rootfs")
	for _, dir := range []string{"bin", "dev", "proc", "sys", "tmp", "usr/local/bin"} {
		must(t, os.MkdirAll(filepath.Join(root, dir), 0o755))
	}
	must(t, os.Chmod(filepath.Join(root, "tmp"), 0o1777))
	copyFile(t, busybox, filepath.Join(root, "bin", "busybox"), 0o755)
	copyFile(t, guestAgent, filepath.Join(root, "usr/local/bin", "guest-agent"), 0o755)
	copyFile(t, echoServer, filepath.Join(root, "usr/local/bin", "echo-server"), 0o755)

	for _, applet := range []string{"sh", "cat", "echo", "sleep", "false", "uname", "id", "tee", "mount", "insmod"} {
		must(t, os.Symlink("busybox", filepath.Join(root, "bin", applet)))
	}

	// The guest agent binds a vsock listener, which requires /dev/vsock. On the
	// distro kernel (linux-image-*), the vsock transport is a loadable module, so
	// stage those modules into the initramfs and insmod them from init. Without
	// this /dev/vsock never appears and the agent exits before binding its port.
	vsockMods := stageVsockModules(t, kernel, root)

	init := `#!/bin/sh
set -eu
export PATH=/bin:/usr/bin:/usr/local/bin
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev || true
for m in ` + strings.Join(vsockMods, " ") + `; do
	if [ -f /lib/modules/$m.ko ]; then
		insmod /lib/modules/$m.ko || echo "init: insmod $m failed"
	fi
done
/usr/local/bin/echo-server >/tmp/echo-server.log 2>&1 &
/usr/local/bin/guest-agent --ssh-target 127.0.0.1:2222 --log-level debug >/tmp/guest-agent.log 2>&1 &
while true; do sleep 3600; done
`
	must(t, os.WriteFile(filepath.Join(root, "init"), []byte(init), 0o755))
	writeCPIOGzip(t, root, out)
}

// stageVsockModules copies the virtio-vsock kernel modules for the given kernel
// into <root>/lib/modules, decompressing them if the distro ships compressed
// modules (Ubuntu uses .ko.zst). It returns the module names, in load order, that
// were actually staged. If a module isn't found it's assumed built-in and skipped.
func stageVsockModules(t *testing.T, kernel, root string) []string {
	t.Helper()
	release := strings.TrimPrefix(filepath.Base(kernel), "vmlinuz-")
	modRoot := filepath.Join("/lib/modules", release, "kernel", "net", "vmw_vsock")
	dstDir := filepath.Join(root, "lib", "modules")
	must(t, os.MkdirAll(dstDir, 0o755))

	// Load order: core, then the common transport, then the virtio transport.
	order := []string{"vsock", "vmw_vsock_virtio_transport_common", "vmw_vsock_virtio_transport"}
	var staged []string
	for _, m := range order {
		matches, err := filepath.Glob(filepath.Join(modRoot, m+".ko*"))
		if err != nil {
			t.Fatalf("glob module %s: %v", m, err)
		}
		if len(matches) == 0 {
			t.Logf("vsock module %s not found under %s (assuming built-in)", m, modRoot)
			continue
		}
		decompressModule(t, matches[0], filepath.Join(dstDir, m+".ko"))
		staged = append(staged, m)
	}
	if len(staged) == 0 {
		t.Logf("no vsock modules staged for %s; assuming vsock is built into the kernel", release)
	}
	return staged
}

// decompressModule writes src to dst, decompressing by extension. Ubuntu ships
// kernel modules as .ko.zst; .ko.xz and .ko.gz are also handled, and a plain .ko
// is copied through.
func decompressModule(t *testing.T, src, dst string) {
	t.Helper()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("create %s: %v", dst, err)
	}
	defer func() {
		if cerr := out.Close(); cerr != nil {
			t.Fatalf("close %s: %v", dst, cerr)
		}
	}()

	switch {
	case strings.HasSuffix(src, ".zst"):
		decompressWith(t, out, "zstd", "-d", "-c", src)
	case strings.HasSuffix(src, ".xz"):
		decompressWith(t, out, "xz", "-d", "-c", src)
	case strings.HasSuffix(src, ".gz"):
		in, err := os.Open(src)
		if err != nil {
			t.Fatalf("open %s: %v", src, err)
		}
		defer in.Close()
		gz, err := gzip.NewReader(in)
		if err != nil {
			t.Fatalf("gunzip %s: %v", src, err)
		}
		if _, err := io.Copy(out, gz); err != nil {
			t.Fatalf("decompress %s: %v", src, err)
		}
	default:
		in, err := os.Open(src)
		if err != nil {
			t.Fatalf("open %s: %v", src, err)
		}
		defer in.Close()
		if _, err := io.Copy(out, in); err != nil {
			t.Fatalf("copy %s: %v", src, err)
		}
	}
}

// decompressWith runs a decompressor and streams its stdout into out.
func decompressWith(t *testing.T, out io.Writer, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, stderr.Bytes())
	}
}

func copyFile(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		t.Fatalf("create %s: %v", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatalf("copy %s to %s: %v", src, dst, err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close %s: %v", dst, err)
	}
}

func writeCPIOGzip(t *testing.T, root, out string) {
	t.Helper()
	var list bytes.Buffer
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		list.WriteString("./" + filepath.ToSlash(rel))
		list.WriteByte(0)
		return nil
	})
	if err != nil {
		t.Fatalf("walk initramfs root: %v", err)
	}

	cmd := exec.Command("cpio", "--quiet", "-o", "-H", "newc", "--null")
	cmd.Dir = root
	cmd.Stdin = &list
	cpioOut, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("cpio: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("cpio: %v", err)
	}

	f, err := os.Create(out)
	if err != nil {
		t.Fatalf("create %s: %v", out, err)
	}
	gz := gzip.NewWriter(f)
	if _, err := gz.Write(cpioOut); err != nil {
		_ = f.Close()
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		_ = f.Close()
		t.Fatalf("gzip close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", out, err)
	}
}

func launchCloudHypervisor(t *testing.T, ctx context.Context, kernel, initramfs, vsockPath, serialLog, vmmLog string) *exec.Cmd {
	t.Helper()
	vmm, err := os.Create(vmmLog)
	if err != nil {
		t.Fatalf("create vmm log: %v", err)
	}
	t.Cleanup(func() {
		_ = vmm.Close()
	})

	cmd := exec.CommandContext(ctx, "cloud-hypervisor",
		"--kernel", kernel,
		"--initramfs", initramfs,
		"--cmdline", "console=ttyS0 panic=1 reboot=k init=/init",
		"--cpus", "boot=1",
		"--memory", "size=512M",
		"--vsock", "cid=3,socket="+vsockPath,
		"--serial", "file="+serialLog,
		"--console", "off",
	)
	cmd.Stdout = vmm
	cmd.Stderr = vmm
	if err := cmd.Start(); err != nil {
		t.Fatalf("start cloud-hypervisor: %v", err)
	}
	return cmd
}

func waitForAgent(t *testing.T, vsockConnect, vsockPath string) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	var last commandResult
	for time.Now().Before(deadline) {
		last = runVsockAllowExitNoFatal(vsockConnect, nil, "ping", "--uds", vsockPath, "--port", controlPort)
		if last.code < 0 {
			t.Fatalf("could not execute vsock-connect readiness probe\nstdout=%s\nstderr=%s", last.stdout, last.stderr)
		}
		if last.code == 0 {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("agent did not become ready before deadline\nlast stdout=%s\nlast stderr=%s", last.stdout, last.stderr)
}

func runVsock(t *testing.T, bin string, stdin []byte, args ...string) commandResult {
	t.Helper()
	res := runVsockAllowExit(t, bin, stdin, args...)
	if res.code != 0 {
		t.Fatalf("%s %s failed with exit %d\nstdout=%s\nstderr=%s", bin, strings.Join(args, " "), res.code, res.stdout, res.stderr)
	}
	return res
}

func runVsockAllowExit(t *testing.T, bin string, stdin []byte, args ...string) commandResult {
	t.Helper()
	return runCommand(t, exec.Command(bin, args...), stdin)
}

func runVsockAllowExitNoFatal(bin string, stdin []byte, args ...string) commandResult {
	cmd := exec.Command(bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	code := 0
	if err := cmd.Run(); err != nil {
		code = exitCode(err)
		if code < 0 {
			stderr.WriteString(err.Error())
			stderr.WriteByte('\n')
		}
	}
	return commandResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), code: code}
}

func runRawProxy(t *testing.T, bin, vsockPath string, payload []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "--uds", vsockPath, "--port", sshPort)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start raw proxy: %v", err)
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	if _, err := stdin.Write(payload); err != nil {
		t.Fatalf("write raw proxy payload: %v\nstderr=%s", err, stderr.Bytes())
	}
	got := make([]byte, len(payload))
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(stdout, got)
		readDone <- err
	}()

	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("read raw proxy response: %v\nstderr=%s", err, stderr.Bytes())
		}
	case <-ctx.Done():
		t.Fatalf("raw proxy response timed out\nstderr=%s", stderr.Bytes())
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("proxy response = %q, want %q", got, payload)
	}
}

func runCommand(t *testing.T, cmd *exec.Cmd, stdin []byte) commandResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	code := 0
	if err := cmd.Run(); err != nil {
		code = exitCode(err)
		if code < 0 {
			t.Fatalf("%s failed: %v\nstdout=%s\nstderr=%s", strings.Join(cmd.Args, " "), err, stdout.Bytes(), stderr.Bytes())
		}
	}
	return commandResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), code: code}
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func dumpLogsOnFailure(t *testing.T, paths ...string) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, path := range paths {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Logf("%s unavailable: %v", path, err)
				continue
			}
			t.Logf("%s:\n%s", path, b)
		}
	})
}
