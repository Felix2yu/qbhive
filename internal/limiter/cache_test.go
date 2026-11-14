package limiter

import (
	"testing"

	"github.com/Felix2yu/qbhive/internal/models"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

const cacheMaindata = `{"rid":1,"full_update":true,"torrents":{
	"c1":{"hash":"c1","name":"from-cache","state":"uploading","progress":1,"added_on":100}
},"server_state":{}}`

const cacheDirectList = `[{"hash":"d1","name":"from-direct","state":"uploading","progress":1,"added_on":100}]`

// newCacheEnv 造一个「快照与直连内容可区分」的环境：快照里是 from-cache，
// 直连 torrents/info 里是 from-direct 并计数。
func newCacheEnv(t *testing.T, ready bool) (*limiterEnv, *qb.TorrentCache) {
	t.Helper()
	env := newLimiterEnv(t, []models.LimitRule{
		{ID: "c", Name: "cache", Enabled: true, Match: `(?i)cache`, UploadLimit: 10240},
	}, cacheDirectList)
	env.setMaindata(cacheMaindata)

	cache := qb.NewTorrentCache(env.srv.URL, "u", "p", "")
	if ready {
		if err := cache.Refresh(); err != nil {
			t.Fatalf("快照首次同步失败: %v", err)
		}
	}
	return env, cache
}

func TestLimiter_allTorrents_PrefersCache(t *testing.T) {
	env, cache := newCacheEnv(t, true)
	env.l.SetTorrentCache(cache)

	list, err := env.l.allTorrents()
	if err != nil {
		t.Fatalf("allTorrents: %v", err)
	}
	if len(list) != 1 || list[0].Name != "from-cache" {
		t.Fatalf("应读快照内容，got %+v", list)
	}
	if env.infoCount() != 0 {
		t.Errorf("快照可用时不应直连 torrents/info，实际 %d 次", env.infoCount())
	}
}

func TestLimiter_allTorrents_FallsBackWhenNotReady(t *testing.T) {
	env, cache := newCacheEnv(t, false)
	env.l.SetTorrentCache(cache)

	list, err := env.l.allTorrents()
	if err != nil {
		t.Fatalf("allTorrents: %v", err)
	}
	if len(list) != 1 || list[0].Name != "from-direct" {
		t.Fatalf("快照未就绪应回退直连，got %+v", list)
	}
	if env.infoCount() != 1 {
		t.Errorf("应直连一次，实际 %d 次", env.infoCount())
	}
}

func TestLimiter_allTorrents_FallsBackWithoutCache(t *testing.T) {
	env, _ := newCacheEnv(t, true)

	list, err := env.l.allTorrents()
	if err != nil {
		t.Fatalf("allTorrents: %v", err)
	}
	if len(list) != 1 || list[0].Name != "from-direct" {
		t.Fatalf("未注入快照应直连，got %+v", list)
	}
	if env.infoCount() != 1 {
		t.Errorf("应直连一次，实际 %d 次", env.infoCount())
	}
}

// 压速必须作用在快照里的任务上，且整轮零直连全表。
// 把 apply() 改回 l.client.GetTorrents("all") 这条测试就会红。
func TestLimiter_apply_UsesCacheNotFullTable(t *testing.T) {
	env, cache := newCacheEnv(t, true)
	env.l.SetTorrentCache(cache)

	env.l.apply()

	got := env.calls()
	if len(got) != 1 || got[0] != "c1:10485760" {
		t.Fatalf("应对快照里的 c1 下发限速，got %v", got)
	}
	if env.infoCount() != 0 {
		t.Errorf("压速轮不应直连 torrents/info，实际 %d 次", env.infoCount())
	}
}
