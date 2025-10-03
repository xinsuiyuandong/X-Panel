package global

import (
	"context"
	_ "unsafe"

	"github.com/robfig/cron/v3"
)

type TgBotInterface interface {
	// 这些方法需与 service 包中 TelegramService / Tgbot 的方法集合保持兼容
	SendMessage(msg string) error
	SendSubconverterSuccess()
	IsRunning() bool
}

var (
	webServer WebServer
	subServer SubServer
	// 新增：全局的 Telegram Bot 引用（接口类型）
	TgBot TelegramService
)

type WebServer interface {
	GetCron() *cron.Cron
	GetCtx() context.Context
}

type SubServer interface {
	GetCtx() context.Context
}

func SetWebServer(s WebServer) {
	webServer = s
}

func GetWebServer() WebServer {
	return webServer
}

func SetSubServer(s SubServer) {
	subServer = s
}

func GetSubServer() SubServer {
	return subServer
}

// 〔中文注释〕: 设置 / 获取全局 TgBot 引用（由 main 或 service 在启动时注入）。
func SetTgBot(t TelegramService) {
	TgBot = t
}
