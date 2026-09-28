package qbittorrent

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ============ 测试脚手架 ============

// stubErrBody 是一个永远读失败的 Body，用于覆盖 io.ReadAll 出错分支。
// _looksAuthFailure 在 n==0 时直接返回 false，不会重建 body，因此后续 ReadAll 仍会失败。
type stubErrBody struct{}

func (stubErrBody) Read([]byte) (int, error) { return 0, errors.New("stub body read error") }
func (stubErrBody) Close() error             { return nil }

// stubErrReader 是一次性的失败 Reader，用于覆盖 _doRaw 里缓冲 body 时的读取错误
type stubErrReader struct{}

func (stubErrReader) Read([]byte) (int, error) { return 0, errors.New("stub reader error") }

// stubRT 是可编程的 RoundTripper：按 (path, 该 path 的第几次请求) 决定返回响应还是错误。
// 用于构造 httptest 难以表达的场景，例如「重登之后的第二次请求直接断连」。
type stubRT struct {
	mu  sync.Mutex
	seq map[string]int
	fn  func(path string, n int, req *http.Request) (*http.Response, error)
}

func (s *stubRT) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.seq[req.URL.Path]++
	n := s.seq[req.URL.Path]
	s.mu.Unlock()
	return s.fn(req.URL.Path, n, req)
}

// stubResp 构造一个 RoundTripper 响应
func stubResp(req *http.Request, code int, body string, cookies ...string) *http.Response {
	h := make(http.Header)
	for _, c := range cookies {
		h.Add("Set-Cookie", c)
	}
	return &http.Response{
		StatusCode: code,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

// stubLoginOK 是 stub 模式下的登录成功响应
func stubLoginOK(req *http.Request) *http.Response {
	return stubResp(req, http.StatusOK, "Ok.", "SID=stubsid; Path=/")
}

// useStub 把 client 的 transport 换成 stub
func useStub(c *Client, fn func(path string, n int, req *http.Request) (*http.Response, error)) {
	rt := &stubRT{seq: make(map[string]int), fn: fn}
	c.httpCli = &http.Client{Jar: c.httpCli.Jar, Transport: rt}
}

// newQBMock 起一个可定制的 qB mock：login 依调用序号返回 loginBodies 里的 body（超出取最后一个），
// 其余路径交给 route。返回计数函数便于断言重登次数。
func newQBMock(t *testing.T, loginBodies []string, route http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	var mu sync.Mutex
	loginCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		var b string
		if len(loginBodies) == 0 {
			b = "Ok."
		} else if loginCount < len(loginBodies) {
			b = loginBodies[loginCount]
		} else {
			b = loginBodies[len(loginBodies)-1]
		}
		loginCount++
		mu.Unlock()
		if b == "Ok." {
			w.Header().Set("Set-Cookie", "SID=mocksid; Path=/")
		}
		_, _ = w.Write([]byte(b))
	})
	if route != nil {
		mux.HandleFunc("/", route)
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, New(srv.URL, "u", "p", "")
}

// ============ GetCookie / login 剩余分支 ============

func TestClient_GetCookie_ReflectsLoginState(t *testing.T) {
	_, cli := newQBMock(t, nil, nil)
	if got := cli.GetCookie(); got != "" {
		t.Errorf("未登录时 GetCookie 应为空，got %q", got)
	}
	if err := cli.login(); err != nil {
		t.Fatalf("login: %v", err)
	}
	if got := cli.GetCookie(); got != cli.cookie || !strings.HasPrefix(got, "SID=") {
		t.Errorf("GetCookie 应返回登录 cookie，got %q", got)
	}
}

// login 返回 Ok. 但 Set-Cookie 里没有 SID → 回落到 cookie jar 拼接（jar 非空分支）
func TestClient_Login_FallsBackToJarWhenNoSIDCookie(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "token=abc123; Path=/")
		_, _ = w.Write([]byte("Ok."))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cli := New(srv.URL, "u", "p", "")
	if err := cli.login(); err != nil {
		t.Fatalf("login: %v", err)
	}
	if cli.cookie != "token=abc123" {
		t.Errorf("应回落到 jar 里的 cookie，got %q", cli.cookie)
	}
}

