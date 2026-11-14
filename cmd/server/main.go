package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/Felix2yu/qbhive/internal/archive"
	"github.com/Felix2yu/qbhive/internal/categorizer"
	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/filemgr"
	"github.com/Felix2yu/qbhive/internal/limiter"
	"github.com/Felix2yu/qbhive/internal/logger"
	"github.com/Felix2yu/qbhive/internal/notifier"
	"github.com/Felix2yu/qbhive/internal/proxy"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
	"github.com/Felix2yu/qbhive/internal/rss"
	"github.com/Felix2yu/qbhive/internal/scheduler"
	"github.com/Felix2yu/qbhive/internal/web"
)

func main() {
	var (
		cfgFile = flag.String("config", defaultPath(), "path to config file")
		webDir  = flag.String("web", defaultWebDir(), "path to static web files (optional)")
	)
	flag.Parse()

	cfgMgr := config.New(*cfgFile)
	cfg := cfgMgr.Get()

	// 装配全局出网代理：必须早于任何 client（qB / apprise / RSS / AI）构造，
	// 因它们共享 http.DefaultTransport。localhost 目标恒定直连。
	proxy.Install(cfgMgr)
	logger.Info.Printf("代理：%s", proxy.Snapshot(cfgMgr))

	qbCli := qb.New(cfg.Qbittorrent.URL, cfg.Qbittorrent.Username, cfg.Qbittorrent.Password, cfg.Qbittorrent.APIKey)
	if err := qbCli.TestConnection(); err != nil {
		logger.Warn.Printf("qBittorrent 连接测试失败：%v（继续启动）", err)
	} else {
		logger.Info.Println("qBittorrent 连接正常")
	}

	// 共享任务快照：sync/maindata 增量维护，Web 列表/统计和后台扫描都读它，
	// 避免每次刷新让 qB 重新序列化全部任务（7000+ 条时单次 20MB）。
	torrents := qb.NewTorrentCache(cfg.Qbittorrent.URL, cfg.Qbittorrent.Username, cfg.Qbittorrent.Password, cfg.Qbittorrent.APIKey)
	torrents.Start()

	notif := notifier.New(cfg.Notifier)
	lim := limiter.New(cfgMgr, qbCli)
	lim.SetTorrentCache(torrents)
	fm := filemgr.New(cfgMgr, qbCli)
	fm.SetTorrentCache(torrents)
	// 文件管理的异常/不可逆动作走通知通道（未启用通知时 Notify 内部直接返回）
	fm.SetNotifier(func(title, body string) { _ = notif.Notify(title, body) })
	rssE := rss.New(cfgMgr, qbCli)

	sch := scheduler.New(cfgMgr, qbCli, notif, lim, fm, rssE)
	sch.SetTorrentCache(torrents)
	// 自动分类自带扫描循环，与文件管理互不依赖：关掉归档不影响分类，反之亦然
	cat := categorizer.New(cfgMgr, qbCli)
	sch.SetCategorizer(cat)
	// 归档退休：先建档（Sync 落盘）再移除任务，磁盘文件一律保留
	archStore := archive.NewStore(archive.DefaultPath())
	ret := archive.NewRetirer(cfgMgr, qbCli, archStore, fm)
	ret.SetNotifier(func(title, body string) { _ = notif.Notify(title, body) })
	sch.SetRetirer(ret)
	// 重复下载守卫：退休后同 infohash 被重新添加时按档位处置，默认 off
	guard := archive.NewGuard(cfgMgr, qbCli, archStore, fm)
	guard.SetNotifier(func(title, body string) { _ = notif.Notify(title, body) })
	sch.SetGuard(guard)
	sch.Start()

	srv := web.New(cfgMgr, qbCli, lim, rssE, notif, sch, fm)
	srv.SetTorrentCache(torrents)
	srv.SetCategorizer(cat)
	srv.SetArchive(archStore, ret)

	go func() {
		logger.Info.Printf("QBHive 监听地址 %s", cfg.Server.Listen)
		if err := srv.Start(*webDir); err != nil {
			logger.Error.Fatalf("服务启动失败：%v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	logger.Info.Println("正在关闭...")
	sch.Stop()
	torrents.Stop()
	_ = srv.Shutdown()
	logger.Info.Println("已退出")
}

func defaultPath() string {
	if v := os.Getenv("QBHIVE_CONFIG"); v != "" {
		return v
	}
	return "config.json"
}

func defaultWebDir() string {
	if v := os.Getenv("QBHIVE_WEB"); v != "" {
		return v
	}
	return "web/static"
}
