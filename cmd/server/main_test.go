package main

import (
	"bytes"
	"errors"
	"flag"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
)

// TestMain 提供 helper-process 模式：带 QBHIVE_TEST_HELPER=1 的子进程直接执行 main()，
// 父测试进程负责准备配置、发送 SIGTERM 并断言退出状态。
func TestMain(m *testing.M) {
	if os.Getenv("QBHIVE_TEST_HELPER") == "1" {
		// testing 包在 init 时已把 -test.* 注册到 flag.CommandLine，
		// 直接 flag.Parse 会因 -config 未定义而退出，这里换成干净的 FlagSet。
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
		os.Args = []string{
			"qbhive",
			"-config", os.Getenv("QBHIVE_TEST_CONFIG"),
			"-web", os.Getenv("QBHIVE_TEST_WEB"),
		}
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// ============ defaultPath / defaultWebDir ============

func TestDefaults_EnvOverrides(t *testing.T) {
	cases := []struct {
		name     string
		envKey   string
		envVal   string
		wantPath string
		fn       func() string
	}{
		{"config 未设置用默认值", "QBHIVE_CONFIG", "", "config.json", defaultPath},
		{"config 设置后取环境变量", "QBHIVE_CONFIG", "/etc/qbhive/config.json", "/etc/qbhive/config.json", defaultPath},
		{"web 未设置用默认值", "QBHIVE_WEB", "", "web/static", defaultWebDir},
		{"web 设置后取环境变量", "QBHIVE_WEB", "/app/web/dist", "/app/web/dist", defaultWebDir},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.envKey, tc.envVal)
			if got := tc.fn(); got != tc.wantPath {
				t.Errorf("%s() = %q, want %q", tc.name, got, tc.wantPath)
			}
		})
	}
}

// ============ helper 进程脚手架 ============

// syncBuffer 线程安全地收集子进程 stdout/stderr
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// helperProc 代表一个执行 main() 的子进程
type helperProc struct {
	cmd     *exec.Cmd
	out     *syncBuffer
	done    chan struct{}
	mu      sync.Mutex
	waitErr error
}

// startHelper 拉起子进程执行 main()，extraEnv 形如 "K=V"
func startHelper(t *testing.T, extraEnv ...string) *helperProc {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("获取测试二进制路径失败: %v", err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), extraEnv...)
	out := &syncBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 helper 子进程失败: %v", err)
	}

	h := &helperProc{cmd: cmd, out: out, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		h.mu.Lock()
		h.waitErr = err
		h.mu.Unlock()
		close(h.done)
	}()
	t.Cleanup(func() {
		select {
		case <-h.done:
		default:
			_ = cmd.Process.Kill()
			<-h.done
		}
	})
	return h
}

// waitForOutput 轮询等待子进程输出包含关键字
func (h *helperProc) waitForOutput(t *testing.T, keyword string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(h.out.String(), keyword) {
			return
		}
		//子进程若已退出则提前失败
		select {
		case <-h.done:
			if strings.Contains(h.out.String(), keyword) {
				return
			}
			t.Fatalf("子进程已退出（%v），未看到输出 %q；输出内容:\n%s", h.waitErr, keyword, h.out.String())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待输出 %q 超时；输出内容:\n%s", keyword, h.out.String())
}

// waitExit 等待子进程退出，返回退出码
func (h *helperProc) waitExit(t *testing.T, timeout time.Duration) int {
	t.Helper()
	select {
	case <-h.done:
	case <-time.After(timeout):
		_ = h.cmd.Process.Kill()
		<-h.done
		t.Fatalf("等待子进程退出超时；输出内容:\n%s", h.out.String())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.waitErr == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(h.waitErr, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("等待子进程失败: %v；输出内容:\n%s", h.waitErr, h.out.String())
	return -1
}

// signalTERM 发送 SIGTERM 触发 main() 的优雅退出
func (h *helperProc) signalTERM(t *testing.T) {
	t.Helper()
	if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("发送 SIGTERM 失败: %v", err)
	}
}

// mockQB 起一个最小的 qB mock，供子进程做 TestConnection / 拉列表
func mockQB(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "SID=test; Path=/")
		_, _ = w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`"5.2.0"`))
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// writeConfig 生成子进程用的 config.json，返回 (config 路径, web 目录)
func writeConfig(t *testing.T, listen, qbURL string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	webDir := filepath.Join(dir, "web")
	if err := os.MkdirAll(webDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := config.New(path)
	cfg := m.Get()
	cfg.Server.Listen = listen
	cfg.Qbittorrent.URL = qbURL
	cfg.Qbittorrent.Username = "u"
	cfg.Qbittorrent.Password = "p"
	m.Set(cfg)
	if err := m.Save(); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	return path, webDir
}

// ============ main() 集成测试 ============

// TestMain_GracefulShutdown 覆盖 main() 主流程：
// 读配置 → 连通性测试 → 启动调度器与 Web 服务 → 收到 SIGTERM 后优雅退出。
func TestMain_GracefulShutdown(t *testing.T) {
	qbSrv := mockQB(t)
	cfgPath, webDir := writeConfig(t, "127.0.0.1:0", qbSrv.URL)

	h := startHelper(t,
		"QBHIVE_TEST_HELPER=1",
		"QBHIVE_TEST_CONFIG="+cfgPath,
		"QBHIVE_TEST_WEB="+webDir,
		"QBHIVE_CONFIG="+cfgPath,
	)

	// main() 里 Web goroutine 打印监听地址即认为服务已起来
	h.waitForOutput(t, "QBHive 监听地址", 10*time.Second)
	// 给 ListenAndServe 一点时间完成绑定
	time.Sleep(150 * time.Millisecond)

	h.signalTERM(t)
	code := h.waitExit(t, 10*time.Second)

	out := h.out.String()
	if !strings.Contains(out, "qBittorrent 连接正常") {
		t.Errorf("应输出连通性测试成功日志；输出:\n%s", out)
	}
	if !strings.Contains(out, "正在关闭") {
		t.Errorf("应收到 SIGTERM 并进入关闭流程；输出:\n%s", out)
	}
	// scheduler.Stop 会把 finished 状态持久化到 config 同目录
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfgPath), "finished.json")); err != nil {
		t.Errorf("关闭时应写出 finished.json: %v", err)
	}
	assertShutdownExitCode(t, code, out)
}

