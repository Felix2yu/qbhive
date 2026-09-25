package qbittorrent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ============ 基础单元测试（纯逻辑） ============

func TestClient_New_TrimsTrailingSlash(t *testing.T) {
	c := New("http://qb.example.com:8080/", "u", "p", "")
	if c.baseURL != "http://qb.example.com:8080" {
		t.Errorf("trailing slash not trimmed: got %q", c.baseURL)
	}
}

func TestClient_New_TrimsAPIKeyWhitespace(t *testing.T) {
	c := New("http://qb.example.com", "", "", "  qbt_abc123  ")
	if c.apiKey != "qbt_abc123" {
		t.Errorf("apiKey whitespace not trimmed: got %q", c.apiKey)
	}
	if !c.usesAPIKey() {
		t.Error("expected usesAPIKey=true")
	}
}

func TestClient_New_NoAPIKey(t *testing.T) {
	c := New("http://qb.example.com", "u", "p", "")
	if c.usesAPIKey() {
		t.Error("expected usesAPIKey=false when empty")
	}
}

func TestClient_SetCredentials_HotSwapClearsCookie(t *testing.T) {
	c := New("http://old.host", "olduser", "oldpass", "")
	c.cookie = "fake-session=cached"
	c.SetCredentials("https://new.host/", "newuser", "newpass", "qbt_newkey")

	if c.baseURL != "https://new.host" {
		t.Errorf("baseURL not swapped: got %q", c.baseURL)
	}
	if c.user != "newuser" || c.pass != "newpass" {
		t.Error("user/pass not swapped")
	}
	if c.apiKey != "qbt_newkey" {
		t.Errorf("apiKey not swapped: got %q", c.apiKey)
	}
	if c.cookie != "" {
		t.Errorf("cookie should be cleared on credential swap, got %q", c.cookie)
	}
}

func TestClient_SetCredentials_TrimsAPIKey(t *testing.T) {
	c := New("http://x", "", "", "")
	c.SetCredentials("http://x", "", "", "  kkk  ")
	if c.apiKey != "kkk" {
		t.Errorf("SetCredentials should trim apiKey: got %q", c.apiKey)
	}
}

func TestClient_mustParse_ValidURL(t *testing.T) {
	u := mustParse("http://qb.example.com:8080/path")
	if u == nil {
		t.Fatal("mustParse should not return nil on valid URL")
	}
	if u.Host != "qb.example.com:8080" {
		t.Errorf("host mismatch: got %q", u.Host)
	}
}

func TestClient_mustParse_InvalidURL(t *testing.T) {
	u := mustParse("not a url %%%%")
	if u != nil {
		t.Errorf("expected nil on invalid URL, got %v", u)
	}
}

// ============ httptest server 辅助 + API 方法测试 ============

// newMockQB: 返回一个 mock qB server（支持 login + 按需应答 path）和一个 Client
func newMockQB(t *testing.T, router func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *Client) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "SID=testsid123; Path=/")
		w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Ok."))
	})
	if router != nil {
		mux.HandleFunc("/", router)
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	}
	srv := httptest.NewServer(mux)
	cli := New(srv.URL, "user", "pass", "")
	return srv, cli
}

func TestClient_Login_SetsCookie(t *testing.T) {
	srv, cli := newMockQB(t, nil)
	defer srv.Close()
	cli.cookie = ""
	if err := cli.login(); err != nil {
		t.Fatalf("login failed: %v", err)
	}
	if !strings.Contains(cli.cookie, "SID=") {
		t.Errorf("cookie not set after login: %q", cli.cookie)
	}
}

func TestClient_Login_ErrorWhenQBUnreachable(t *testing.T) {
	srv, cli := newMockQB(t, nil)
	srv.Close()
	cli.cookie = ""
	if err := cli.login(); err == nil {
		t.Error("expected login error after server close")
	}
}

func TestClient_TestConnection_Success(t *testing.T) {
	srv, cli := newMockQB(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`"4.6.0"`))
	})
	defer srv.Close()
	if err := cli.TestConnection(); err != nil {
		t.Fatalf("TestConnection failed: %v", err)
	}
}

func TestClient_GetTorrents_AndParse(t *testing.T) {
	srv, cli := newMockQB(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/torrents/info" {
			w.Header().Set("Content-Type", "application/x-bittorrent")
			w.Write([]byte(`[{"hash":"aaa","name":"Movie.mkv","state":"downloading","progress":0.25,"size":1073741824,"num_leechs":1,"num_seeds":5}]`))
			return
		}
		w.WriteHeader(404)
	})
	defer srv.Close()
	list, err := cli.GetTorrents()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "Movie.mkv" {
		t.Errorf("unexpected result: %v", list)
	}
}

func TestClient_GetTransferInfo(t *testing.T) {
	srv, cli := newMockQB(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/transfer/info" {
			w.Write([]byte(`{"up_speed_limit":524288}`))
			return
		}
		w.WriteHeader(404)
	})
	defer srv.Close()
	info, err := cli.GetTransferInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.UpSpeedLimit != 524288 {
		t.Errorf("transfer up_speed_limit mismatch: %+v", info)
	}
}

func TestClient_SetUploadLimit_PauseResume(t *testing.T) {
	var gotPath string
	srv, cli := newMockQB(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(200)
	})
	defer srv.Close()

	if err := cli.SetUploadLimit("hash1", 102400); err != nil {
		t.Errorf("SetUploadLimit: %v", err)
	}
	if gotPath != "/api/v2/torrents/setUploadLimit" {
		t.Errorf("wrong path: %s", gotPath)
	}

	if err := cli.PauseTorrents("h1", "h2"); err != nil {
		t.Errorf("PauseTorrents: %v", err)
	}
	if gotPath != "/api/v2/torrents/pause" {
		t.Errorf("wrong pause path: %s", gotPath)
	}

	if err := cli.ResumeTorrents("h1"); err != nil {
		t.Errorf("ResumeTorrents: %v", err)
	}
	if gotPath != "/api/v2/torrents/resume" {
		t.Errorf("wrong resume path: %s", gotPath)
	}
}