// login 返回 Ok. 且完全不下发 cookie → jar 为空，cookie 保持空串（len(cookies)>0 的 false 分支）
func TestClient_Login_NoCookieLeavesEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Ok."))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cli := New(srv.URL, "u", "p", "")
	if err := cli.login(); err != nil {
		t.Fatalf("login: %v", err)
	}
	if cli.GetCookie() != "" {
		t.Errorf("无 cookie 下发时应保持空串，got %q", cli.GetCookie())
	}
}

// ============ do / _doRaw 的失败分支 ============

// 传输层报错 → do 直接返回错误（同时覆盖 _doRaw 里 Do 失败清 cookie 的分支）
func TestClient_do_TransportError(t *testing.T) {
	srv, cli := newQBMock(t, nil, nil)
	if err := cli.login(); err != nil {
		t.Fatal(err)
	}
	srv.Close() // 模拟 qB 突然断连

	if _, err := cli.do("GET", "/api/v2/app/version", nil, ""); err == nil {
		t.Fatal("服务器不可达时 do 应返回错误")
	}
	if cli.GetCookie() != "" {
		t.Errorf("Do 失败后 cookie 应被清空，got %q", cli.GetCookie())
	}
}

// 业务接口返回认证失败字符串，但重登失败 → auto re-login failed
func TestClient_do_AutoReloginFails(t *testing.T) {
	// 第一次登录成功，第二次登录（自动重登）返回失败
	_, cli := newQBMock(t, []string{"Ok.", "Fails."}, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`"Fails."`))
	})

	_, err := cli.do("GET", "/api/v2/torrents/info", nil, "")
	if err == nil {
		t.Fatal("自动重登失败时应返回错误")
	}
	if !strings.Contains(err.Error(), "auto re-login failed") {
		t.Errorf("错误信息应为 auto re-login failed，got %v", err)
	}
}

// _doRaw 缓冲请求体时读取失败
func TestClient_doRaw_BodyReadError(t *testing.T) {
	_, cli := newQBMock(t, nil, nil)
	if _, err := cli._doRaw("POST", "/api/v2/torrents/add", stubErrReader{}, ""); err == nil {
		t.Fatal("请求体读取失败应返回错误")
	}
}

// 非法 method 让 http.NewRequest 报错：API key 模式与 cookie 模式各一条路径
func TestClient_doRaw_InvalidMethod(t *testing.T) {
	t.Run("API key 模式", func(t *testing.T) {
		cli := New("http://qb.example.com", "", "", "qbt_key")
		if _, err := cli._doRaw("bad method", "/api/v2/app/version", nil, ""); err == nil {
			t.Fatal("非法 method 应返回 NewRequest 错误")
		}
	})

	t.Run("cookie 模式", func(t *testing.T) {
		_, cli := newQBMock(t, nil, nil)
		if err := cli.login(); err != nil {
			t.Fatal(err)
		}
		if _, err := cli._doRaw("bad method", "/api/v2/app/version", nil, ""); err == nil {
			t.Fatal("非法 method 应返回 NewRequest 错误")
		}
	})
}

// cookie 为空且登录失败 → _doRaw 直接返回登录错误
func TestClient_doRaw_LoginFails(t *testing.T) {
	_, cli := newQBMock(t, []string{"Fails."}, nil)
	if _, err := cli._doRaw("GET", "/api/v2/app/version", nil, ""); err == nil {
		t.Fatal("登录失败时 _doRaw 应返回错误")
	}
	if !strings.Contains(errString(cli._doRaw("GET", "/api/v2/app/version", nil, "")), "login failed") {
		t.Error("错误信息应来自 login failed")
	}
}

