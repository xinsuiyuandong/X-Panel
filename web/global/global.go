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
	// 中文注释：在这里新增一个全局变量，用于存放 Telegram Bot 实例
	// 这样其他文件（server.go、inbound.go 等）就能通过 global.TgBot 调用它
	TgBot TgBotInterface
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