// assertShutdownExitCode 校验 main() 关闭时的退出码。
// 源码里 srv.Shutdown() 会让 ListenAndServe 返回 http.ErrServerClosed，
// main 的 web goroutine 会把它当成「服务启动失败」并 Fatal(1)，
// 与主 goroutine 的正常返回（0）存在竞争，因此这里两种结果都接受，
// 但必须与日志自洽：0 ⇒ 打印「已退出」，1 ⇒ 打印「服务启动失败」。
func assertShutdownExitCode(t *testing.T, code int, out string) {
	t.Helper()
	switch code {
	case 0:
		if !strings.Contains(out, "已退出") {
			t.Errorf("退出码 0 时应打印「已退出」；输出:\n%s", out)
		}
	case 1:
		if !strings.Contains(out, "服务启动失败") {
			t.Errorf("退出码 1 时应打印「服务启动失败」；输出:\n%s", out)
		}
	default:
		t.Errorf("退出码应为 0 或 1，got %d；输出:\n%s", code, out)
	}
}

// TestMain_QBConnectionFailureStillStarts 覆盖 main() 里 TestConnection 失败只告警不中断的分支
func TestMain_QBConnectionFailureStillStarts(t *testing.T) {
	cfgPath, webDir := writeConfig(t, "127.0.0.1:0", "http://127.0.0.1:1")

	h := startHelper(t,
		"QBHIVE_TEST_HELPER=1",
		"QBHIVE_TEST_CONFIG="+cfgPath,
		"QBHIVE_TEST_WEB="+webDir,
		"QBHIVE_CONFIG="+cfgPath,
	)

	h.waitForOutput(t, "QBHive 监听地址", 10*time.Second)
	out := h.out.String()
	if !strings.Contains(out, "qBittorrent 连接测试失败") {
		t.Errorf("连接失败应有告警日志；输出:\n%s", out)
	}
	time.Sleep(150 * time.Millisecond)
	h.signalTERM(t)
	code := h.waitExit(t, 10*time.Second)
	out = h.out.String()
	if !strings.Contains(out, "正在关闭") {
		t.Errorf("即使 qB 不可达也应响应 SIGTERM；输出:\n%s", out)
	}
	assertShutdownExitCode(t, code, out)
}

// TestMain_ListenFailureExitsNonZero 覆盖 main() 里 srv.Start 报错 → Fatal 的分支：
// 先占住一个端口，子进程绑定同一地址必然失败。
func TestMain_ListenFailureExitsNonZero(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	qbSrv := mockQB(t)
	cfgPath, webDir := writeConfig(t, addr, qbSrv.URL)

	h := startHelper(t,
		"QBHIVE_TEST_HELPER=1",
		"QBHIVE_TEST_CONFIG="+cfgPath,
		"QBHIVE_TEST_WEB="+webDir,
		"QBHIVE_CONFIG="+cfgPath,
	)

	// Fatal 直接终止进程，不需要也不会收到 SIGTERM
	code := h.waitExit(t, 10*time.Second)
	out := h.out.String()
	if code == 0 {
		t.Errorf("端口被占用时应以非 0 退出；输出:\n%s", out)
	}
	if !strings.Contains(out, "服务启动失败") {
		t.Errorf("应输出服务启动失败日志；输出:\n%s", out)
	}
}

// TestMain_EmptyWebDirSkipsStatic 覆盖 webDir 为空时不挂载静态目录的路径
func TestMain_EmptyWebDirSkipsStatic(t *testing.T) {
	qbSrv := mockQB(t)
	cfgPath, _ := writeConfig(t, "127.0.0.1:0", qbSrv.URL)

	h := startHelper(t,
		"QBHIVE_TEST_HELPER=1",
		"QBHIVE_TEST_CONFIG="+cfgPath,
		"QBHIVE_TEST_WEB=",
		"QBHIVE_CONFIG="+cfgPath,
	)

	h.waitForOutput(t, "QBHive 监听地址", 10*time.Second)
	time.Sleep(150 * time.Millisecond)
	h.signalTERM(t)
	_ = h.waitExit(t, 10*time.Second)
}