// errString 只是为了让断言更紧凑
func errString(_ *http.Response, err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// 401 → 重登 → 重试成功（覆盖 _doRaw 的 401/403 重登重试路径）
func TestClient_doRaw_UnauthorizedThenRetryOK(t *testing.T) {
	var mu sync.Mutex
	bizCount := 0
	_, cli := newQBMock(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/torrents/info" {
			mu.Lock()
			bizCount++
			n := bizCount
			mu.Unlock()
			if n == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"hash":"h1","name":"ok.mkv"}]`))
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	list, err := cli.GetTorrents()
	if err != nil {
		t.Fatalf("401 后应重登并重试成功: %v", err)
	}
	if len(list) != 1 || list[0].Name != "ok.mkv" {
		t.Errorf("重试结果错误: %+v", list)
	}
}

// 401 → 重登成功 → 重试的那次请求直接断连
func TestClient_doRaw_UnauthorizedRetryDoError(t *testing.T) {
	_, cli := newQBMock(t, nil, nil)
	useStub(cli, func(path string, n int, req *http.Request) (*http.Response, error) {
		switch {
		case path == "/api/v2/auth/login":
			return stubLoginOK(req), nil
		case n == 1:
			return stubResp(req, http.StatusUnauthorized, ""), nil
		default:
			return nil, errors.New("connection reset by peer")
		}
	})

	if _, err := cli.do("GET", "/api/v2/app/version", nil, ""); err == nil {
		t.Fatal("重试请求断连时应返回错误")
	}
}

// ============ TestConnection 的错误分支 ============

func TestClient_TestConnection_Errors(t *testing.T) {
	t.Run("请求失败", func(t *testing.T) {
		srv, cli := newQBMock(t, nil, nil)
		if err := cli.login(); err != nil {
			t.Fatal(err)
		}
		srv.Close()
		if err := cli.TestConnection(); err == nil {
			t.Fatal("服务器不可达应返回错误")
		}
	})

	t.Run("非 200 状态", func(t *testing.T) {
		_, cli := newQBMock(t, nil, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		})
		err := cli.TestConnection()
		if err == nil || !strings.Contains(err.Error(), "unexpected status") {
			t.Fatalf("应返回 unexpected status 错误，got %v", err)
		}
	})
}

// ============ isQBErrorString ============

func TestClient_isQBErrorString(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{`"Fails."`, true},
		{`"Unauthorized"`, true},
		{`  "Invalid sort field"  `, true},
		{`""`, false},     // 引号内为空 → 不是 qB 错误串
		{`not-json`, false},
		{`[]`, false},
		{``, false},
	}
	for _, tc := range cases {
		if got := isQBErrorString([]byte(tc.in)); got != tc.want {
			t.Errorf("isQBErrorString(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// ============ GetTorrents 参数与错误分支 ============

func TestClient_GetTorrents_QueryParams(t *testing.T) {
	var gotQuery string
	_, cli := newQBMock(t, nil, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[{"hash":"h1","name":"n"}]`))
	})

	cases := []struct {
		name   string
		params []string
		want   string
	}{
		{"无参数", nil, ""},
		{"全部参数", []string{"active", "name", "1"}, "filter=active&reverse=1&sort=name"},
		{"filter=all 不拼 query", []string{"all", "", ""}, ""},
		{"空白参数不拼 query", []string{"  ", "  ", "  "}, ""},
		{"只排序", []string{"", "size", ""}, "sort=size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := cli.GetTorrents(tc.params...); err != nil {
				t.Fatalf("GetTorrents: %v", err)
			}
			if gotQuery != tc.want {
				t.Errorf("query = %q, want %q", gotQuery, tc.want)
			}
		})
	}
}

func TestClient_GetTorrents_DoError(t *testing.T) {
	srv, cli := newQBMock(t, nil, nil)
	if err := cli.login(); err != nil {
		t.Fatal(err)
	}
	srv.Close()

	if _, err := cli.GetTorrents(); err == nil {
		t.Fatal("服务器不可达应返回错误")
	}
}

