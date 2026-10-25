package qbittorrent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// mockMaindata 模拟 qB 的登录 + sync/maindata 应答。
func mockMaindata(t *testing.T, bodies []string) (*httptest.Server, *TorrentCache) {
	t.Helper()
	i := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "SID=testsid; Path=/")
		_, _ = w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/sync/maindata", func(w http.ResponseWriter, r *http.Request) {
		if i >= len(bodies) {
			t.Errorf("maindata 调用次数超出预期（第 %d 次）", i+1)
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write([]byte(bodies[i]))
		i++
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, NewTorrentCache(srv.URL, "user", "pass", "")
}

func TestTorrentCache_NotReadyBeforeFirstSync(t *testing.T) {
	c := NewTorrentCache("http://127.0.0.1:1", "u", "p", "")
	if c.Ready() {
		t.Fatal("cache should not be ready before first sync")
	}
	if _, err := c.GetTorrents("all"); err != ErrCacheNotReady {
		t.Fatalf("expected ErrCacheNotReady, got %v", err)
	}
	if _, ok := c.Stats(); ok {
		t.Fatal("stats should not be available before first sync")
	}
}

func TestTorrentCache_FullThenDelta(t *testing.T) {
	full := `{"rid":1,"full_update":true,
		"torrents":{
			"a":{"hash":"a","name":"Alpha","state":"downloading","progress":0.5,"size":1000,"dlspeed":100,"upspeed":0,"added_on":100},
			"b":{"hash":"b","name":"Beta","state":"stoppedUP","progress":1,"size":2000,"dlspeed":0,"upspeed":0,"added_on":200}
		},
		"server_state":{"dl_info_speed":123,"up_info_speed":456,"dl_rate_limit":0,"up_rate_limit":789}}`
	delta := `{"rid":2,"torrents":{"a":{"name":"Alpha2","dlspeed":200}},"torrents_removed":["b"],
		"server_state":{"dl_info_speed":321}}`

	_, c := mockMaindata(t, []string{full, delta})
	if err := c.Refresh(); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if !c.Ready() {
		t.Fatal("cache should be ready after full sync")
	}
	list, err := c.GetTorrents("all", "added_on", "true")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Hash != "b" {
		t.Fatalf("unexpected full list: %+v", list)
	}

	if err := c.Refresh(); err != nil {
		t.Fatalf("delta sync: %v", err)
	}
	list, _ = c.GetTorrents("all", "added_on", "false")
	if len(list) != 1 {
		t.Fatalf("removed torrent should be gone, got %+v", list)
	}
	got := list[0]
	if got.Name != "Alpha2" || got.DownloadSpeed != 200 {
		t.Errorf("delta fields not applied: %+v", got)
	}
	// 增量里没出现的字段必须保留
	if got.State != "downloading" || got.AddedOn != 100 || got.Size != 1000 {
		t.Errorf("unchanged fields lost after merge: %+v", got)
	}

	st, ok := c.Stats()
	if !ok {
		t.Fatal("stats not ready")
	}
	if st.DlSpeed != 321 || st.UpSpeed != 456 || st.UpSpeedLimit != 789 {
		t.Errorf("server_state delta merge wrong: %+v", st)
	}
	if st.ActiveCount != 1 || st.DownloadingCount != 1 || st.StoppedUP != 0 {
		t.Errorf("stats counts wrong after delta: %+v", st)
	}
}

func TestTorrentCache_StatsAndFilters(t *testing.T) {
	full := `{"rid":1,"full_update":true,"torrents":{
		"a":{"hash":"a","state":"downloading","dlspeed":10},
		"b":{"hash":"b","state":"stalledUP"},
		"c":{"hash":"c","state":"stalledDL"},
		"d":{"hash":"d","state":"stoppedUP","progress":1,"upspeed":5},
		"e":{"hash":"e","state":"missingFiles"},
		"f":{"hash":"f","state":"error"},
		"g":{"hash":"g","state":"queuedDL"},
		"h":{"hash":"h","state":"uploading","upspeed":7}
	},"server_state":{}}`
	_, c := mockMaindata(t, []string{full})
	if err := c.Refresh(); err != nil {
		t.Fatal(err)
	}

	tests := map[string]int{
		"all":               8,
		"downloading":       3, // downloading + queuedDL + stalledDL
		"seeding":           2, // stalledUP + uploading
		"completed":         3, // stalledUP + stoppedUP + uploading
		"stopped":           3, // stoppedUP + missingFiles + error
		"stalled":           2,
		"stalled_uploading": 1,
		"errored":           2,
		"active":            3, // a, d, h
		"inactive":          5,
	}
	for filter, want := range tests {
		got, err := c.GetTorrents(filter)
		if err != nil {
			t.Fatalf("%s: %v", filter, err)
		}
		if len(got) != want {
			t.Errorf("filter %q: got %d, want %d", filter, len(got), want)
		}
	}

	st, _ := c.Stats()
	if st.ActiveCount != 3 || st.DownloadingCount != 1 || st.SeedingCount != 1 ||
		st.StoppedUP != 1 || st.StalledCount != 2 || st.ErroredCount != 2 {
		t.Errorf("stats mismatch: %+v", st)
	}
}

