package filemgr

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/models"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

// setupCacheFilemgr 造一个「快照与直连返回不同内容」的环境：
// 快照里是 from-cache，直连 /torrents/info 里是 from-direct 并计数。
// 两者可区分，才能证明扫描到底读了哪一边。
func setupCacheFilemgr(t *testing.T, ready bool) (*Manager, *int, *qb.TorrentCache) {
	t.Helper()
	var mu sync.Mutex
	infoCalls := new(int)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "SID=test")
		_, _ = w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/sync/maindata", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"rid":1,"full_update":true,"torrents":{
			"c1":{"hash":"c1","name":"from-cache","state":"downloading","progress":0.5,"added_on":100}
		},"server_state":{}}`))
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*infoCalls++
		mu.Unlock()
		_, _ = w.Write([]byte(`[{"hash":"d1","name":"from-direct","state":"downloading","progress":0.5,"added_on":100}]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("QBHIVE_CONFIG", cfgPath)
	cfg := config.New(cfgPath)
	cfg.Set(models.AppConfig{FileManager: models.FileManagerConfig{Enabled: true, ScanInterval: 15}})

	cache := qb.NewTorrentCache(srv.URL, "u", "p", "")
	if ready {
		if err := cache.Refresh(); err != nil {
			t.Fatalf("快照首次同步失败: %v", err)
		}
	}
	m := New(cfg, qb.New(srv.URL, "u", "p", ""))
	return m, infoCalls, cache
}

func TestManager_allTorrents_PrefersCache(t *testing.T) {
	m, calls, cache := setupCacheFilemgr(t, true)
	m.SetTorrentCache(cache)

	list, err := m.allTorrents()
	if err != nil {
		t.Fatalf("allTorrents: %v", err)
	}
	if len(list) != 1 || list[0].Name != "from-cache" {
		t.Fatalf("应读快照内容，got %+v", list)
	}
	if *calls != 0 {
		t.Errorf("快照可用时不应直连 /torrents/info，实际 %d 次", *calls)
	}
}

func TestManager_allTorrents_FallsBackWhenNotReady(t *testing.T) {
	m, calls, cache := setupCacheFilemgr(t, false)
	m.SetTorrentCache(cache)

	list, err := m.allTorrents()
	if err != nil {
		t.Fatalf("allTorrents: %v", err)
	}
	if len(list) != 1 || list[0].Name != "from-direct" {
		t.Fatalf("快照未就绪应回退直连，got %+v", list)
	}
	if *calls != 1 {
		t.Errorf("应直连一次，实际 %d 次", *calls)
	}
}

func TestManager_allTorrents_FallsBackWithoutCache(t *testing.T) {
	m, calls, _ := setupCacheFilemgr(t, true)

	list, err := m.allTorrents()
	if err != nil {
		t.Fatalf("allTorrents: %v", err)
	}
	if len(list) != 1 || list[0].Name != "from-direct" {
		t.Fatalf("未注入快照应直连，got %+v", list)
	}
	if *calls != 1 {
		t.Errorf("应直连一次，实际 %d 次", *calls)
	}
}

// 扫描是周期性的（默认 15s），必须走快照：直连全表在 7000+ 任务时单次 20MB。
// 把 scan() 改回 m.client.GetTorrents() 这条测试就会红。
func TestManager_scan_UsesCacheNotFullTable(t *testing.T) {
	m, calls, cache := setupCacheFilemgr(t, true)
	m.SetTorrentCache(cache)

	m.scan()

	if *calls != 0 {
		t.Errorf("扫描不应直连 /torrents/info，实际 %d 次", *calls)
	}
}