// 响应体读取失败（data 为 nil）→ 走「非 qB 错误串」的兜底日志分支
func TestClient_GetTorrents_ReadAllError(t *testing.T) {
	_, cli := newQBMock(t, nil, nil)
	useStub(cli, func(path string, n int, req *http.Request) (*http.Response, error) {
		if path == "/api/v2/auth/login" {
			return stubLoginOK(req), nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       stubErrBody{},
			Request:    req,
		}, nil
	})

	if _, err := cli.GetTorrents(); err == nil {
		t.Fatal("响应体读取失败应返回错误")
	}
}

// qB 返回错误字符串、强制重登也失败
func TestClient_GetTorrents_ForcedReloginFails(t *testing.T) {
	// 第一次登录成功；GetTorrents 发现错误串后强制重登 → 第二次登录失败
	_, cli := newQBMock(t, []string{"Ok.", "Fails."}, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`"internal error"`))
	})

	_, err := cli.GetTorrents()
	if err == nil {
		t.Fatal("应返回错误")
	}
	if !strings.Contains(err.Error(), "auto re-login failed") {
		t.Errorf("应包含 auto re-login failed，got %v", err)
	}
}

// qB 返回错误字符串、重登成功但重试仍失败
func TestClient_GetTorrents_RetryStillFails(t *testing.T) {
	var mu sync.Mutex
	n := 0
	_, cli := newQBMock(t, nil, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		cur := n
		mu.Unlock()
		if cur == 1 {
			_, _ = w.Write([]byte(`"internal error"`))
			return
		}
		_, _ = w.Write([]byte(`not-json-after-retry`))
	})

	_, err := cli.GetTorrents()
	if err == nil {
		t.Fatal("重试仍失败时应返回错误")
	}
	if !strings.Contains(err.Error(), "parse error") {
		t.Errorf("应包含 parse error，got %v", err)
	}
}