func TestClient_PauseTorrents_ErrorStatus(t *testing.T) {
	srv, cli := newMockQB(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/torrents/pause" {
			w.WriteHeader(500)
		}
	})
	defer srv.Close()
	if err := cli.PauseTorrents("h1"); err == nil {
		t.Error("expected error on 500 response")
	}
}

func TestClient_RenameFile_Success(t *testing.T) {
	var gotHash, gotOld, gotNew string
	srv, cli := newMockQB(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotHash = r.FormValue("hash")
		gotOld = r.FormValue("oldPath")
		gotNew = r.FormValue("newPath")
		w.WriteHeader(200)
	})
	defer srv.Close()

	err := cli.RenameFile("abc123", "SubDir/Video.mkv", "Video.mkv")
	if err != nil {
		t.Fatalf("RenameFile: %v", err)
	}
	if gotHash != "abc123" || gotOld != "SubDir/Video.mkv" || gotNew != "Video.mkv" {
		t.Errorf("form fields wrong: h=%q o=%q n=%q", gotHash, gotOld, gotNew)
	}
}

func TestClient_RenameFile_ErrorResponse(t *testing.T) {
	srv, cli := newMockQB(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/torrents/renameFile" {
			w.WriteHeader(404)
			w.Write([]byte("not found"))
		}
	})
	defer srv.Close()
	if err := cli.RenameFile("x", "old", "new"); err == nil {
		t.Error("expected error on renameFile 404")
	}
}

func TestClient_AddTorrent_Success(t *testing.T) {
	srv, cli := newMockQB(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/torrents/add" {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(404)
	})
	defer srv.Close()
	// uploadLimitKB=0 阻止后台 goroutine 里的 applyLatestUploadLimit
	if err := cli.AddTorrent([]byte("fake-torrent-bytes"), "/save", "", "", 0); err != nil {
		t.Fatalf("AddTorrent failed: %v", err)
	}
}

func TestClient_GetTorrentFiles_SuccessAndParseError(t *testing.T) {
	// 先测成功
	srv, cli := newMockQB(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"name":"Video.mkv","size":1073741824,"progress":1.0,"priority":1}]`))
	})
	files, err := cli.GetTorrentFiles("h1")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != "Video.mkv" {
		t.Errorf("wrong files: %+v", files)
	}
	srv.Close()

	// 再测 JSON 解析错误
	srv2, cli2 := newMockQB(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("not json"))
	})
	defer srv2.Close()
	if _, err := cli2.GetTorrentFiles("h1"); err == nil {
		t.Error("expected parse error on bad JSON")
	}
}

// ============ 认证失败字符串嗅探自动重登（用户报的 P0 级 bug） ============

// 模拟 qB 先返回 "Fails."（200 OK + 认证失败字符串），然后重登成功正常返回 JSON 数组
func TestClient_AuthFailureString_AutoRelogin(t *testing.T) {
	loginCount := 0
	infoCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		loginCount++
		w.Header().Set("Set-Cookie", "SID=test")
		w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		infoCount++
		// 第一次返回 qBittorrent 典型的认证失败字符串（200 OK + 纯 JSON string）
		if infoCount == 1 {
			w.WriteHeader(200)
			w.Write([]byte(`"Fails."`))
			return
		}
		w.Header().Set("Content-Type", "application/x-bittorrent")
		w.Write([]byte(`[{"hash":"h1","name":"OK.mkv","state":"downloading","progress":1}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cli := New(srv.URL, "u", "p", "")
	list, err := cli.GetTorrents()
	if err != nil {
		t.Fatalf("GetTorrents should auto re-login: %v", err)
	}
	if len(list) != 1 || list[0].Name != "OK.mkv" {
		t.Errorf("expected auto-relogin to return real data: %v", list)
	}
	if loginCount != 2 {
		t.Errorf("expected 2 logins (initial + auto relogin), got %d", loginCount)
	}
	if infoCount != 2 {
		t.Errorf("expected 2 info calls (fails + success), got %d", infoCount)
	}
}

// 非认证失败的字符串（比如 qB 500 报错）不应触发重登循环
func TestClient_NonAuthErrorString_NotAutoRelogin(t *testing.T) {
	loginCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		loginCount++
		w.Header().Set("Set-Cookie", "SID=test")
		w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		// 非认证失败的 JSON string 值（包含 "error" 但不含 auth 关键词）
		w.WriteHeader(200)
		w.Write([]byte(`"internal error"`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cli := New(srv.URL, "u", "p", "")
	_, err := cli.GetTorrents()
	if err == nil {
		t.Error("expected error response")
	}
	// 只应调用 login 一次（初始的），"internal error" 不该触发重登
	if loginCount != 1 {
		t.Errorf("expected only 1 login (no auto relogin), got %d", loginCount)
	}
}

// 认证接口自身（login）不应触发嗅探重登死循环
func TestClient_LoginAPI_NoAuthSniff(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		// 故意返回 "Fails." 但 login 是白名单接口，不会触发重登
		w.Write([]byte(`"Fails."`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cli := New(srv.URL, "u", "p", "")
	if err := cli.login(); err == nil {
		t.Error("expected login failure")
	}
}
