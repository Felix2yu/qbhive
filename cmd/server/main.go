package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/filemgr"
	"github.com/Felix2yu/qbhive/internal/limiter"
	"github.com/Felix2yu/qbhive/internal/logger"
	"github.com/Felix2yu/qbhive/internal/notifier"
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

	qbCli := qb.New(cfg.Qbittorrent.URL, cfg.Qbittorrent.Username, cfg.Qbittorrent.Password, cfg.Qbittorrent.APIKey)
	if err := qbCli.TestConnection(); err != nil {
		logger.Warn.Printf("qBittorrent 连接测试失败：%v（继续启动）", err)
	} else {
		logger.Info.Println("qBittorrent 连接正常")
	}

	notif := notifier.New(cfg.Notifier)
	lim := limiter.New(cfgMgr, qbCli)
	fm := filemgr.New(cfgMgr, qbCli)
	rssE := rss.New(cfgMgr, qbCli)

	sch := scheduler.New(cfgMgr, qbCli, notif, lim, fm, rssE)
	sch.Start()

	srv := web.New(cfgMgr, qbCli, lim, rssE, notif, sch)

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