func TestTorrentCache_SortsBeforeLimit(t *testing.T) {
	full := `{"rid":1,"full_update":true,"torrents":{
		"a":{"hash":"a","name":"b","added_on":100},
		"b":{"hash":"b","name":"a","added_on":300},
		"c":{"hash":"c","name":"c","added_on":200}
	},"server_state":{}}`
	_, c := mockMaindata(t, []string{full})
	if err := c.Refresh(); err != nil {
		t.Fatal(err)
	}
	list, _ := c.GetTorrents("all", "added_on", "true")
	if got := []string{list[0].Hash, list[1].Hash, list[2].Hash}; got[0] != "b" || got[1] != "c" || got[2] != "a" {
		t.Errorf("added_on desc order wrong: %v", got)
	}
	list, _ = c.GetTorrents("all", "name", "")
	if list[0].Name != "a" || list[2].Name != "c" {
		t.Errorf("name asc order wrong: %+v", list)
	}
}

func TestTorrentCache_DisabledDeltaWithoutPassword(t *testing.T) {
	// 只有 API key：qB 的 rid 游标不生效，必须走整表刷新
	var infoCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		infoCalls++
		if r.Header.Get("X-Webapi-Key") != "key123" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`[{"hash":"x","name":"X","state":"uploading"}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewTorrentCache(srv.URL, "", "", "key123")
	if err := c.Refresh(); err != nil {
		t.Fatalf("fallback sync: %v", err)
	}
	if infoCalls != 1 {
		t.Fatalf("expected torrents/info fallback, calls=%d", infoCalls)
	}
	list, err := c.GetTorrents("all")
	if err != nil || len(list) != 1 || list[0].Hash != "x" {
		t.Fatalf("fallback list wrong: %v %+v", err, list)
	}
}

func TestTorrentCache_KeepOldDataWhenSyncFails(t *testing.T) {
	full := `{"rid":1,"full_update":true,"torrents":{"a":{"hash":"a","state":"stoppedUP"}},"server_state":{}}`
	srv, c := mockMaindata(t, []string{full})
	if err := c.Refresh(); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if err := c.Refresh(); err == nil {
		t.Fatal("expected sync error after server close")
	}
	if !c.Ready() {
		t.Fatal("stale snapshot should stay ready")
	}
	if list, _ := c.GetTorrents("all"); len(list) != 1 {
		t.Fatalf("stale snapshot should keep serving, got %+v", list)
	}
}

func TestClient_GetMaindata(t *testing.T) {
	srv, cli := newMockQB(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/sync/maindata" {
			w.WriteHeader(404)
			return
		}
		if r.URL.Query().Get("rid") != "7" {
			t.Errorf("rid not forwarded: %q", r.URL.RawQuery)
		}
		if r.Header.Get("Cookie") == "" {
			t.Error("maindata must use cookie session, not API key")
		}
		_, _ = w.Write([]byte(`{"rid":8,"full_update":false,"torrents":{"a":{"name":"A"}},"torrents_removed":["b"],"server_state":{"dl_info_speed":5}}`))
	})
	defer srv.Close()

	resp, err := cli.GetMaindata(7)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Rid != 8 || resp.FullUpdate {
		t.Errorf("unexpected rid/full: %+v", resp)
	}
	if len(resp.Torrents) != 1 || len(resp.TorrentsRemoved) != 1 {
		t.Errorf("unexpected payload: %+v", resp)
	}
	var name struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(resp.Torrents["a"], &name); err != nil || name.Name != "A" {
		t.Errorf("raw torrent parse failed: %v %+v", err, name)
	}
}

// 错误密码 + 配置了 API key：只尝试一次 cookie 登录，随后切到 API key 整表轮询，
// 避免每 5 秒重试错误密码触发 qB 的登录封禁。
func TestTorrentCache_LoginFailureFallsBackToAPIKey(t *testing.T) {
	var logins, infoCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		logins++
		_, _ = w.Write([]byte("Fails."))
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		infoCalls++
		if r.Header.Get("X-Webapi-Key") != "key123" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`[{"hash":"k","state":"uploading"}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewTorrentCache(srv.URL, "u", "wrong-pass", "key123")
	if err := c.Refresh(); err == nil {
		t.Fatal("错误密码的首次同步应返回错误")
	}
	if err := c.Refresh(); err != nil {
		t.Fatalf("降级到 API key 后应同步成功: %v", err)
	}
	if logins != 1 {
		t.Errorf("错误凭据不应反复登录，实际尝试 %d 次", logins)
	}
	if infoCalls != 1 {
		t.Errorf("应走一次 torrents/info 全量，实际 %d 次", infoCalls)
	}
	if list, _ := c.GetTorrents("all"); len(list) != 1 || list[0].Hash != "k" {
		t.Errorf("降级数据不正确: %+v", list)
	}
}

func TestTorrentCache_BackoffGrowsAndResets(t *testing.T) {
	c := NewTorrentCache("http://127.0.0.1:1", "u", "p", "")
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := c.backoff(); got != w {
			t.Fatalf("第 %d 次退避: got %s, want %s", i+1, got, w)
		}
	}
	c.resetBackoff()
	if got := c.backoff(); got != 5*time.Second {
		t.Fatalf("成功后应重置退避，got %s", got)
	}
}

func TestTorrentCache_PollInterval(t *testing.T) {
	delta := NewTorrentCache("http://x", "u", "p", "")
	if got := delta.pollInterval(); got != 2*time.Second {
		t.Errorf("增量模式应 2s 轮询，got %s", got)
	}
	fallback := NewTorrentCache("http://x", "", "", "key")
	if got := fallback.pollInterval(); got != 10*time.Second {
		t.Errorf("整表模式应 10s 轮询，got %s", got)
	}
	delta.cmu.Lock()
	delta.deltaDisabled = true
	delta.cmu.Unlock()
	if got := delta.pollInterval(); got != 10*time.Second {
		t.Errorf("登录失败降级后应 10s 轮询，got %s", got)
	}
}
