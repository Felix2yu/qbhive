package notifier

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Felix2yu/qbhive/internal/models"
)

// notifyRecorder 是本地假的 Apprise 接收端：
// apprise 的 json:// URL 最终会往对应的 http 端点 POST 一条 JSON 通知，
// 用它可以在完全离线的环境下覆盖「发送成功 / 发送失败」分支。
type notifyRecorder struct {
	mu     sync.Mutex
	titles []string
	bodies []string
	status int // 返回给 Apprise 的 HTTP 状态码（200=成功，500=失败）
}

// newNotifySink 起一个本地通知接收端，测试结束自动关闭
func newNotifySink(t *testing.T, status int) (*httptest.Server, *notifyRecorder) {
	t.Helper()
	rec := &notifyRecorder{status: status}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Title   string `json:"title"`
			Message string `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		rec.mu.Lock()
		rec.titles = append(rec.titles, payload.Title)
		rec.bodies = append(rec.bodies, payload.Message)
		rec.mu.Unlock()
		w.WriteHeader(rec.status)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, rec
}

// appriseURL 把本地 httptest 地址转成 apprise 的 json:// 通知 URL（走 http，不碰外网）
func appriseURL(srv *httptest.Server) string {
	return "json://" + strings.TrimPrefix(srv.URL, "http://") + "/notify"
}

func (r *notifyRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.titles)
}

func (r *notifyRecorder) last() (title, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.titles) == 0 {
		return "", ""
	}
	return r.titles[len(r.titles)-1], r.bodies[len(r.bodies)-1]
}

// TestEnabled_Semantics 表驱动验证 Enabled() 语义：开关打开 && 至少配置了一个 URL
func TestEnabled_Semantics(t *testing.T) {
	cases := []struct {
		name string
		cfg  models.NotifierConfig
		want bool
	}{
		{"禁用且无URL", models.NotifierConfig{}, false},
		{"禁用但有URL", models.NotifierConfig{Enabled: false, AppriseURLs: []string{"json://127.0.0.1:1/notify"}}, false},
		{"启用但无URL", models.NotifierConfig{Enabled: true}, false},
		{"启用且有URL", models.NotifierConfig{Enabled: true, AppriseURLs: []string{"json://127.0.0.1:1/notify"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := New(c.cfg).Enabled(); got != c.want {
				t.Errorf("Enabled() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestNotify_DisabledReturnsNil 禁用时直接返回 nil，不产生任何外发
func TestNotify_DisabledReturnsNil(t *testing.T) {
	n := New(models.NotifierConfig{
		Enabled:     false,
		AppriseURLs: []string{"json://127.0.0.1:1/notify"},
	})
	if err := n.Notify("标题", "正文"); err != nil {
		t.Errorf("禁用时 Notify 应返回 nil，got %v", err)
	}
}

// TestNotify_LocalSuccess 启用 + 本地接收端 → 发送成功，标题/正文原样送达
func TestNotify_LocalSuccess(t *testing.T) {
	srv, rec := newNotifySink(t, http.StatusOK)
	n := New(models.NotifierConfig{Enabled: true, AppriseURLs: []string{appriseURL(srv)}})
	if !n.Enabled() {
		t.Fatal("启用且有 URL 时 Enabled() 应为 true")
	}
	if err := n.Notify("下载完成 · Movie.mkv", "正文内容"); err != nil {
		t.Fatalf("本地发送应成功，got %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("接收端应收到 1 条，got %d", rec.count())
	}
	title, body := rec.last()
	if title != "下载完成 · Movie.mkv" || body != "正文内容" {
		t.Errorf("送达内容不符：title=%q body=%q", title, body)
	}
}

// TestNotify_SendFailure 启用但对端返回 500 → 发送失败，应返回 error
func TestNotify_SendFailure(t *testing.T) {
	srv, rec := newNotifySink(t, http.StatusInternalServerError)
	n := New(models.NotifierConfig{Enabled: true, AppriseURLs: []string{appriseURL(srv)}})
	if err := n.Notify("标题", "正文"); err == nil {
		t.Error("对端 500 时 Notify 应返回 error")
	}
	if rec.count() != 1 {
		t.Errorf("接收端仍应收到 1 次请求，got %d", rec.count())
	}
}

// TestNotify_InvalidURL 启用但 URL 非法：
// Reload 里 AddAll 失败只告警（覆盖该分支），Enabled() 仍为 true，
// 但 client 内没有任何目标，Send 会直接报错。
func TestNotify_InvalidURL(t *testing.T) {
	n := New(models.NotifierConfig{
		Enabled:     true,
		AppriseURLs: []string{"这不是一个合法的 URL"},
	})
	if !n.Enabled() {
		t.Error("Enabled() 只看配置，不看 URL 是否合法")
	}
	if err := n.Notify("标题", "正文"); err == nil {
		t.Error("URL 非法导致没有可用目标时 Notify 应返回 error")
	}
}

// TestReload_TogglesEnabled 覆盖 Reload：从启用切到禁用再切回启用
func TestReload_TogglesEnabled(t *testing.T) {
	srv, rec := newNotifySink(t, http.StatusOK)
	n := New(models.NotifierConfig{Enabled: true, AppriseURLs: []string{appriseURL(srv)}})

	// 禁用：Enabled=false 且 Notify 直接返回 nil（不外发）
	n.Reload(models.NotifierConfig{})
	if n.Enabled() {
		t.Error("Reload 到空配置后 Enabled() 应为 false")
	}
	if err := n.Notify("t", "b"); err != nil {
		t.Errorf("禁用时 Notify 应返回 nil，got %v", err)
	}
	if rec.count() != 0 {
		t.Errorf("禁用期间不应有外发，got %d 次", rec.count())
	}

	// 重新启用：换一条合法 URL，Notify 应真正发出去
	n.Reload(models.NotifierConfig{Enabled: true, AppriseURLs: []string{appriseURL(srv)}})
	if !n.Enabled() {
		t.Error("重新启用后 Enabled() 应为 true")
	}
	if err := n.Notify("重新启用", "ok"); err != nil {
		t.Errorf("重新启用后 Notify 应成功，got %v", err)
	}
	if rec.count() != 1 {
		t.Errorf("接收端应收到 1 条，got %d", rec.count())
	}
}

// TestReload_InvalidURLStaysEnabled URL 非法时 Reload 只告警不 panic，
// Enabled() 语义保持（仍然只看配置），Notify 走「无目标」错误分支。
func TestReload_InvalidURLStaysEnabled(t *testing.T) {
	n := New(models.NotifierConfig{})
	n.Reload(models.NotifierConfig{Enabled: true, AppriseURLs: []string{"bad-url-no-scheme"}})
	if !n.Enabled() {
		t.Error("URL 非法不应影响 Enabled() 的配置语义")
	}
	if err := n.Notify("t", "b"); err == nil {
		t.Error("无可用目标时 Notify 应返回 error")
	}
}

// TestDisabledPaths 表驱动：禁用 / 空 URL 列表的路径一律返回 nil 且不外发
func TestDisabledPaths(t *testing.T) {
	srv, rec := newNotifySink(t, http.StatusOK)
	cases := []struct {
		name string
		cfg  models.NotifierConfig
	}{
		{"完全空配置", models.NotifierConfig{}},
		{"启用但无URL", models.NotifierConfig{Enabled: true}},
		{"禁用但有URL", models.NotifierConfig{Enabled: false, AppriseURLs: []string{appriseURL(srv)}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := New(c.cfg)
			if n.Enabled() {
				t.Error("Enabled() 应为 false")
			}
			if err := n.Notify("t", "b"); err != nil {
				t.Errorf("禁用路径 Notify 应返回 nil，got %v", err)
			}
			if err := n.Test(c.cfg); err != nil {
				t.Errorf("禁用路径 Test 应返回 nil，got %v", err)
			}
		})
	}
	if rec.count() != 0 {
		t.Errorf("禁用路径不应产生外发，got %d 次", rec.count())
	}
}

// TestTest_LocalSuccess 测试通知发到本地接收端
func TestTest_LocalSuccess(t *testing.T) {
	srv, rec := newNotifySink(t, http.StatusOK)
	cfg := models.NotifierConfig{Enabled: true, AppriseURLs: []string{appriseURL(srv)}}
	if err := New(models.NotifierConfig{}).Test(cfg); err != nil {
		t.Fatalf("本地发送测试通知应成功，got %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("接收端应收到 1 条，got %d", rec.count())
	}
	title, _ := rec.last()
	if title != "QBHive 通知测试" {
		t.Errorf("测试通知标题不符，got %q", title)
	}
}

// TestTest_SendFailure 对端返回 500 → Test 返回 error
func TestTest_SendFailure(t *testing.T) {
	srv, _ := newNotifySink(t, http.StatusInternalServerError)
	cfg := models.NotifierConfig{Enabled: true, AppriseURLs: []string{appriseURL(srv)}}
	if err := New(models.NotifierConfig{}).Test(cfg); err == nil {
		t.Error("对端 500 时 Test 应返回 error")
	}
}

// TestTest_InvalidURL URL 非法 → AddAll 失败，立即返回 error（不外发）
func TestTest_InvalidURL(t *testing.T) {
	cfg := models.NotifierConfig{Enabled: true, AppriseURLs: []string{"没有 scheme 的地址"}}
	if err := New(models.NotifierConfig{}).Test(cfg); err == nil {
		t.Error("URL 非法时 Test 应返回 error")
	}
}