// 非 JSON 且不是 qB 错误串 → 直接返回解析错误，不重登
func TestClient_GetTorrents_NonQBErrorNotRetried(t *testing.T) {
	var mu sync.Mutex
	loginCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		loginCount++
		mu.Unlock()
		w.Header().Set("Set-Cookie", "SID=x; Path=/")
		_, _ = w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{ not json`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cli := New(srv.URL, "u", "p", "")
	if _, err := cli.GetTorrents(); err == nil {
		t.Fatal("非法 JSON 应返回错误")
	}
	mu.Lock()
	got := loginCount
	mu.Unlock()
	if got != 1 {
		t.Errorf("非 qB 错误串不应触发强制重登，login count=%d want 1", got)
	}
}

// ============ GetTransferInfo / GetTorrentFiles 错误分支 ============

func TestClient_GetTransferInfo_Errors(t *testing.T) {
	t.Run("请求失败", func(t *testing.T) {
		srv, cli := newQBMock(t, nil, nil)
		if err := cli.login(); err != nil {
			t.Fatal(err)
		}
		srv.Close()
		if _, err := cli.GetTransferInfo(); err == nil {
			t.Fatal("服务器不可达应返回错误")
		}
	})

	t.Run("响应体读取失败", func(t *testing.T) {
		_, cli := newQBMock(t, nil, nil)
		useStub(cli, func(path string, n int, req *http.Request) (*http.Response, error) {
			if path == "/api/v2/auth/login" {
				return stubLoginOK(req), nil
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: stubErrBody{}, Request: req}, nil
		})
		if _, err := cli.GetTransferInfo(); err == nil {
			t.Fatal("响应体读取失败应返回错误")
		}
	})

	t.Run("JSON 非法", func(t *testing.T) {
		_, cli := newQBMock(t, nil, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`not-json`))
		})
		if _, err := cli.GetTransferInfo(); err == nil {
			t.Fatal("非法 JSON 应返回错误")
		}
	})
}

func TestClient_GetTorrentFiles_DoAndReadErrors(t *testing.T) {
	srv, cli := newQBMock(t, nil, nil)
	if err := cli.login(); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if _, err := cli.GetTorrentFiles("h1"); err == nil {
		t.Fatal("服务器不可达应返回错误")
	}

	cli2 := New("http://qb.example.com", "u", "p", "")
	useStub(cli2, func(path string, n int, req *http.Request) (*http.Response, error) {
		if path == "/api/v2/auth/login" {
			return stubLoginOK(req), nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: stubErrBody{}, Request: req}, nil
	})
	if _, err := cli2.GetTorrentFiles("h1"); err == nil {
		t.Fatal("响应体读取失败应返回错误")
	}
}

// ============ 各写接口的失败分支 ============

func TestClient_WriteAPIs_FailureBranches(t *testing.T) {
	// 1) 请求直接失败（服务器不可达）
	srv, cli := newQBMock(t, nil, nil)
	if err := cli.login(); err != nil {
		t.Fatal(err)
	}
	srv.Close()

	if err := cli.SetUploadLimit("h", 1); err == nil {
		t.Error("SetUploadLimit 不可达应报错")
	}
	if err := cli.StopTorrents("h"); err == nil {
		t.Error("StopTorrents 不可达应报错")
	}
	if err := cli.StartTorrents("h"); err == nil {
		t.Error("StartTorrents 不可达应报错")
	}
	if err := cli.RenameFile("h", "a", "b"); err == nil {
		t.Error("RenameFile 不可达应报错")
	}
	if err := cli.AddTorrent([]byte("x"), "", "", "", 0, false); err == nil {
		t.Error("AddTorrent 不可达应报错")
	}

	// 2) 服务器返回 500
	_, cli2 := newQBMock(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})
	if err := cli2.SetUploadLimit("h", 1); err == nil {
		t.Error("SetUploadLimit 500 应报错")
	}
	if err := cli2.StartTorrents("h"); err == nil {
		t.Error("StartTorrents 500 应报错")
	}
	if err := cli2.AddTorrent([]byte("x"), "", "", "", 0, false); err != nil {
		t.Errorf("AddTorrent 即便 500 也只记日志不返回错误，got %v", err)
	}
}

// AddTorrent 带 category/tags 的表单字段都要写入
func TestClient_AddTorrent_WithCategoryAndTags(t *testing.T) {
	var gotBody string
	var mu sync.Mutex
	_, cli := newQBMock(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err == nil {
			mu.Lock()
			gotBody = r.FormValue("category") + "|" + r.FormValue("tags") + "|" + r.FormValue("savepath")
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	})

	if err := cli.AddTorrent([]byte("fake-torrent"), "/dl", "tv", "hot,free", 0, false); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	mu.Lock()
	got := gotBody
	mu.Unlock()
	if got != "tv|hot,free|/dl" {
		t.Errorf("表单字段错误: got %q want %q", got, "tv|hot,free|/dl")
	}
}

// ============ applyLatestUploadLimit ============

// API key 模式：请求应带上 Authorization 头（401 也不触发 cookie 重登）
func TestClient_doRaw_APIKeyAuthorizationHeader(t *testing.T) {
	var mu sync.Mutex
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		_, _ = w.Write([]byte(`"5.2.0"`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cli := New(srv.URL, "", "", "qbt_key123")
	if err := cli.TestConnection(); err != nil {
		t.Fatalf("API key 模式请求应成功: %v", err)
	}
	mu.Lock()
	got := gotAuth
	mu.Unlock()
	if got != "Bearer qbt_key123" {
		t.Errorf("Authorization 头错误: got %q want %q", got, "Bearer qbt_key123")
	}
	if cli.GetCookie() != "" {
		t.Errorf("API key 模式不应产生 cookie，got %q", cli.GetCookie())
	}
}

// 401 → 自动重登失败 → 返回登录错误
func TestClient_doRaw_UnauthorizedReloginFails(t *testing.T) {
	// 第一次登录成功，401 之后的自动重登返回失败
	_, cli := newQBMock(t, []string{"Ok.", "Fails."}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/torrents/setUploadLimit" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	err := cli.SetUploadLimit("h", 100)
	if err == nil {
		t.Fatal("401 且重登失败时应返回错误")
	}
	if !strings.Contains(err.Error(), "login failed") {
		t.Errorf("错误信息应来自 login failed，got %v", err)
	}
}

// 401 → 重登成功 → 重试请求必须保留 Content-Type
func TestClient_doRaw_UnauthorizedRetryKeepsContentType(t *testing.T) {
	var mu sync.Mutex
	retries := 0
	var gotCT string
	_, cli := newQBMock(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/torrents/setUploadLimit" {
			w.WriteHeader(http.StatusOK)
			return
		}
		mu.Lock()
		retries++
		n := retries
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		gotCT = r.Header.Get("Content-Type")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	if err := cli.SetUploadLimit("h", 100); err != nil {
		t.Fatalf("401 后应重登并重试成功: %v", err)
	}
	mu.Lock()
	ct := gotCT
	mu.Unlock()
	if ct != "application/x-www-form-urlencoded" {
		t.Errorf("重试请求 Content-Type 错误: got %q", ct)
	}
}

func TestClient_applyLatestUploadLimit(t *testing.T) {
	t.Run("拉取列表失败直接返回", func(t *testing.T) {
		srv, cli := newQBMock(t, nil, nil)
		if err := cli.login(); err != nil {
			t.Fatal(err)
		}
		srv.Close()
		cli.applyLatestUploadLimit(100) // 不应 panic
	})

	t.Run("列表为空直接返回", func(t *testing.T) {
		_, cli := newQBMock(t, nil, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[]`))
		})
		cli.applyLatestUploadLimit(100)
	})

	t.Run("命中下载中任务则下发限速", func(t *testing.T) {
		var mu sync.Mutex
		var gotLimit string
		_, cli := newQBMock(t, nil, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v2/torrents/setUploadLimit" {
				_ = r.ParseForm()
				mu.Lock()
				gotLimit = r.FormValue("hashes") + ":" + r.FormValue("limit")
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
				return
			}
			_, _ = w.Write([]byte(`[{"hash":"h1","name":"a","state":"uploading"},{"hash":"h2","name":"b","state":"stalledDL"}]`))
		})

		cli.applyLatestUploadLimit(512)
		mu.Lock()
		got := gotLimit
		mu.Unlock()
		if got != "h2:524288" {
			t.Errorf("应对第一个 downloading/stalledDL 任务下发限速，got %q want h2:524288", got)
		}
	})

	t.Run("没有下载中任务则不下发", func(t *testing.T) {
		var mu sync.Mutex
		calls := 0
		_, cli := newQBMock(t, nil, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v2/torrents/setUploadLimit" {
				mu.Lock()
				calls++
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
				return
			}
			_, _ = w.Write([]byte(`[{"hash":"h1","name":"a","state":"uploading"}]`))
		})

		cli.applyLatestUploadLimit(512)
		mu.Lock()
		got := calls
		mu.Unlock()
		if got != 0 {
			t.Errorf("没有下载中任务时不应下发，calls=%d", got)
		}
	})
}

// AddTorrent(uploadLimitKB>0) 会起 goroutine，在 3 秒后调用 applyLatestUploadLimit。
// 这里轮询等待该调用发生（上限 5 秒），确保 goroutine 内的语句被覆盖且测试退出前已结束。
func TestClient_AddTorrent_AppliesUploadLimitAsynchronously(t *testing.T) {
	var mu sync.Mutex
	applied := false
	_, cli := newQBMock(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/torrents/setUploadLimit" {
			_ = r.ParseForm()
			mu.Lock()
			applied = r.FormValue("limit") == "1048576"
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/api/v2/torrents/add" {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write([]byte(`[{"hash":"h1","name":"a","state":"downloading"}]`))
	})

	if err := cli.AddTorrent([]byte("fake-torrent"), "/dl", "", "", 1024, false); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ok := applied
		mu.Unlock()
		if ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("等待 applyLatestUploadLimit 超时（源码固定 3 秒延迟）")
}
