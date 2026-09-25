package notifier

import (
	"github.com/Felix2yu/qbhive/internal/logger"
	"github.com/Felix2yu/qbhive/internal/models"
	"github.com/unraid/apprise-go"
)

// Notifier 基于 github.com/unraid/apprise-go 实现统一通知
type Notifier struct {
	cfg     models.NotifierConfig
	client  *apprise.Apprise
}

func New(cfg models.NotifierConfig) *Notifier {
	n := &Notifier{client: apprise.New()}
	n.Reload(cfg)
	return n
}

func (n *Notifier) Reload(cfg models.NotifierConfig) {
	n.cfg = cfg
	n.client = apprise.New()
	if cfg.Enabled && len(cfg.AppriseURLs) > 0 {
		if err := n.client.AddAll(cfg.AppriseURLs...); err != nil {
			logger.Warn.Printf("notifier reload: add urls failed: %v", err)
		}
	}
}

func (n *Notifier) Enabled() bool {
	return n.cfg.Enabled && len(n.cfg.AppriseURLs) > 0
}

// Send 发送通知
func (n *Notifier) Notify(title, body string) error {
	if !n.Enabled() {
		return nil
	}
	return n.client.Send(body,
		apprise.WithTitle(title),
		apprise.WithNotifyType(apprise.NotifyInfo),
	)
}

// Test 发送一条测试通知
func (n *Notifier) Test(cfg models.NotifierConfig) error {
	if !cfg.Enabled || len(cfg.AppriseURLs) == 0 {
		return nil
	}
	a := apprise.New()
	if err := a.AddAll(cfg.AppriseURLs...); err != nil {
		return err
	}
	return a.Send("这是一条来自 QBHive 的测试通知 🎉",
		apprise.WithTitle("QBHive 通知测试"),
		apprise.WithNotifyType(apprise.NotifySuccess),
	)
}
