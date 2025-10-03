package global

import (
	"context"
	_ "unsafe"

	"x-ui/database/model"
	
	"github.com/robfig/cron/v3"
)

type TelegramService interface {
	SendMessage(msg string) error
	SendSubconverterSuccess()
	IsRunning() bool
    // 【中文注释】: 这个方法是在 inbound.go 中被调用的，所以也必须包含在接口定义里。
	SendOneClickConfig(ib *model.Inbound, inFromPanel bool) error
}

type TgBotInterface interface {
	SendMessage(msg string) error
	SendSubconverterSuccess()
	IsRunning() bool
}

var (
	webServer WebServer
	subServer SubServer
	// 定义全局变量  TelegramService / Tgbot 的接口方法
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
