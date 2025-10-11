package service

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"encoding/json"    // 新增：用于 json.Marshal / Unmarshal
    "net/http"         // 新增：用于 http.Client / Transport
    "crypto/tls"       // 新增：用于 tls.Config
    "os/exec"          // 新增：用于 exec.Command（getDomain 等）
    "path/filepath"    // 新增：用于 filepath.Base / Dir（getDomain 用到）
	"io/ioutil" // 〔中文注释〕: 新增，用于读取 HTTP API 响应体。
	rng "math/rand"    // 用于随机排列
	"encoding/xml"   // 【新增】: 用于直接解析 RSS XML 响应体

	"x-ui/config"
	"x-ui/database"
	"x-ui/database/model"
	"x-ui/logger"
	"x-ui/util/common"
	"x-ui/web/global"
	"x-ui/web/locale"
	"x-ui/xray"

	"github.com/google/uuid"
	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpproxy"
	// 新增 qrcode 包，用于生成二维码
	"github.com/skip2/go-qrcode"
)


// 〔中文注释〕: 新增 TelegramService 接口，用于解耦 Job 和 Telegram Bot 的直接依赖。
// 任何实现了 SendMessage(msg string) error 方法的结构体，都可以被认为是 TelegramService。
type TelegramService interface {
	SendMessage(msg string) error
	SendSubconverterSuccess()
	IsRunning() bool
	// 您可以根据 server.go 的需要，在这里继续扩展接口
	// 〔中文注释〕: 将 SendOneClickConfig 方法添加到接口中，这样其他服务可以通过接口来调用它，
	// 实现了与具体实现 Tgbot 的解耦。
	SendOneClickConfig(inbound *model.Inbound, inFromPanel bool, chatId int64) error
	// 新增 GetDomain 方法签名，以满足 server.go 的调用需求
    GetDomain() (string, error)
}

var (
	bot         *telego.Bot
	botHandler  *th.BotHandler
	adminIds    []int64
	isRunning   bool
	hostname    string
	hashStorage *global.HashStorage

	// clients data to adding new client
	receiver_inbound_ID int
	client_Id           string
	client_Flow         string
	client_Email        string
	client_LimitIP      int
	client_TotalGB      int64
	client_ExpiryTime   int64
	client_Enable       bool
	client_TgID         string
	client_SubID        string
	client_Comment      string
	client_Reset        int
	client_Security     string
	client_ShPassword   string
	client_TrPassword   string
	client_Method       string
)

var userStates = make(map[int64]string)

type LoginStatus byte

const (
	LoginSuccess        LoginStatus = 1
	LoginFail           LoginStatus = 0
	EmptyTelegramUserID             = int64(0)
)

type Tgbot struct {
	inboundService *InboundService
	settingService *SettingService
	serverService *ServerService
	xrayService *XrayService
	lastStatus *Status
}

// 【新增方法】: 用于从外部注入 ServerService 实例
func (t *Tgbot) SetServerService(s *ServerService) {
	t.serverService = s
}

// 配合目前 main.go 代码结构实践。
func (t *Tgbot) SetInboundService(s *InboundService) {
	t.inboundService = s
}

// 〔中文注释〕: 在这里添加新的构造函数
// NewTgBot 创建并返回一个完全初始化的 Tgbot 实例。
// 这个函数确保了所有服务依赖项都被正确注入，避免了空指针问题。
func NewTgBot(
	inboundService *InboundService,
	settingService *SettingService,
	serverService *ServerService,
	xrayService *XrayService,
	lastStatus *Status,
) *Tgbot {
	return &Tgbot{
		inboundService: inboundService,
		settingService: settingService,
		serverService:  serverService,
		xrayService:    xrayService,
		lastStatus:     lastStatus,
	}
}

/*
func (t *Tgbot) NewTgbot() *Tgbot {
	return new(Tgbot)
}
*/

func (t *Tgbot) I18nBot(name string, params ...string) string {
	return locale.I18n(locale.Bot, name, params...)
}

func (t *Tgbot) GetHashStorage() *global.HashStorage {
	return hashStorage
}

func (t *Tgbot) Start(i18nFS embed.FS) error {
	// Initialize localizer
	err := locale.InitLocalizer(i18nFS, t.settingService)
	if err != nil {
		return err
	}

	// Initialize hash storage to store callback queries
	hashStorage = global.NewHashStorage(20 * time.Minute)

	t.SetHostname()

	// Get Telegram bot token
	tgBotToken, err := t.settingService.GetTgBotToken()
	if err != nil || tgBotToken == "" {
		logger.Warning("Failed to get Telegram bot token:", err)
		return err
	}

	// Get Telegram bot chat ID(s)
	tgBotID, err := t.settingService.GetTgBotChatId()
	if err != nil {
		logger.Warning("Failed to get Telegram bot chat ID:", err)
		return err
	}

	// Parse admin IDs from comma-separated string
	if tgBotID != "" {
		for _, adminID := range strings.Split(tgBotID, ",") {
			id, err := strconv.Atoi(adminID)
			if err != nil {
				logger.Warning("Failed to parse admin ID from Telegram bot chat ID:", err)
				return err
			}
			adminIds = append(adminIds, int64(id))
		}
	}

	// Get Telegram bot proxy URL
	tgBotProxy, err := t.settingService.GetTgBotProxy()
	if err != nil {
		logger.Warning("Failed to get Telegram bot proxy URL:", err)
	}

	// Get Telegram bot API server URL
	tgBotAPIServer, err := t.settingService.GetTgBotAPIServer()
	if err != nil {
		logger.Warning("Failed to get Telegram bot API server URL:", err)
	}

	// Create new Telegram bot instance
	bot, err = t.NewBot(tgBotToken, tgBotProxy, tgBotAPIServer)
	if err != nil {
		logger.Error("Failed to initialize Telegram bot API:", err)
		return err
	}

	// After bot initialization, set up bot commands with localized descriptions
	err = bot.SetMyCommands(context.Background(), &telego.SetMyCommandsParams{
		Commands: []telego.BotCommand{
			{Command: "start", Description: t.I18nBot("tgbot.commands.startDesc")},
			{Command: "help", Description: t.I18nBot("tgbot.commands.helpDesc")},
			{Command: "status", Description: t.I18nBot("tgbot.commands.statusDesc")},
			{Command: "id", Description: t.I18nBot("tgbot.commands.idDesc")},
			{Command: "oneclick", Description: "🚀 一键配置节点 (有可选项)"},
			{Command: "subconverter", Description: "🔄 检测或安装订阅转换"},
		},
	})
	if err != nil {
		logger.Warning("Failed to set bot commands:", err)
	}

	// Start receiving Telegram bot messages
	if !isRunning {
		logger.Info("Telegram bot receiver started")
		go t.OnReceive()
		isRunning = true
	}

	return nil
}

func (t *Tgbot) NewBot(token string, proxyUrl string, apiServerUrl string) (*telego.Bot, error) {
	if proxyUrl == "" && apiServerUrl == "" {
		return telego.NewBot(token)
	}

	if proxyUrl != "" {
		if !strings.HasPrefix(proxyUrl, "socks5://") {
			logger.Warning("Invalid socks5 URL, using default")
			return telego.NewBot(token)
		}

		_, err := url.Parse(proxyUrl)
		if err != nil {
			logger.Warningf("Can't parse proxy URL, using default instance for tgbot: %v", err)
			return telego.NewBot(token)
		}

		return telego.NewBot(token, telego.WithFastHTTPClient(&fasthttp.Client{
			Dial: fasthttpproxy.FasthttpSocksDialer(proxyUrl),
		}))
	}

	if !strings.HasPrefix(apiServerUrl, "http") {
		logger.Warning("Invalid http(s) URL, using default")
		return telego.NewBot(token)
	}

	_, err := url.Parse(apiServerUrl)
	if err != nil {
		logger.Warningf("Can't parse API server URL, using default instance for tgbot: %v", err)
		return telego.NewBot(token)
	}

	return telego.NewBot(token, telego.WithAPIServer(apiServerUrl))
}

func (t *Tgbot) IsRunning() bool {
	return isRunning
}

func (t *Tgbot) SetHostname() {
	host, err := os.Hostname()
	if err != nil {
		logger.Error("get hostname error:", err)
		hostname = ""
		return
	}
	hostname = host
}

func (t *Tgbot) Stop() {
	if botHandler != nil {
		botHandler.Stop()
	}
	logger.Info("Stop Telegram receiver ...")
	isRunning = false
	adminIds = nil
}

func (t *Tgbot) encodeQuery(query string) string {
	// NOTE: we only need to hash for more than 64 chars
	if len(query) <= 64 {
		return query
	}

	return hashStorage.SaveHash(query)
}

func (t *Tgbot) decodeQuery(query string) (string, error) {
	if !hashStorage.IsMD5(query) {
		return query, nil
	}

	decoded, exists := hashStorage.GetValue(query)
	if !exists {
		return "", common.NewError("hash not found in storage!")
	}

	return decoded, nil
}

func (t *Tgbot) OnReceive() {
	params := telego.GetUpdatesParams{
		Timeout: 10,
	}

	updates, _ := bot.UpdatesViaLongPolling(context.Background(), &params)

	botHandler, _ = th.NewBotHandler(bot, updates)

	botHandler.HandleMessage(func(ctx *th.Context, message telego.Message) error {
		delete(userStates, message.Chat.ID)
		t.SendMsgToTgbot(message.Chat.ID, t.I18nBot("tgbot.keyboardClosed"), tu.ReplyKeyboardRemove())
		return nil
	}, th.TextEqual(t.I18nBot("tgbot.buttons.closeKeyboard")))

	botHandler.HandleMessage(func(ctx *th.Context, message telego.Message) error {
		delete(userStates, message.Chat.ID)
		t.answerCommand(&message, message.Chat.ID, checkAdmin(message.From.ID))
		return nil
	}, th.AnyCommand())

	botHandler.HandleCallbackQuery(func(ctx *th.Context, query telego.CallbackQuery) error {
		delete(userStates, query.Message.GetChat().ID)
		t.answerCallback(&query, checkAdmin(query.From.ID))
		return nil
	}, th.AnyCallbackQueryWithMessage())

	botHandler.HandleMessage(func(ctx *th.Context, message telego.Message) error {
		if userState, exists := userStates[message.Chat.ID]; exists {
			switch userState {
			case "awaiting_id":
				if client_Id == strings.TrimSpace(message.Text) {
					t.SendMsgToTgbotDeleteAfter(message.Chat.ID, t.I18nBot("tgbot.messages.using_default_value"), 3, tu.ReplyKeyboardRemove())
					delete(userStates, message.Chat.ID)
					inbound, _ := t.inboundService.GetInbound(receiver_inbound_ID)
					message_text, _ := t.BuildInboundClientDataMessage(inbound.Remark, inbound.Protocol)
					t.addClient(message.Chat.ID, message_text)
					return nil
				}

				client_Id = strings.TrimSpace(message.Text)
				if t.isSingleWord(client_Id) {
					userStates[message.Chat.ID] = "awaiting_id"

					cancel_btn_markup := tu.InlineKeyboard(
						tu.InlineKeyboardRow(
							tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.use_default")).WithCallbackData("add_client_default_info"),
						),
					)

					t.SendMsgToTgbot(message.Chat.ID, t.I18nBot("tgbot.messages.incorrect_input"), cancel_btn_markup)
				} else {
					t.SendMsgToTgbotDeleteAfter(message.Chat.ID, t.I18nBot("tgbot.messages.received_id"), 3, tu.ReplyKeyboardRemove())
					delete(userStates, message.Chat.ID)
					inbound, _ := t.inboundService.GetInbound(receiver_inbound_ID)
					message_text, _ := t.BuildInboundClientDataMessage(inbound.Remark, inbound.Protocol)
					t.addClient(message.Chat.ID, message_text)
				}
			case "awaiting_password_tr":
				if client_TrPassword == strings.TrimSpace(message.Text) {
					t.SendMsgToTgbotDeleteAfter(message.Chat.ID, t.I18nBot("tgbot.messages.using_default_value"), 3, tu.ReplyKeyboardRemove())
					delete(userStates, message.Chat.ID)
					return nil
				}

				client_TrPassword = strings.TrimSpace(message.Text)
				if t.isSingleWord(client_TrPassword) {
					userStates[message.Chat.ID] = "awaiting_password_tr"

					cancel_btn_markup := tu.InlineKeyboard(
						tu.InlineKeyboardRow(
							tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.use_default")).WithCallbackData("add_client_default_info"),
						),
					)

					t.SendMsgToTgbot(message.Chat.ID, t.I18nBot("tgbot.messages.incorrect_input"), cancel_btn_markup)
				} else {
					t.SendMsgToTgbotDeleteAfter(message.Chat.ID, t.I18nBot("tgbot.messages.received_password"), 3, tu.ReplyKeyboardRemove())
					delete(userStates, message.Chat.ID)
					inbound, _ := t.inboundService.GetInbound(receiver_inbound_ID)
					message_text, _ := t.BuildInboundClientDataMessage(inbound.Remark, inbound.Protocol)
					t.addClient(message.Chat.ID, message_text)
				}
			case "awaiting_password_sh":
				if client_ShPassword == strings.TrimSpace(message.Text) {
					t.SendMsgToTgbotDeleteAfter(message.Chat.ID, t.I18nBot("tgbot.messages.using_default_value"), 3, tu.ReplyKeyboardRemove())
					delete(userStates, message.Chat.ID)
					return nil
				}

				client_ShPassword = strings.TrimSpace(message.Text)
				if t.isSingleWord(client_ShPassword) {
					userStates[message.Chat.ID] = "awaiting_password_sh"

					cancel_btn_markup := tu.InlineKeyboard(
						tu.InlineKeyboardRow(
							tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.use_default")).WithCallbackData("add_client_default_info"),
						),
					)

					t.SendMsgToTgbot(message.Chat.ID, t.I18nBot("tgbot.messages.incorrect_input"), cancel_btn_markup)
				} else {
					t.SendMsgToTgbotDeleteAfter(message.Chat.ID, t.I18nBot("tgbot.messages.received_password"), 3, tu.ReplyKeyboardRemove())
					delete(userStates, message.Chat.ID)
					inbound, _ := t.inboundService.GetInbound(receiver_inbound_ID)
					message_text, _ := t.BuildInboundClientDataMessage(inbound.Remark, inbound.Protocol)
					t.addClient(message.Chat.ID, message_text)
				}
			case "awaiting_email":
				if client_Email == strings.TrimSpace(message.Text) {
					t.SendMsgToTgbotDeleteAfter(message.Chat.ID, t.I18nBot("tgbot.messages.using_default_value"), 3, tu.ReplyKeyboardRemove())
					delete(userStates, message.Chat.ID)
					return nil
				}

				client_Email = strings.TrimSpace(message.Text)
				if t.isSingleWord(client_Email) {
					userStates[message.Chat.ID] = "awaiting_email"

					cancel_btn_markup := tu.InlineKeyboard(
						tu.InlineKeyboardRow(
							tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.use_default")).WithCallbackData("add_client_default_info"),
						),
					)

					t.SendMsgToTgbot(message.Chat.ID, t.I18nBot("tgbot.messages.incorrect_input"), cancel_btn_markup)
				} else {
					t.SendMsgToTgbotDeleteAfter(message.Chat.ID, t.I18nBot("tgbot.messages.received_email"), 3, tu.ReplyKeyboardRemove())
					delete(userStates, message.Chat.ID)
					inbound, _ := t.inboundService.GetInbound(receiver_inbound_ID)
					message_text, _ := t.BuildInboundClientDataMessage(inbound.Remark, inbound.Protocol)
					t.addClient(message.Chat.ID, message_text)
				}
			case "awaiting_comment":
				if client_Comment == strings.TrimSpace(message.Text) {
					t.SendMsgToTgbotDeleteAfter(message.Chat.ID, t.I18nBot("tgbot.messages.using_default_value"), 3, tu.ReplyKeyboardRemove())
					delete(userStates, message.Chat.ID)
					return nil
				}

				client_Comment = strings.TrimSpace(message.Text)
				t.SendMsgToTgbotDeleteAfter(message.Chat.ID, t.I18nBot("tgbot.messages.received_comment"), 3, tu.ReplyKeyboardRemove())
				delete(userStates, message.Chat.ID)
				inbound, _ := t.inboundService.GetInbound(receiver_inbound_ID)
				message_text, _ := t.BuildInboundClientDataMessage(inbound.Remark, inbound.Protocol)
				t.addClient(message.Chat.ID, message_text)
			}

		} else {
			if message.UsersShared != nil {
				if checkAdmin(message.From.ID) {
					for _, sharedUser := range message.UsersShared.Users {
						userID := sharedUser.UserID
						needRestart, err := t.inboundService.SetClientTelegramUserID(message.UsersShared.RequestID, userID)
						if needRestart {
							t.xrayService.SetToNeedRestart()
						}
						output := ""
						if err != nil {
							output += t.I18nBot("tgbot.messages.selectUserFailed")
						} else {
							output += t.I18nBot("tgbot.messages.userSaved")
						}
						t.SendMsgToTgbot(message.Chat.ID, output, tu.ReplyKeyboardRemove())
					}
				} else {
					t.SendMsgToTgbot(message.Chat.ID, t.I18nBot("tgbot.noResult"), tu.ReplyKeyboardRemove())
				}
			}
		}
		return nil
	}, th.AnyMessage())

	botHandler.Start()
}

func (t *Tgbot) answerCommand(message *telego.Message, chatId int64, isAdmin bool) {
	msg, onlyMessage := "", false

	command, _, commandArgs := tu.ParseCommand(message.Text)

	// Helper function to handle unknown commands.
	handleUnknownCommand := func() {
		msg += t.I18nBot("tgbot.commands.unknown")
	}

	// Handle the command.
	switch command {
	case "help":
		msg += t.I18nBot("tgbot.commands.help")
		msg += t.I18nBot("tgbot.commands.pleaseChoose")
	case "start":
		msg += t.I18nBot("tgbot.commands.start", "Firstname=="+message.From.FirstName)
		if isAdmin {
			msg += t.I18nBot("tgbot.commands.welcome", "Hostname=="+hostname)
		}
		msg += "\n\n" + t.I18nBot("tgbot.commands.pleaseChoose")
	case "status":
		onlyMessage = true
		msg += t.I18nBot("tgbot.commands.status")
	case "id":
		onlyMessage = true
		msg += t.I18nBot("tgbot.commands.getID", "ID=="+strconv.FormatInt(message.From.ID, 10))
	case "usage":
		onlyMessage = true
		if len(commandArgs) > 0 {
			if isAdmin {
				t.searchClient(chatId, commandArgs[0])
			} else {
				t.getClientUsage(chatId, int64(message.From.ID), commandArgs[0])
			}
		} else {
			msg += t.I18nBot("tgbot.commands.usage")
		}
	case "inbound":
		onlyMessage = true
		if isAdmin && len(commandArgs) > 0 {
			t.searchInbound(chatId, commandArgs[0])
		} else {
			handleUnknownCommand()
		}
	case "restart":
		onlyMessage = true
		if isAdmin {
			if len(commandArgs) == 0 {
				if t.xrayService.IsXrayRunning() {
					err := t.xrayService.RestartXray(true)
					if err != nil {
						msg += t.I18nBot("tgbot.commands.restartFailed", "Error=="+err.Error())
					} else {
						msg += t.I18nBot("tgbot.commands.restartSuccess")
					}
				} else {
					msg += t.I18nBot("tgbot.commands.xrayNotRunning")
				}
			} else {
				handleUnknownCommand()
				msg += t.I18nBot("tgbot.commands.restartUsage")
			}
		} else {
			handleUnknownCommand()
		}
	// 【新增代码】: 处理 /oneclick 指令
	case "oneclick":
		onlyMessage = true
		if isAdmin {
			t.sendOneClickOptions(chatId)
		} else {
			handleUnknownCommand()
		}

	// 【新增代码】: 处理 /subconverter 指令
	case "subconverter":
		onlyMessage = true
		if isAdmin {
			t.checkAndInstallSubconverter(chatId)
		} else {
			handleUnknownCommand()
		}	
	default:
		handleUnknownCommand()
	}

	if msg != "" {
		t.sendResponse(chatId, msg, onlyMessage, isAdmin)
	}
}

// Helper function to send the message based on onlyMessage flag.
func (t *Tgbot) sendResponse(chatId int64, msg string, onlyMessage, isAdmin bool) {
	if onlyMessage {
		t.SendMsgToTgbot(chatId, msg)
	} else {
		t.SendAnswer(chatId, msg, isAdmin)
	}
}

func (t *Tgbot) randomLowerAndNum(length int) string {
	charset := "abcdefghijklmnopqrstuvwxyz0123456789"
	bytes := make([]byte, length)
	for i := range bytes {
		randomIndex, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		bytes[i] = charset[randomIndex.Int64()]
	}
	return string(bytes)
}

func (t *Tgbot) randomShadowSocksPassword() string {
	array := make([]byte, 32)
	_, err := rand.Read(array)
	if err != nil {
		return t.randomLowerAndNum(32)
	}
	return base64.StdEncoding.EncodeToString(array)
}

func (t *Tgbot) answerCallback(callbackQuery *telego.CallbackQuery, isAdmin bool) {
	chatId := callbackQuery.Message.GetChat().ID

	if isAdmin {
		// get query from hash storage
		decodedQuery, err := t.decodeQuery(callbackQuery.Data)
		if err != nil {
			t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.noQuery"))
			return
		}
		dataArray := strings.Split(decodedQuery, " ")

		if len(dataArray) >= 2 && len(dataArray[1]) > 0 {
			email := dataArray[1]
			switch dataArray[0] {
			case "client_get_usage":
				t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.messages.email", "Email=="+email))
				t.searchClient(chatId, email)
			case "client_refresh":
				t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.clientRefreshSuccess", "Email=="+email))
				t.searchClient(chatId, email, callbackQuery.Message.GetMessageID())
			case "client_cancel":
				t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.canceled", "Email=="+email))
				t.searchClient(chatId, email, callbackQuery.Message.GetMessageID())
			case "ips_refresh":
				t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.IpRefreshSuccess", "Email=="+email))
				t.searchClientIps(chatId, email, callbackQuery.Message.GetMessageID())
			case "ips_cancel":
				t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.canceled", "Email=="+email))
				t.searchClientIps(chatId, email, callbackQuery.Message.GetMessageID())
			case "tgid_refresh":
				t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.TGIdRefreshSuccess", "Email=="+email))
				t.clientTelegramUserInfo(chatId, email, callbackQuery.Message.GetMessageID())
			case "tgid_cancel":
				t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.canceled", "Email=="+email))
				t.clientTelegramUserInfo(chatId, email, callbackQuery.Message.GetMessageID())
			case "reset_traffic":
				inlineKeyboard := tu.InlineKeyboard(
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancelReset")).WithCallbackData(t.encodeQuery("client_cancel "+email)),
					),
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.confirmResetTraffic")).WithCallbackData(t.encodeQuery("reset_traffic_c "+email)),
					),
				)
				t.editMessageCallbackTgBot(chatId, callbackQuery.Message.GetMessageID(), inlineKeyboard)
			case "reset_traffic_c":
				err := t.inboundService.ResetClientTrafficByEmail(email)
				if err == nil {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.resetTrafficSuccess", "Email=="+email))
					t.searchClient(chatId, email, callbackQuery.Message.GetMessageID())
				} else {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.errorOperation"))
				}
			case "limit_traffic":
				inlineKeyboard := tu.InlineKeyboard(
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancel")).WithCallbackData(t.encodeQuery("client_cancel "+email)),
					),
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(t.I18nBot("tgbot.unlimited")).WithCallbackData(t.encodeQuery("limit_traffic_c "+email+" 0")),
						tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.custom")).WithCallbackData(t.encodeQuery("limit_traffic_in "+email+" 0")),
					),
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton("1 GB").WithCallbackData(t.encodeQuery("limit_traffic_c "+email+" 1")),
						tu.InlineKeyboardButton("5 GB").WithCallbackData(t.encodeQuery("limit_traffic_c "+email+" 5")),
						tu.InlineKeyboardButton("10 GB").WithCallbackData(t.encodeQuery("limit_traffic_c "+email+" 10")),
					),
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton("20 GB").WithCallbackData(t.encodeQuery("limit_traffic_c "+email+" 20")),
						tu.InlineKeyboardButton("30 GB").WithCallbackData(t.encodeQuery("limit_traffic_c "+email+" 30")),
						tu.InlineKeyboardButton("40 GB").WithCallbackData(t.encodeQuery("limit_traffic_c "+email+" 40")),
					),
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton("50 GB").WithCallbackData(t.encodeQuery("limit_traffic_c "+email+" 50")),
						tu.InlineKeyboardButton("60 GB").WithCallbackData(t.encodeQuery("limit_traffic_c "+email+" 60")),
						tu.InlineKeyboardButton("80 GB").WithCallbackData(t.encodeQuery("limit_traffic_c "+email+" 80")),
					),
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton("100 GB").WithCallbackData(t.encodeQuery("limit_traffic_c "+email+" 100")),
						tu.InlineKeyboardButton("150 GB").WithCallbackData(t.encodeQuery("limit_traffic_c "+email+" 150")),
						tu.InlineKeyboardButton("200 GB").WithCallbackData(t.encodeQuery("limit_traffic_c "+email+" 200")),
					),
				)
				t.editMessageCallbackTgBot(chatId, callbackQuery.Message.GetMessageID(), inlineKeyboard)
			case "limit_traffic_c":
				if len(dataArray) == 3 {
					limitTraffic, err := strconv.Atoi(dataArray[2])
					if err == nil {
						needRestart, err := t.inboundService.ResetClientTrafficLimitByEmail(email, limitTraffic)
						if needRestart {
							t.xrayService.SetToNeedRestart()
						}
						if err == nil {
							t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.setTrafficLimitSuccess", "Email=="+email))
							t.searchClient(chatId, email, callbackQuery.Message.GetMessageID())
							return
						}
					}
				}
				t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.errorOperation"))
				t.searchClient(chatId, email, callbackQuery.Message.GetMessageID())
			case "limit_traffic_in":
				if len(dataArray) >= 3 {
					oldInputNumber, err := strconv.Atoi(dataArray[2])
					inputNumber := oldInputNumber
					if err == nil {
						if len(dataArray) == 4 {
							num, err := strconv.Atoi(dataArray[3])
							if err == nil {
								switch num {
								case -2:
									inputNumber = 0
								case -1:
									if inputNumber > 0 {
										inputNumber = (inputNumber / 10)
									}
								default:
									inputNumber = (inputNumber * 10) + num
								}
							}
							if inputNumber == oldInputNumber {
								t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.successfulOperation"))
								return
							}
							if inputNumber >= 999999 {
								t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.errorOperation"))
								return
							}
						}
						inlineKeyboard := tu.InlineKeyboard(
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancel")).WithCallbackData(t.encodeQuery("client_cancel "+email)),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.confirmNumberAdd", "Num=="+strconv.Itoa(inputNumber))).WithCallbackData(t.encodeQuery("limit_traffic_c "+email+" "+strconv.Itoa(inputNumber))),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("1").WithCallbackData(t.encodeQuery("limit_traffic_in "+email+" "+strconv.Itoa(inputNumber)+" 1")),
								tu.InlineKeyboardButton("2").WithCallbackData(t.encodeQuery("limit_traffic_in "+email+" "+strconv.Itoa(inputNumber)+" 2")),
								tu.InlineKeyboardButton("3").WithCallbackData(t.encodeQuery("limit_traffic_in "+email+" "+strconv.Itoa(inputNumber)+" 3")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("4").WithCallbackData(t.encodeQuery("limit_traffic_in "+email+" "+strconv.Itoa(inputNumber)+" 4")),
								tu.InlineKeyboardButton("5").WithCallbackData(t.encodeQuery("limit_traffic_in "+email+" "+strconv.Itoa(inputNumber)+" 5")),
								tu.InlineKeyboardButton("6").WithCallbackData(t.encodeQuery("limit_traffic_in "+email+" "+strconv.Itoa(inputNumber)+" 6")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("7").WithCallbackData(t.encodeQuery("limit_traffic_in "+email+" "+strconv.Itoa(inputNumber)+" 7")),
								tu.InlineKeyboardButton("8").WithCallbackData(t.encodeQuery("limit_traffic_in "+email+" "+strconv.Itoa(inputNumber)+" 8")),
								tu.InlineKeyboardButton("9").WithCallbackData(t.encodeQuery("limit_traffic_in "+email+" "+strconv.Itoa(inputNumber)+" 9")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("🔄").WithCallbackData(t.encodeQuery("limit_traffic_in "+email+" "+strconv.Itoa(inputNumber)+" -2")),
								tu.InlineKeyboardButton("0").WithCallbackData(t.encodeQuery("limit_traffic_in "+email+" "+strconv.Itoa(inputNumber)+" 0")),
								tu.InlineKeyboardButton("⬅️").WithCallbackData(t.encodeQuery("limit_traffic_in "+email+" "+strconv.Itoa(inputNumber)+" -1")),
							),
						)
						t.editMessageCallbackTgBot(chatId, callbackQuery.Message.GetMessageID(), inlineKeyboard)
						return
					}
				}
				t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.errorOperation"))
				t.searchClient(chatId, email, callbackQuery.Message.GetMessageID())
			case "add_client_limit_traffic_c":
				limitTraffic, _ := strconv.Atoi(dataArray[1])
				client_TotalGB = int64(limitTraffic) * 1024 * 1024 * 1024
				messageId := callbackQuery.Message.GetMessageID()
				inbound, err := t.inboundService.GetInbound(receiver_inbound_ID)
				if err != nil {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, err.Error())
					return
				}
				message_text, err := t.BuildInboundClientDataMessage(inbound.Remark, inbound.Protocol)
				if err != nil {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, err.Error())
					return
				}

				t.addClient(callbackQuery.Message.GetChat().ID, message_text, messageId)
				t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.successfulOperation"))
			case "add_client_limit_traffic_in":
				if len(dataArray) >= 2 {
					oldInputNumber, err := strconv.Atoi(dataArray[1])
					inputNumber := oldInputNumber
					if err == nil {
						if len(dataArray) == 3 {
							num, err := strconv.Atoi(dataArray[2])
							if err == nil {
								switch num {
								case -2:
									inputNumber = 0
								case -1:
									if inputNumber > 0 {
										inputNumber = (inputNumber / 10)
									}
								default:
									inputNumber = (inputNumber * 10) + num
								}
							}
							if inputNumber == oldInputNumber {
								t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.successfulOperation"))
								return
							}
							if inputNumber >= 999999 {
								t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.errorOperation"))
								return
							}
						}
						inlineKeyboard := tu.InlineKeyboard(
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancel")).WithCallbackData(t.encodeQuery("add_client_default_traffic_exp")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.confirmNumberAdd", "Num=="+strconv.Itoa(inputNumber))).WithCallbackData(t.encodeQuery("add_client_limit_traffic_c "+strconv.Itoa(inputNumber))),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("1").WithCallbackData(t.encodeQuery("add_client_limit_traffic_in "+strconv.Itoa(inputNumber)+" 1")),
								tu.InlineKeyboardButton("2").WithCallbackData(t.encodeQuery("add_client_limit_traffic_in "+strconv.Itoa(inputNumber)+" 2")),
								tu.InlineKeyboardButton("3").WithCallbackData(t.encodeQuery("add_client_limit_traffic_in "+strconv.Itoa(inputNumber)+" 3")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("4").WithCallbackData(t.encodeQuery("add_client_limit_traffic_in "+strconv.Itoa(inputNumber)+" 4")),
								tu.InlineKeyboardButton("5").WithCallbackData(t.encodeQuery("add_client_limit_traffic_in "+strconv.Itoa(inputNumber)+" 5")),
								tu.InlineKeyboardButton("6").WithCallbackData(t.encodeQuery("add_client_limit_traffic_in "+strconv.Itoa(inputNumber)+" 6")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("7").WithCallbackData(t.encodeQuery("add_client_limit_traffic_in "+strconv.Itoa(inputNumber)+" 7")),
								tu.InlineKeyboardButton("8").WithCallbackData(t.encodeQuery("add_client_limit_traffic_in "+strconv.Itoa(inputNumber)+" 8")),
								tu.InlineKeyboardButton("9").WithCallbackData(t.encodeQuery("add_client_limit_traffic_in "+strconv.Itoa(inputNumber)+" 9")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("🔄").WithCallbackData(t.encodeQuery("add_client_limit_traffic_in "+strconv.Itoa(inputNumber)+" -2")),
								tu.InlineKeyboardButton("0").WithCallbackData(t.encodeQuery("add_client_limit_traffic_in "+strconv.Itoa(inputNumber)+" 0")),
								tu.InlineKeyboardButton("⬅️").WithCallbackData(t.encodeQuery("add_client_limit_traffic_in "+strconv.Itoa(inputNumber)+" -1")),
							),
						)
						t.editMessageCallbackTgBot(chatId, callbackQuery.Message.GetMessageID(), inlineKeyboard)
						return
					}
				}
			case "reset_exp":
				inlineKeyboard := tu.InlineKeyboard(
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancelReset")).WithCallbackData(t.encodeQuery("client_cancel "+email)),
					),
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(t.I18nBot("tgbot.unlimited")).WithCallbackData(t.encodeQuery("reset_exp_c "+email+" 0")),
						tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.custom")).WithCallbackData(t.encodeQuery("reset_exp_in "+email+" 0")),
					),
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(t.I18nBot("tgbot.add")+" 7 "+t.I18nBot("tgbot.days")).WithCallbackData(t.encodeQuery("reset_exp_c "+email+" 7")),
						tu.InlineKeyboardButton(t.I18nBot("tgbot.add")+" 10 "+t.I18nBot("tgbot.days")).WithCallbackData(t.encodeQuery("reset_exp_c "+email+" 10")),
					),
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(t.I18nBot("tgbot.add")+" 14 "+t.I18nBot("tgbot.days")).WithCallbackData(t.encodeQuery("reset_exp_c "+email+" 14")),
						tu.InlineKeyboardButton(t.I18nBot("tgbot.add")+" 20 "+t.I18nBot("tgbot.days")).WithCallbackData(t.encodeQuery("reset_exp_c "+email+" 20")),
					),
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(t.I18nBot("tgbot.add")+" 1 "+t.I18nBot("tgbot.month")).WithCallbackData(t.encodeQuery("reset_exp_c "+email+" 30")),
						tu.InlineKeyboardButton(t.I18nBot("tgbot.add")+" 3 "+t.I18nBot("tgbot.months")).WithCallbackData(t.encodeQuery("reset_exp_c "+email+" 90")),
					),
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(t.I18nBot("tgbot.add")+" 6 "+t.I18nBot("tgbot.months")).WithCallbackData(t.encodeQuery("reset_exp_c "+email+" 180")),
						tu.InlineKeyboardButton(t.I18nBot("tgbot.add")+" 12 "+t.I18nBot("tgbot.months")).WithCallbackData(t.encodeQuery("reset_exp_c "+email+" 365")),
					),
				)
				t.editMessageCallbackTgBot(chatId, callbackQuery.Message.GetMessageID(), inlineKeyboard)
			case "reset_exp_c":
				if len(dataArray) == 3 {
					days, err := strconv.Atoi(dataArray[2])
					if err == nil {
						var date int64 = 0
						if days > 0 {
							traffic, err := t.inboundService.GetClientTrafficByEmail(email)
							if err != nil {
								logger.Warning(err)
								msg := t.I18nBot("tgbot.wentWrong")
								t.SendMsgToTgbot(chatId, msg)
								return
							}
							if traffic == nil {
								msg := t.I18nBot("tgbot.noResult")
								t.SendMsgToTgbot(chatId, msg)
								return
							}

							if traffic.ExpiryTime > 0 {
								if traffic.ExpiryTime-time.Now().Unix()*1000 < 0 {
									date = -int64(days * 24 * 60 * 60000)
								} else {
									date = traffic.ExpiryTime + int64(days*24*60*60000)
								}
							} else {
								date = traffic.ExpiryTime - int64(days*24*60*60000)
							}

						}
						needRestart, err := t.inboundService.ResetClientExpiryTimeByEmail(email, date)
						if needRestart {
							t.xrayService.SetToNeedRestart()
						}
						if err == nil {
							t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.expireResetSuccess", "Email=="+email))
							t.searchClient(chatId, email, callbackQuery.Message.GetMessageID())
							return
						}
					}
				}
				t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.errorOperation"))
				t.searchClient(chatId, email, callbackQuery.Message.GetMessageID())
			case "reset_exp_in":
				if len(dataArray) >= 3 {
					oldInputNumber, err := strconv.Atoi(dataArray[2])
					inputNumber := oldInputNumber
					if err == nil {
						if len(dataArray) == 4 {
							num, err := strconv.Atoi(dataArray[3])
							if err == nil {
								switch num {
								case -2:
									inputNumber = 0
								case -1:
									if inputNumber > 0 {
										inputNumber = (inputNumber / 10)
									}
								default:
									inputNumber = (inputNumber * 10) + num
								}
							}
							if inputNumber == oldInputNumber {
								t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.successfulOperation"))
								return
							}
							if inputNumber >= 999999 {
								t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.errorOperation"))
								return
							}
						}
						inlineKeyboard := tu.InlineKeyboard(
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancel")).WithCallbackData(t.encodeQuery("client_cancel "+email)),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.confirmNumber", "Num=="+strconv.Itoa(inputNumber))).WithCallbackData(t.encodeQuery("reset_exp_c "+email+" "+strconv.Itoa(inputNumber))),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("1").WithCallbackData(t.encodeQuery("reset_exp_in "+email+" "+strconv.Itoa(inputNumber)+" 1")),
								tu.InlineKeyboardButton("2").WithCallbackData(t.encodeQuery("reset_exp_in "+email+" "+strconv.Itoa(inputNumber)+" 2")),
								tu.InlineKeyboardButton("3").WithCallbackData(t.encodeQuery("reset_exp_in "+email+" "+strconv.Itoa(inputNumber)+" 3")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("4").WithCallbackData(t.encodeQuery("reset_exp_in "+email+" "+strconv.Itoa(inputNumber)+" 4")),
								tu.InlineKeyboardButton("5").WithCallbackData(t.encodeQuery("reset_exp_in "+email+" "+strconv.Itoa(inputNumber)+" 5")),
								tu.InlineKeyboardButton("6").WithCallbackData(t.encodeQuery("reset_exp_in "+email+" "+strconv.Itoa(inputNumber)+" 6")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("7").WithCallbackData(t.encodeQuery("reset_exp_in "+email+" "+strconv.Itoa(inputNumber)+" 7")),
								tu.InlineKeyboardButton("8").WithCallbackData(t.encodeQuery("reset_exp_in "+email+" "+strconv.Itoa(inputNumber)+" 8")),
								tu.InlineKeyboardButton("9").WithCallbackData(t.encodeQuery("reset_exp_in "+email+" "+strconv.Itoa(inputNumber)+" 9")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("🔄").WithCallbackData(t.encodeQuery("reset_exp_in "+email+" "+strconv.Itoa(inputNumber)+" -2")),
								tu.InlineKeyboardButton("0").WithCallbackData(t.encodeQuery("reset_exp_in "+email+" "+strconv.Itoa(inputNumber)+" 0")),
								tu.InlineKeyboardButton("⬅️").WithCallbackData(t.encodeQuery("reset_exp_in "+email+" "+strconv.Itoa(inputNumber)+" -1")),
							),
						)
						t.editMessageCallbackTgBot(chatId, callbackQuery.Message.GetMessageID(), inlineKeyboard)
						return
					}
				}
				t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.errorOperation"))
				t.searchClient(chatId, email, callbackQuery.Message.GetMessageID())
			case "add_client_reset_exp_c":
				client_ExpiryTime = 0
				days, _ := strconv.Atoi(dataArray[1])
				var date int64 = 0
				if client_ExpiryTime > 0 {
					if client_ExpiryTime-time.Now().Unix()*1000 < 0 {
						date = -int64(days * 24 * 60 * 60000)
					} else {
						date = client_ExpiryTime + int64(days*24*60*60000)
					}
				} else {
					date = client_ExpiryTime - int64(days*24*60*60000)
				}
				client_ExpiryTime = date

				messageId := callbackQuery.Message.GetMessageID()
				inbound, err := t.inboundService.GetInbound(receiver_inbound_ID)
				if err != nil {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, err.Error())
					return
				}
				message_text, err := t.BuildInboundClientDataMessage(inbound.Remark, inbound.Protocol)
				if err != nil {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, err.Error())
					return
				}

				t.addClient(callbackQuery.Message.GetChat().ID, message_text, messageId)
				t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.successfulOperation"))
			case "add_client_reset_exp_in":
				if len(dataArray) >= 2 {
					oldInputNumber, err := strconv.Atoi(dataArray[1])
					inputNumber := oldInputNumber
					if err == nil {
						if len(dataArray) == 3 {
							num, err := strconv.Atoi(dataArray[2])
							if err == nil {
								switch num {
								case -2:
									inputNumber = 0
								case -1:
									if inputNumber > 0 {
										inputNumber = (inputNumber / 10)
									}
								default:
									inputNumber = (inputNumber * 10) + num
								}
							}
							if inputNumber == oldInputNumber {
								t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.successfulOperation"))
								return
							}
							if inputNumber >= 999999 {
								t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.errorOperation"))
								return
							}
						}
						inlineKeyboard := tu.InlineKeyboard(
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancel")).WithCallbackData(t.encodeQuery("add_client_default_traffic_exp")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.confirmNumberAdd", "Num=="+strconv.Itoa(inputNumber))).WithCallbackData(t.encodeQuery("add_client_reset_exp_c "+strconv.Itoa(inputNumber))),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("1").WithCallbackData(t.encodeQuery("add_client_reset_exp_in "+strconv.Itoa(inputNumber)+" 1")),
								tu.InlineKeyboardButton("2").WithCallbackData(t.encodeQuery("add_client_reset_exp_in "+strconv.Itoa(inputNumber)+" 2")),
								tu.InlineKeyboardButton("3").WithCallbackData(t.encodeQuery("add_client_reset_exp_in "+strconv.Itoa(inputNumber)+" 3")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("4").WithCallbackData(t.encodeQuery("add_client_reset_exp_in "+strconv.Itoa(inputNumber)+" 4")),
								tu.InlineKeyboardButton("5").WithCallbackData(t.encodeQuery("add_client_reset_exp_in "+strconv.Itoa(inputNumber)+" 5")),
								tu.InlineKeyboardButton("6").WithCallbackData(t.encodeQuery("add_client_reset_exp_in "+strconv.Itoa(inputNumber)+" 6")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("7").WithCallbackData(t.encodeQuery("add_client_reset_exp_in "+strconv.Itoa(inputNumber)+" 7")),
								tu.InlineKeyboardButton("8").WithCallbackData(t.encodeQuery("add_client_reset_exp_in "+strconv.Itoa(inputNumber)+" 8")),
								tu.InlineKeyboardButton("9").WithCallbackData(t.encodeQuery("add_client_reset_exp_in "+strconv.Itoa(inputNumber)+" 9")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("🔄").WithCallbackData(t.encodeQuery("add_client_reset_exp_in "+strconv.Itoa(inputNumber)+" -2")),
								tu.InlineKeyboardButton("0").WithCallbackData(t.encodeQuery("add_client_reset_exp_in "+strconv.Itoa(inputNumber)+" 0")),
								tu.InlineKeyboardButton("⬅️").WithCallbackData(t.encodeQuery("add_client_reset_exp_in "+strconv.Itoa(inputNumber)+" -1")),
							),
						)
						t.editMessageCallbackTgBot(chatId, callbackQuery.Message.GetMessageID(), inlineKeyboard)
						return
					}
				}
			case "ip_limit":
				inlineKeyboard := tu.InlineKeyboard(
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancelIpLimit")).WithCallbackData(t.encodeQuery("client_cancel "+email)),
					),
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(t.I18nBot("tgbot.unlimited")).WithCallbackData(t.encodeQuery("ip_limit_c "+email+" 0")),
						tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.custom")).WithCallbackData(t.encodeQuery("ip_limit_in "+email+" 0")),
					),
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton("1").WithCallbackData(t.encodeQuery("ip_limit_c "+email+" 1")),
						tu.InlineKeyboardButton("2").WithCallbackData(t.encodeQuery("ip_limit_c "+email+" 2")),
					),
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton("3").WithCallbackData(t.encodeQuery("ip_limit_c "+email+" 3")),
						tu.InlineKeyboardButton("4").WithCallbackData(t.encodeQuery("ip_limit_c "+email+" 4")),
					),
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton("5").WithCallbackData(t.encodeQuery("ip_limit_c "+email+" 5")),
						tu.InlineKeyboardButton("6").WithCallbackData(t.encodeQuery("ip_limit_c "+email+" 6")),
						tu.InlineKeyboardButton("7").WithCallbackData(t.encodeQuery("ip_limit_c "+email+" 7")),
					),
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton("8").WithCallbackData(t.encodeQuery("ip_limit_c "+email+" 8")),
						tu.InlineKeyboardButton("9").WithCallbackData(t.encodeQuery("ip_limit_c "+email+" 9")),
						tu.InlineKeyboardButton("10").WithCallbackData(t.encodeQuery("ip_limit_c "+email+" 10")),
					),
				)
				t.editMessageCallbackTgBot(chatId, callbackQuery.Message.GetMessageID(), inlineKeyboard)
			case "ip_limit_c":
				if len(dataArray) == 3 {
					count, err := strconv.Atoi(dataArray[2])
					if err == nil {
						needRestart, err := t.inboundService.ResetClientIpLimitByEmail(email, count)
						if needRestart {
							t.xrayService.SetToNeedRestart()
						}
						if err == nil {
							t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.resetIpSuccess", "Email=="+email, "Count=="+strconv.Itoa(count)))
							t.searchClient(chatId, email, callbackQuery.Message.GetMessageID())
							return
						}
					}
				}
				t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.errorOperation"))
				t.searchClient(chatId, email, callbackQuery.Message.GetMessageID())
			case "ip_limit_in":
				if len(dataArray) >= 3 {
					oldInputNumber, err := strconv.Atoi(dataArray[2])
					inputNumber := oldInputNumber
					if err == nil {
						if len(dataArray) == 4 {
							num, err := strconv.Atoi(dataArray[3])
							if err == nil {
								switch num {
								case -2:
									inputNumber = 0
								case -1:
									if inputNumber > 0 {
										inputNumber = (inputNumber / 10)
									}
								default:
									inputNumber = (inputNumber * 10) + num
								}
							}
							if inputNumber == oldInputNumber {
								t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.successfulOperation"))
								return
							}
							if inputNumber >= 999999 {
								t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.errorOperation"))
								return
							}
						}
						inlineKeyboard := tu.InlineKeyboard(
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancel")).WithCallbackData(t.encodeQuery("client_cancel "+email)),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.confirmNumber", "Num=="+strconv.Itoa(inputNumber))).WithCallbackData(t.encodeQuery("ip_limit_c "+email+" "+strconv.Itoa(inputNumber))),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("1").WithCallbackData(t.encodeQuery("ip_limit_in "+email+" "+strconv.Itoa(inputNumber)+" 1")),
								tu.InlineKeyboardButton("2").WithCallbackData(t.encodeQuery("ip_limit_in "+email+" "+strconv.Itoa(inputNumber)+" 2")),
								tu.InlineKeyboardButton("3").WithCallbackData(t.encodeQuery("ip_limit_in "+email+" "+strconv.Itoa(inputNumber)+" 3")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("4").WithCallbackData(t.encodeQuery("ip_limit_in "+email+" "+strconv.Itoa(inputNumber)+" 4")),
								tu.InlineKeyboardButton("5").WithCallbackData(t.encodeQuery("ip_limit_in "+email+" "+strconv.Itoa(inputNumber)+" 5")),
								tu.InlineKeyboardButton("6").WithCallbackData(t.encodeQuery("ip_limit_in "+email+" "+strconv.Itoa(inputNumber)+" 6")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("7").WithCallbackData(t.encodeQuery("ip_limit_in "+email+" "+strconv.Itoa(inputNumber)+" 7")),
								tu.InlineKeyboardButton("8").WithCallbackData(t.encodeQuery("ip_limit_in "+email+" "+strconv.Itoa(inputNumber)+" 8")),
								tu.InlineKeyboardButton("9").WithCallbackData(t.encodeQuery("ip_limit_in "+email+" "+strconv.Itoa(inputNumber)+" 9")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("🔄").WithCallbackData(t.encodeQuery("ip_limit_in "+email+" "+strconv.Itoa(inputNumber)+" -2")),
								tu.InlineKeyboardButton("0").WithCallbackData(t.encodeQuery("ip_limit_in "+email+" "+strconv.Itoa(inputNumber)+" 0")),
								tu.InlineKeyboardButton("⬅️").WithCallbackData(t.encodeQuery("ip_limit_in "+email+" "+strconv.Itoa(inputNumber)+" -1")),
							),
						)
						t.editMessageCallbackTgBot(chatId, callbackQuery.Message.GetMessageID(), inlineKeyboard)
						return
					}
				}
				t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.errorOperation"))
				t.searchClient(chatId, email, callbackQuery.Message.GetMessageID())
			case "add_client_ip_limit_c":
				if len(dataArray) == 2 {
					count, _ := strconv.Atoi(dataArray[1])
					client_LimitIP = count
				}

				messageId := callbackQuery.Message.GetMessageID()
				inbound, err := t.inboundService.GetInbound(receiver_inbound_ID)
				if err != nil {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, err.Error())
					return
				}
				message_text, err := t.BuildInboundClientDataMessage(inbound.Remark, inbound.Protocol)
				if err != nil {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, err.Error())
					return
				}

				t.addClient(callbackQuery.Message.GetChat().ID, message_text, messageId)
				t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.successfulOperation"))
			case "add_client_ip_limit_in":
				if len(dataArray) >= 2 {
					oldInputNumber, err := strconv.Atoi(dataArray[1])
					inputNumber := oldInputNumber
					if err == nil {
						if len(dataArray) == 3 {
							num, err := strconv.Atoi(dataArray[2])
							if err == nil {
								switch num {
								case -2:
									inputNumber = 0
								case -1:
									if inputNumber > 0 {
										inputNumber = (inputNumber / 10)
									}
								default:
									inputNumber = (inputNumber * 10) + num
								}
							}
							if inputNumber == oldInputNumber {
								t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.successfulOperation"))
								return
							}
							if inputNumber >= 999999 {
								t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.errorOperation"))
								return
							}
						}
						inlineKeyboard := tu.InlineKeyboard(
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancel")).WithCallbackData(t.encodeQuery("add_client_default_ip_limit")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.confirmNumber", "Num=="+strconv.Itoa(inputNumber))).WithCallbackData(t.encodeQuery("add_client_ip_limit_c "+strconv.Itoa(inputNumber))),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("1").WithCallbackData(t.encodeQuery("add_client_ip_limit_in "+strconv.Itoa(inputNumber)+" 1")),
								tu.InlineKeyboardButton("2").WithCallbackData(t.encodeQuery("add_client_ip_limit_in "+strconv.Itoa(inputNumber)+" 2")),
								tu.InlineKeyboardButton("3").WithCallbackData(t.encodeQuery("add_client_ip_limit_in "+strconv.Itoa(inputNumber)+" 3")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("4").WithCallbackData(t.encodeQuery("add_client_ip_limit_in "+strconv.Itoa(inputNumber)+" 4")),
								tu.InlineKeyboardButton("5").WithCallbackData(t.encodeQuery("add_client_ip_limit_in "+strconv.Itoa(inputNumber)+" 5")),
								tu.InlineKeyboardButton("6").WithCallbackData(t.encodeQuery("add_client_ip_limit_in "+strconv.Itoa(inputNumber)+" 6")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("7").WithCallbackData(t.encodeQuery("add_client_ip_limit_in "+strconv.Itoa(inputNumber)+" 7")),
								tu.InlineKeyboardButton("8").WithCallbackData(t.encodeQuery("add_client_ip_limit_in "+strconv.Itoa(inputNumber)+" 8")),
								tu.InlineKeyboardButton("9").WithCallbackData(t.encodeQuery("add_client_ip_limit_in "+strconv.Itoa(inputNumber)+" 9")),
							),
							tu.InlineKeyboardRow(
								tu.InlineKeyboardButton("🔄").WithCallbackData(t.encodeQuery("add_client_ip_limit_in "+strconv.Itoa(inputNumber)+" -2")),
								tu.InlineKeyboardButton("0").WithCallbackData(t.encodeQuery("add_client_ip_limit_in "+strconv.Itoa(inputNumber)+" 0")),
								tu.InlineKeyboardButton("⬅️").WithCallbackData(t.encodeQuery("add_client_ip_limit_in "+strconv.Itoa(inputNumber)+" -1")),
							),
						)
						t.editMessageCallbackTgBot(chatId, callbackQuery.Message.GetMessageID(), inlineKeyboard)
						return
					}
				}
			case "clear_ips":
				inlineKeyboard := tu.InlineKeyboard(
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancel")).WithCallbackData(t.encodeQuery("ips_cancel "+email)),
					),
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.confirmClearIps")).WithCallbackData(t.encodeQuery("clear_ips_c "+email)),
					),
				)
				t.editMessageCallbackTgBot(chatId, callbackQuery.Message.GetMessageID(), inlineKeyboard)
			case "clear_ips_c":
				err := t.inboundService.ClearClientIps(email)
				if err == nil {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.clearIpSuccess", "Email=="+email))
					t.searchClientIps(chatId, email, callbackQuery.Message.GetMessageID())
				} else {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.errorOperation"))
				}
			case "ip_log":
				t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.getIpLog", "Email=="+email))
				t.searchClientIps(chatId, email)
			case "tg_user":
				t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.getUserInfo", "Email=="+email))
				t.clientTelegramUserInfo(chatId, email)
			case "tgid_remove":
				inlineKeyboard := tu.InlineKeyboard(
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancel")).WithCallbackData(t.encodeQuery("tgid_cancel "+email)),
					),
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.confirmRemoveTGUser")).WithCallbackData(t.encodeQuery("tgid_remove_c "+email)),
					),
				)
				t.editMessageCallbackTgBot(chatId, callbackQuery.Message.GetMessageID(), inlineKeyboard)
			case "tgid_remove_c":
				traffic, err := t.inboundService.GetClientTrafficByEmail(email)
				if err != nil || traffic == nil {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.errorOperation"))
					return
				}
				needRestart, err := t.inboundService.SetClientTelegramUserID(traffic.Id, EmptyTelegramUserID)
				if needRestart {
					t.xrayService.SetToNeedRestart()
				}
				if err == nil {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.removedTGUserSuccess", "Email=="+email))
					t.clientTelegramUserInfo(chatId, email, callbackQuery.Message.GetMessageID())
				} else {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.errorOperation"))
				}
			case "toggle_enable":
				inlineKeyboard := tu.InlineKeyboard(
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancel")).WithCallbackData(t.encodeQuery("client_cancel "+email)),
					),
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.confirmToggle")).WithCallbackData(t.encodeQuery("toggle_enable_c "+email)),
					),
				)
				t.editMessageCallbackTgBot(chatId, callbackQuery.Message.GetMessageID(), inlineKeyboard)
			case "toggle_enable_c":
				enabled, needRestart, err := t.inboundService.ToggleClientEnableByEmail(email)
				if needRestart {
					t.xrayService.SetToNeedRestart()
				}
				if err == nil {
					if enabled {
						t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.enableSuccess", "Email=="+email))
					} else {
						t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.disableSuccess", "Email=="+email))
					}
					t.searchClient(chatId, email, callbackQuery.Message.GetMessageID())
				} else {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.errorOperation"))
				}
			case "get_clients":
				inboundId := dataArray[1]
				inboundIdInt, err := strconv.Atoi(inboundId)
				if err != nil {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, err.Error())
					return
				}
				inbound, err := t.inboundService.GetInbound(inboundIdInt)
				if err != nil {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, err.Error())
					return
				}
				clients, err := t.getInboundClients(inboundIdInt)
				if err != nil {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, err.Error())
					return
				}
				t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.answers.chooseClient", "Inbound=="+inbound.Remark), clients)
			case "add_client_to":
				// assign default values to clients variables
				client_Id = uuid.New().String()
				client_Flow = ""
				client_Email = t.randomLowerAndNum(8)
				client_LimitIP = 0
				client_TotalGB = 0
				client_ExpiryTime = 0
				client_Enable = true
				client_TgID = ""
				client_SubID = t.randomLowerAndNum(16)
				client_Comment = ""
				client_Reset = 0
				client_Security = "auto"
				client_ShPassword = t.randomShadowSocksPassword()
				client_TrPassword = t.randomLowerAndNum(10)
				client_Method = ""

				inboundId := dataArray[1]
				inboundIdInt, err := strconv.Atoi(inboundId)
				if err != nil {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, err.Error())
					return
				}
				receiver_inbound_ID = inboundIdInt
				inbound, err := t.inboundService.GetInbound(inboundIdInt)
				if err != nil {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, err.Error())
					return
				}

				message_text, err := t.BuildInboundClientDataMessage(inbound.Remark, inbound.Protocol)
				if err != nil {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, err.Error())
					return
				}

				t.addClient(callbackQuery.Message.GetChat().ID, message_text)
			}
			return
		} else {
			switch callbackQuery.Data {
			case "get_inbounds":
				inbounds, err := t.getInbounds()
				if err != nil {
					t.sendCallbackAnswerTgBot(callbackQuery.ID, err.Error())
					return

				}
				t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.buttons.allClients"))
				t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.answers.chooseInbound"), inbounds)
			}

		}
	}

	switch callbackQuery.Data {
	case "get_usage":
		t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.buttons.serverUsage"))
		t.getServerUsage(chatId)
	case "usage_refresh":
		t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.successfulOperation"))
		t.getServerUsage(chatId, callbackQuery.Message.GetMessageID())
	case "inbounds":
		t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.buttons.getInbounds"))
		t.SendMsgToTgbot(chatId, t.getInboundUsages())
	case "deplete_soon":
		t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.buttons.depleteSoon"))
		t.getExhausted(chatId)
	case "get_backup":
		t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.buttons.dbBackup"))
		t.sendBackup(chatId)
	case "get_banlogs":
		t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.buttons.getBanLogs"))
		t.sendBanLogs(chatId, true)
	case "client_traffic":
		tgUserID := callbackQuery.From.ID
		t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.buttons.clientUsage"))
		t.getClientUsage(chatId, tgUserID)
	case "client_commands":
		t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.buttons.commands"))
		t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.commands.helpClientCommands"))
	case "onlines":
		t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.buttons.onlines"))
		t.onlineClients(chatId)
	case "onlines_refresh":
		t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.successfulOperation"))
		t.onlineClients(chatId, callbackQuery.Message.GetMessageID())
	case "commands":
		t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.buttons.commands"))
		t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.commands.helpAdminCommands"))
	case "add_client":
		// assign default values to clients variables
		client_Id = uuid.New().String()
		client_Flow = ""
		client_Email = t.randomLowerAndNum(8)
		client_LimitIP = 0
		client_TotalGB = 0
		client_ExpiryTime = 0
		client_Enable = true
		client_TgID = ""
		client_SubID = t.randomLowerAndNum(16)
		client_Comment = ""
		client_Reset = 0
		client_Security = "auto"
		client_ShPassword = t.randomShadowSocksPassword()
		client_TrPassword = t.randomLowerAndNum(10)
		client_Method = ""

		inbounds, err := t.getInboundsAddClient()
		if err != nil {
			t.sendCallbackAnswerTgBot(callbackQuery.ID, err.Error())
			return
		}
		t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.buttons.addClient"))
		t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.answers.chooseInbound"), inbounds)
	case "add_client_ch_default_email":
		t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
		userStates[chatId] = "awaiting_email"
		cancel_btn_markup := tu.InlineKeyboard(
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.use_default")).WithCallbackData("add_client_default_info"),
			),
		)
		prompt_message := t.I18nBot("tgbot.messages.email_prompt", "ClientEmail=="+client_Email)
		t.SendMsgToTgbot(chatId, prompt_message, cancel_btn_markup)
	case "add_client_ch_default_id":
		t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
		userStates[chatId] = "awaiting_id"
		cancel_btn_markup := tu.InlineKeyboard(
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.use_default")).WithCallbackData("add_client_default_info"),
			),
		)
		prompt_message := t.I18nBot("tgbot.messages.id_prompt", "ClientId=="+client_Id)
		t.SendMsgToTgbot(chatId, prompt_message, cancel_btn_markup)
	case "add_client_ch_default_pass_tr":
		t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
		userStates[chatId] = "awaiting_password_tr"
		cancel_btn_markup := tu.InlineKeyboard(
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.use_default")).WithCallbackData("add_client_default_info"),
			),
		)
		prompt_message := t.I18nBot("tgbot.messages.pass_prompt", "ClientPassword=="+client_TrPassword)
		t.SendMsgToTgbot(chatId, prompt_message, cancel_btn_markup)
	case "add_client_ch_default_pass_sh":
		t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
		userStates[chatId] = "awaiting_password_sh"
		cancel_btn_markup := tu.InlineKeyboard(
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.use_default")).WithCallbackData("add_client_default_info"),
			),
		)
		prompt_message := t.I18nBot("tgbot.messages.pass_prompt", "ClientPassword=="+client_ShPassword)
		t.SendMsgToTgbot(chatId, prompt_message, cancel_btn_markup)
	case "add_client_ch_default_comment":
		t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
		userStates[chatId] = "awaiting_comment"
		cancel_btn_markup := tu.InlineKeyboard(
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.use_default")).WithCallbackData("add_client_default_info"),
			),
		)
		prompt_message := t.I18nBot("tgbot.messages.comment_prompt", "ClientComment=="+client_Comment)
		t.SendMsgToTgbot(chatId, prompt_message, cancel_btn_markup)
	case "add_client_ch_default_traffic":
		inlineKeyboard := tu.InlineKeyboard(
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancel")).WithCallbackData(t.encodeQuery("add_client_default_traffic_exp")),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.unlimited")).WithCallbackData(t.encodeQuery("add_client_limit_traffic_c 0")),
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.custom")).WithCallbackData(t.encodeQuery("add_client_limit_traffic_in 0")),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton("1 GB").WithCallbackData(t.encodeQuery("add_client_limit_traffic_c 1")),
				tu.InlineKeyboardButton("5 GB").WithCallbackData(t.encodeQuery("add_client_limit_traffic_c 5")),
				tu.InlineKeyboardButton("10 GB").WithCallbackData(t.encodeQuery("add_client_limit_traffic_c 10")),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton("20 GB").WithCallbackData(t.encodeQuery("add_client_limit_traffic_c 20")),
				tu.InlineKeyboardButton("30 GB").WithCallbackData(t.encodeQuery("add_client_limit_traffic_c 30")),
				tu.InlineKeyboardButton("40 GB").WithCallbackData(t.encodeQuery("add_client_limit_traffic_c 40")),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton("50 GB").WithCallbackData(t.encodeQuery("add_client_limit_traffic_c 50")),
				tu.InlineKeyboardButton("60 GB").WithCallbackData(t.encodeQuery("add_client_limit_traffic_c 60")),
				tu.InlineKeyboardButton("80 GB").WithCallbackData(t.encodeQuery("add_client_limit_traffic_c 80")),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton("100 GB").WithCallbackData(t.encodeQuery("add_client_limit_traffic_c 100")),
				tu.InlineKeyboardButton("150 GB").WithCallbackData(t.encodeQuery("add_client_limit_traffic_c 150")),
				tu.InlineKeyboardButton("200 GB").WithCallbackData(t.encodeQuery("add_client_limit_traffic_c 200")),
			),
		)
		t.editMessageCallbackTgBot(chatId, callbackQuery.Message.GetMessageID(), inlineKeyboard)
	case "add_client_ch_default_exp":
		inlineKeyboard := tu.InlineKeyboard(
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancel")).WithCallbackData(t.encodeQuery("add_client_default_traffic_exp")),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.unlimited")).WithCallbackData(t.encodeQuery("add_client_reset_exp_c 0")),
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.custom")).WithCallbackData(t.encodeQuery("add_client_reset_exp_in 0")),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.add")+" 7 "+t.I18nBot("tgbot.days")).WithCallbackData(t.encodeQuery("add_client_reset_exp_c 7")),
				tu.InlineKeyboardButton(t.I18nBot("tgbot.add")+" 10 "+t.I18nBot("tgbot.days")).WithCallbackData(t.encodeQuery("add_client_reset_exp_c 10")),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.add")+" 14 "+t.I18nBot("tgbot.days")).WithCallbackData(t.encodeQuery("add_client_reset_exp_c 14")),
				tu.InlineKeyboardButton(t.I18nBot("tgbot.add")+" 20 "+t.I18nBot("tgbot.days")).WithCallbackData(t.encodeQuery("add_client_reset_exp_c 20")),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.add")+" 1 "+t.I18nBot("tgbot.month")).WithCallbackData(t.encodeQuery("add_client_reset_exp_c 30")),
				tu.InlineKeyboardButton(t.I18nBot("tgbot.add")+" 3 "+t.I18nBot("tgbot.months")).WithCallbackData(t.encodeQuery("add_client_reset_exp_c 90")),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.add")+" 6 "+t.I18nBot("tgbot.months")).WithCallbackData(t.encodeQuery("add_client_reset_exp_c 180")),
				tu.InlineKeyboardButton(t.I18nBot("tgbot.add")+" 12 "+t.I18nBot("tgbot.months")).WithCallbackData(t.encodeQuery("add_client_reset_exp_c 365")),
			),
		)
		t.editMessageCallbackTgBot(chatId, callbackQuery.Message.GetMessageID(), inlineKeyboard)
	case "add_client_ch_default_ip_limit":
		inlineKeyboard := tu.InlineKeyboard(
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancel")).WithCallbackData(t.encodeQuery("add_client_default_ip_limit")),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.unlimited")).WithCallbackData(t.encodeQuery("add_client_ip_limit_c 0")),
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.custom")).WithCallbackData(t.encodeQuery("add_client_ip_limit_in 0")),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton("1").WithCallbackData(t.encodeQuery("add_client_ip_limit_c 1")),
				tu.InlineKeyboardButton("2").WithCallbackData(t.encodeQuery("add_client_ip_limit_c 2")),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton("3").WithCallbackData(t.encodeQuery("add_client_ip_limit_c 3")),
				tu.InlineKeyboardButton("4").WithCallbackData(t.encodeQuery("add_client_ip_limit_c 4")),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton("5").WithCallbackData(t.encodeQuery("add_client_ip_limit_c 5")),
				tu.InlineKeyboardButton("6").WithCallbackData(t.encodeQuery("add_client_ip_limit_c 6")),
				tu.InlineKeyboardButton("7").WithCallbackData(t.encodeQuery("add_client_ip_limit_c 7")),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton("8").WithCallbackData(t.encodeQuery("add_client_ip_limit_c 8")),
				tu.InlineKeyboardButton("9").WithCallbackData(t.encodeQuery("add_client_ip_limit_c 9")),
				tu.InlineKeyboardButton("10").WithCallbackData(t.encodeQuery("add_client_ip_limit_c 10")),
			),
		)
		t.editMessageCallbackTgBot(chatId, callbackQuery.Message.GetMessageID(), inlineKeyboard)
	case "add_client_default_info":
		t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
		t.SendMsgToTgbotDeleteAfter(chatId, t.I18nBot("tgbot.messages.using_default_value"), 3, tu.ReplyKeyboardRemove())
		delete(userStates, chatId)
		inbound, _ := t.inboundService.GetInbound(receiver_inbound_ID)
		message_text, _ := t.BuildInboundClientDataMessage(inbound.Remark, inbound.Protocol)
		t.addClient(chatId, message_text)
	case "add_client_cancel":
		delete(userStates, chatId)
		t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
		t.SendMsgToTgbotDeleteAfter(chatId, t.I18nBot("tgbot.messages.cancel"), 3, tu.ReplyKeyboardRemove())
	case "add_client_default_traffic_exp":
		messageId := callbackQuery.Message.GetMessageID()
		inbound, err := t.inboundService.GetInbound(receiver_inbound_ID)
		if err != nil {
			t.sendCallbackAnswerTgBot(callbackQuery.ID, err.Error())
			return
		}
		message_text, err := t.BuildInboundClientDataMessage(inbound.Remark, inbound.Protocol)
		if err != nil {
			t.sendCallbackAnswerTgBot(callbackQuery.ID, err.Error())
			return
		}
		t.addClient(chatId, message_text, messageId)
		t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.canceled", "Email=="+client_Email))
	case "add_client_default_ip_limit":
		messageId := callbackQuery.Message.GetMessageID()
		inbound, err := t.inboundService.GetInbound(receiver_inbound_ID)
		if err != nil {
			t.sendCallbackAnswerTgBot(callbackQuery.ID, err.Error())
			return
		}
		message_text, err := t.BuildInboundClientDataMessage(inbound.Remark, inbound.Protocol)
		if err != nil {
			t.sendCallbackAnswerTgBot(callbackQuery.ID, err.Error())
			return
		}
		t.addClient(chatId, message_text, messageId)
		t.sendCallbackAnswerTgBot(callbackQuery.ID, t.I18nBot("tgbot.answers.canceled", "Email=="+client_Email))
	case "add_client_submit_disable":
		client_Enable = false
		_, err := t.SubmitAddClient()
		if err != nil {
			errorMessage := fmt.Sprintf("%v", err)
			t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.messages.error_add_client", "error=="+errorMessage), tu.ReplyKeyboardRemove())
		} else {
			t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
			t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.answers.successfulOperation"), tu.ReplyKeyboardRemove())
		}
	case "add_client_submit_enable":
		client_Enable = true
		_, err := t.SubmitAddClient()
		if err != nil {
			errorMessage := fmt.Sprintf("%v", err)
			t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.messages.error_add_client", "error=="+errorMessage), tu.ReplyKeyboardRemove())
		} else {
			t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
			t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.answers.successfulOperation"), tu.ReplyKeyboardRemove())
		}
	case "reset_all_traffics_cancel":
		t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
		t.SendMsgToTgbotDeleteAfter(chatId, t.I18nBot("tgbot.messages.cancel"), 1, tu.ReplyKeyboardRemove())
	case "reset_all_traffics":
		inlineKeyboard := tu.InlineKeyboard(
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancelReset")).WithCallbackData(t.encodeQuery("reset_all_traffics_cancel")),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.confirmResetTraffic")).WithCallbackData(t.encodeQuery("reset_all_traffics_c")),
			),
		)
		t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.messages.AreYouSure"), inlineKeyboard)
	case "reset_all_traffics_c":
		t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
		emails, err := t.inboundService.getAllEmails()
		if err != nil {
			t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.answers.errorOperation"), tu.ReplyKeyboardRemove())
			return
		}

		for _, email := range emails {
			err := t.inboundService.ResetClientTrafficByEmail(email)
			if err == nil {
				msg := t.I18nBot("tgbot.messages.SuccessResetTraffic", "ClientEmail=="+email)
				t.SendMsgToTgbot(chatId, msg, tu.ReplyKeyboardRemove())
			} else {
				msg := t.I18nBot("tgbot.messages.FailedResetTraffic", "ClientEmail=="+email, "ErrorMessage=="+err.Error())
				t.SendMsgToTgbot(chatId, msg, tu.ReplyKeyboardRemove())
			}
		}

		t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.messages.FinishProcess"), tu.ReplyKeyboardRemove())
	case "get_sorted_traffic_usage_report":
		t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
		emails, err := t.inboundService.getAllEmails()

		if err != nil {
			t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.answers.errorOperation"), tu.ReplyKeyboardRemove())
			return
		}
		valid_emails, extra_emails, err := t.inboundService.FilterAndSortClientEmails(emails)
		if err != nil {
			t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.answers.errorOperation"), tu.ReplyKeyboardRemove())
			return
		}
		
		for _, valid_emails := range valid_emails {
			traffic, err := t.inboundService.GetClientTrafficByEmail(valid_emails)
			if err != nil {
				logger.Warning(err)
				msg := t.I18nBot("tgbot.wentWrong")
				t.SendMsgToTgbot(chatId, msg)
				continue
			}
			if traffic == nil {
				msg := t.I18nBot("tgbot.noResult")
				t.SendMsgToTgbot(chatId, msg)
				continue
			}

			output := t.clientInfoMsg(traffic, false, false, false, false, true, false)
			t.SendMsgToTgbot(chatId, output, tu.ReplyKeyboardRemove())
		}
		for _, extra_emails := range extra_emails {
			msg := fmt.Sprintf("📧 %s\n%s", extra_emails, t.I18nBot("tgbot.noResult"))
			t.SendMsgToTgbot(chatId, msg, tu.ReplyKeyboardRemove())

		}

		 // 【新增代码】: 在这里处理新按钮的回调
	 case "oneclick_options":
		 t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
		 t.sendCallbackAnswerTgBot(callbackQuery.ID, "请选择配置类型...")
		 t.sendOneClickOptions(chatId)

	 case "oneclick_reality":
		 t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
		 t.sendCallbackAnswerTgBot(callbackQuery.ID, "🚀 正在创建 Vless + TCP + Reality 节点...")
		 t.SendMsgToTgbot(chatId, "🚀 正在远程创建【Vless + TCP + Reality】节点，请稍候...")
		 t.remoteCreateOneClickInbound("reality", chatId)

	 case "oneclick_xhttp_reality":
		 t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
		 t.sendCallbackAnswerTgBot(callbackQuery.ID, "⚡ 正在创建 Vless + XHTTP + Reality 节点...")
		 t.SendMsgToTgbot(chatId, "⚡ 正在远程创建【Vless + XHTTP + Reality】节点，请稍候...")
		 t.remoteCreateOneClickInbound("xhttp_reality", chatId)	

	 case "oneclick_tls":
		 t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
		 t.sendCallbackAnswerTgBot(callbackQuery.ID, "🛡️ 正在创建 Vless Encryption + XHTTP + TLS 节点...")
		 t.SendMsgToTgbot(chatId, "🛡️ 正在远程创建【Vless Encryption + XHTTP + TLS】节点，请稍候...")
		 t.remoteCreateOneClickInbound("tls", chatId)

	 case "oneclick_switch_vision":
		 t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
		 t.sendCallbackAnswerTgBot(callbackQuery.ID, "🌀 Switch + vision Seed 协议组合的功能还在开发中 ...........")
		 t.SendMsgToTgbot(chatId, "🌀 Switch + vision Seed 协议组合的功能还在开发中 ........，暂不可用...")
		 t.remoteCreateOneClickInbound("switch_vision", chatId)	

	 case "subconverter_install":
		 t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
		 t.sendCallbackAnswerTgBot(callbackQuery.ID, "🔄 正在检查服务...")
		 t.checkAndInstallSubconverter(chatId)

	 case "confirm_sub_install":
		 t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
		 t.sendCallbackAnswerTgBot(callbackQuery.ID, "✅ 指令已发送")
		 t.SendMsgToTgbot(chatId, "【订阅转换】模块正在后台安装，大约需要1-2分钟，完成后将再次通知您。")
		    err := t.serverService.InstallSubconverter()
			if err != nil {
				t.SendMsgToTgbot(chatId, fmt.Sprintf("发送安装指令失败: %v", err))
			}

	 case "cancel_sub_install":
		 t.deleteMessageTgBot(chatId, callbackQuery.Message.GetMessageID())
		 t.sendCallbackAnswerTgBot(callbackQuery.ID, "已取消")
		 t.SendMsgToTgbot(chatId, "已取消【订阅转换】安装操作。")		
	}
}

func (t *Tgbot) BuildInboundClientDataMessage(inbound_remark string, protocol model.Protocol) (string, error) {
	var message string

	currentTime := time.Now()
	timestampMillis := currentTime.UnixNano() / int64(time.Millisecond)

	expiryTime := ""
	diff := client_ExpiryTime/1000 - timestampMillis
	if client_ExpiryTime == 0 {
		expiryTime = t.I18nBot("tgbot.unlimited")
	} else if diff > 172800 {
		expiryTime = time.Unix((client_ExpiryTime / 1000), 0).Format("2006-01-02 15:04:05")
	} else if client_ExpiryTime < 0 {
		expiryTime = fmt.Sprintf("%d %s", client_ExpiryTime/-86400000, t.I18nBot("tgbot.days"))
	} else {
		expiryTime = fmt.Sprintf("%d %s", diff/3600, t.I18nBot("tgbot.hours"))
	}

	traffic_value := ""
	if client_TotalGB == 0 {
		traffic_value = "♾️ Unlimited(Reset)"
	} else {
		traffic_value = common.FormatTraffic(client_TotalGB)
	}

	ip_limit := ""
	if client_LimitIP == 0 {
		ip_limit = "♾️ Unlimited(Reset)"
	} else {
		ip_limit = fmt.Sprint(client_LimitIP)
	}

	switch protocol {
	case model.VMESS, model.VLESS:
		message = t.I18nBot("tgbot.messages.inbound_client_data_id", "InboundRemark=="+inbound_remark, "ClientId=="+client_Id, "ClientEmail=="+client_Email, "ClientTraffic=="+traffic_value, "ClientExp=="+expiryTime, "IpLimit=="+ip_limit, "ClientComment=="+client_Comment)

	case model.Trojan:
		message = t.I18nBot("tgbot.messages.inbound_client_data_pass", "InboundRemark=="+inbound_remark, "ClientPass=="+client_TrPassword, "ClientEmail=="+client_Email, "ClientTraffic=="+traffic_value, "ClientExp=="+expiryTime, "IpLimit=="+ip_limit, "ClientComment=="+client_Comment)

	case model.Shadowsocks:
		message = t.I18nBot("tgbot.messages.inbound_client_data_pass", "InboundRemark=="+inbound_remark, "ClientPass=="+client_ShPassword, "ClientEmail=="+client_Email, "ClientTraffic=="+traffic_value, "ClientExp=="+expiryTime, "IpLimit=="+ip_limit, "ClientComment=="+client_Comment)

	default:
		return "", errors.New("unknown protocol")
	}

	return message, nil
}

func (t *Tgbot) BuildJSONForProtocol(protocol model.Protocol) (string, error) {
	var jsonString string

	switch protocol {
	case model.VMESS:
		jsonString = fmt.Sprintf(`{
            "clients": [{
                "id": "%s",
                "security": "%s",
                "email": "%s",
                "limitIp": %d,
                "totalGB": %d,
                "expiryTime": %d,
                "enable": %t,
                "tgId": "%s",
                "subId": "%s",
                "comment": "%s",
                "reset": %d
            }]
        }`, client_Id, client_Security, client_Email, client_LimitIP, client_TotalGB, client_ExpiryTime, client_Enable, client_TgID, client_SubID, client_Comment, client_Reset)

	case model.VLESS:
		jsonString = fmt.Sprintf(`{
            "clients": [{
                "id": "%s",
                "flow": "%s",
                "email": "%s",
                "limitIp": %d,
                "totalGB": %d,
                "expiryTime": %d,
                "enable": %t,
                "tgId": "%s",
                "subId": "%s",
                "comment": "%s",
                "reset": %d
            }]
        }`, client_Id, client_Flow, client_Email, client_LimitIP, client_TotalGB, client_ExpiryTime, client_Enable, client_TgID, client_SubID, client_Comment, client_Reset)

	case model.Trojan:
		jsonString = fmt.Sprintf(`{
            "clients": [{
                "password": "%s",
                "email": "%s",
                "limitIp": %d,
                "totalGB": %d,
                "expiryTime": %d,
                "enable": %t,
                "tgId": "%s",
                "subId": "%s",
                "comment": "%s",
                "reset": %d
            }]
        }`, client_TrPassword, client_Email, client_LimitIP, client_TotalGB, client_ExpiryTime, client_Enable, client_TgID, client_SubID, client_Comment, client_Reset)

	case model.Shadowsocks:
		jsonString = fmt.Sprintf(`{
            "clients": [{
                "method": "%s",
                "password": "%s",
                "email": "%s",
                "limitIp": %d,
                "totalGB": %d,
                "expiryTime": %d,
                "enable": %t,
                "tgId": "%s",
                "subId": "%s",
                "comment": "%s",
                "reset": %d
            }]
        }`, client_Method, client_ShPassword, client_Email, client_LimitIP, client_TotalGB, client_ExpiryTime, client_Enable, client_TgID, client_SubID, client_Comment, client_Reset)

	default:
		return "", errors.New("unknown protocol")
	}

	return jsonString, nil
}

func (t *Tgbot) SubmitAddClient() (bool, error) {

	inbound, err := t.inboundService.GetInbound(receiver_inbound_ID)
	if err != nil {
		logger.Warning("getIboundClients run failed:", err)
		return false, errors.New(t.I18nBot("tgbot.answers.getInboundsFailed"))
	}

	jsonString, err := t.BuildJSONForProtocol(inbound.Protocol)
	if err != nil {
		logger.Warning("BuildJSONForProtocol run failed:", err)
		return false, errors.New("failed to build JSON for protocol")
	}

	newInbound := &model.Inbound{
		Id:       receiver_inbound_ID,
		Settings: jsonString,
	}

	return t.inboundService.AddInboundClient(newInbound)
}

func checkAdmin(tgId int64) bool {
	for _, adminId := range adminIds {
		if adminId == tgId {
			return true
		}
	}
	return false
}

func (t *Tgbot) SendAnswer(chatId int64, msg string, isAdmin bool) {
	numericKeyboard := tu.InlineKeyboard(
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.SortedTrafficUsageReport")).WithCallbackData(t.encodeQuery("get_sorted_traffic_usage_report")),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.serverUsage")).WithCallbackData(t.encodeQuery("get_usage")),
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.ResetAllTraffics")).WithCallbackData(t.encodeQuery("reset_all_traffics")),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.dbBackup")).WithCallbackData(t.encodeQuery("get_backup")),
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.getBanLogs")).WithCallbackData(t.encodeQuery("get_banlogs")),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.getInbounds")).WithCallbackData(t.encodeQuery("inbounds")),
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.depleteSoon")).WithCallbackData(t.encodeQuery("deplete_soon")),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.commands")).WithCallbackData(t.encodeQuery("commands")),
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.onlines")).WithCallbackData(t.encodeQuery("onlines")),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.allClients")).WithCallbackData(t.encodeQuery("get_inbounds")),
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.addClient")).WithCallbackData(t.encodeQuery("add_client")),
		),
		// 【一键配置】和【订阅转换】按钮的回调数据
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.oneClick")).WithCallbackData(t.encodeQuery("oneclick_options")),
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.subconverter")).WithCallbackData(t.encodeQuery("subconverter_install")),
		),
		// TODOOOOOOOOOOOOOO: Add restart button here.
	)
	numericKeyboardClient := tu.InlineKeyboard(
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.clientUsage")).WithCallbackData(t.encodeQuery("client_traffic")),
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.commands")).WithCallbackData(t.encodeQuery("client_commands")),
		),
	)

	var ReplyMarkup telego.ReplyMarkup
	if isAdmin {
		ReplyMarkup = numericKeyboard
	} else {
		ReplyMarkup = numericKeyboardClient
	}
	t.SendMsgToTgbot(chatId, msg, ReplyMarkup)
}

func (t *Tgbot) SendMsgToTgbot(chatId int64, msg string, replyMarkup ...telego.ReplyMarkup) {
	if !isRunning {
		return
	}

	if msg == "" {
		logger.Info("[tgbot] message is empty!")
		return
	}

	var allMessages []string
	limit := 2000

	// paging message if it is big
	if len(msg) > limit {
		messages := strings.Split(msg, "\r\n\r\n")
		lastIndex := -1

		for _, message := range messages {
			if (len(allMessages) == 0) || (len(allMessages[lastIndex])+len(message) > limit) {
				allMessages = append(allMessages, message)
				lastIndex++
			} else {
				allMessages[lastIndex] += "\r\n\r\n" + message
			}
		}
		if strings.TrimSpace(allMessages[len(allMessages)-1]) == "" {
			allMessages = allMessages[:len(allMessages)-1]
		}
	} else {
		allMessages = append(allMessages, msg)
	}
	for n, message := range allMessages {
		params := telego.SendMessageParams{
			ChatID:    tu.ID(chatId),
			Text:      message,
			ParseMode: "HTML",
		}
		// only add replyMarkup to last message
		if len(replyMarkup) > 0 && n == (len(allMessages)-1) {
			params.ReplyMarkup = replyMarkup[0]
		}
		_, err := bot.SendMessage(context.Background(), &params)
		if err != nil {
			logger.Warning("Error sending telegram message :", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (t *Tgbot) SendMsgToTgbotAdmins(msg string, replyMarkup ...telego.ReplyMarkup) {
	if len(replyMarkup) > 0 {
		for _, adminId := range adminIds {
			t.SendMsgToTgbot(adminId, msg, replyMarkup[0])
		}
	} else {
		for _, adminId := range adminIds {
			t.SendMsgToTgbot(adminId, msg)
		}
	}
}

// 〔中文注释〕: 全新重构的 SendReport 函数，只发送四条趣味性内容。
func (t *Tgbot) SendReport() {
	// --- 第一条消息：发送问候与时间 (顺序 1) ---
    // 修正：确保任务名称即使为空也能发送消息
	runTime, _ := t.settingService.GetTgbotRuntime() 
    taskName := runTime
    if taskName == "" {
        taskName = "未配置任务名称" // 使用占位符，避免因空值跳过
    }

	greetingMsg := fmt.Sprintf(
		"☀️ **每日定时报告** (任务: `%s`)\n\n*美好的一天，从〔X-Panel 面板〕开始！*\n\n⏰ **当前时间**\n`%s`",
		taskName,
		time.Now().Format("2006-01-02 15:04:05"),
	)
	t.SendMsgToTgbotAdmins(greetingMsg) 
	time.Sleep(1000 * time.Millisecond)

	// --- 第二条消息：每日一语（最终稳定版） (顺序 2) ---
	if verse, err := t.getDailyVerse(); err == nil {
		t.SendMsgToTgbotAdmins(verse)
	} else {
		// 即使失败，也记录日志，不影响后续发送
		logger.Warningf("获取每日诗词失败: %v", err)
	}
	time.Sleep(1000 * time.Millisecond)

	// --- 第三条消息：今日美图（三重冗余，已修复） (顺序 3) ---
	t.sendRandomImageWithFallback()
	time.Sleep(1000 * time.Millisecond)

	// --- 第四条消息：新闻资讯简报（最终稳定版：中文 IT/AI/币圈） (顺序 4) ---
	if news, err := t.getNewsBriefingWithFallback(); err == nil {
		t.SendMsgToTgbotAdmins(news)
	} else {
		// 即使失败，也记录日志，不影响发送流程结束
		logger.Warningf("获取所有新闻资讯失败: %v", err)
	}
}

func (t *Tgbot) SendBackupToAdmins() {
	if !t.IsRunning() {
		return
	}
	for _, adminId := range adminIds {
		t.sendBackup(int64(adminId))
	}
}

func (t *Tgbot) sendExhaustedToAdmins() {
	if !t.IsRunning() {
		return
	}
	for _, adminId := range adminIds {
		t.getExhausted(int64(adminId))
	}
}

func (t *Tgbot) getServerUsage(chatId int64, messageID ...int) string {
	info := t.prepareServerUsageInfo()

	keyboard := tu.InlineKeyboard(tu.InlineKeyboardRow(
		tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.refresh")).WithCallbackData(t.encodeQuery("usage_refresh"))))

	if len(messageID) > 0 {
		t.editMessageTgBot(chatId, messageID[0], info, keyboard)
	} else {
		t.SendMsgToTgbot(chatId, info, keyboard)
	}

	return info
}

// Send server usage without an inline keyboard
func (t *Tgbot) sendServerUsage() string {
	info := t.prepareServerUsageInfo()
	return info
}

func (t *Tgbot) prepareServerUsageInfo() string {
	info, ipv4, ipv6 := "", "", ""

	// get latest status of server
	t.lastStatus = t.serverService.GetStatus(t.lastStatus)
	onlines := p.GetOnlineClients()

	info += t.I18nBot("tgbot.messages.hostname", "Hostname=="+hostname)
	info += t.I18nBot("tgbot.messages.version", "Version=="+config.GetVersion())
	info += t.I18nBot("tgbot.messages.xrayVersion", "XrayVersion=="+fmt.Sprint(t.lastStatus.Xray.Version))

	// get ip address
	netInterfaces, err := net.Interfaces()
	if err != nil {
		logger.Error("net.Interfaces failed, err: ", err.Error())
		info += t.I18nBot("tgbot.messages.ip", "IP=="+t.I18nBot("tgbot.unknown"))
		info += "\r\n"
	} else {
		for i := 0; i < len(netInterfaces); i++ {
			if (netInterfaces[i].Flags & net.FlagUp) != 0 {
				addrs, _ := netInterfaces[i].Addrs()

				for _, address := range addrs {
					if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
						if ipnet.IP.To4() != nil {
							ipv4 += ipnet.IP.String() + " "
						} else if ipnet.IP.To16() != nil && !ipnet.IP.IsLinkLocalUnicast() {
							ipv6 += ipnet.IP.String() + " "
						}
					}
				}
			}
		}

		info += t.I18nBot("tgbot.messages.ipv4", "IPv4=="+ipv4)
		info += t.I18nBot("tgbot.messages.ipv6", "IPv6=="+ipv6)
	}

	info += t.I18nBot("tgbot.messages.serverUpTime", "UpTime=="+strconv.FormatUint(t.lastStatus.Uptime/86400, 10), "Unit=="+t.I18nBot("tgbot.days"))
	info += t.I18nBot("tgbot.messages.serverLoad", "Load1=="+strconv.FormatFloat(t.lastStatus.Loads[0], 'f', 2, 64), "Load2=="+strconv.FormatFloat(t.lastStatus.Loads[1], 'f', 2, 64), "Load3=="+strconv.FormatFloat(t.lastStatus.Loads[2], 'f', 2, 64))
	info += t.I18nBot("tgbot.messages.serverMemory", "Current=="+common.FormatTraffic(int64(t.lastStatus.Mem.Current)), "Total=="+common.FormatTraffic(int64(t.lastStatus.Mem.Total)))
	info += t.I18nBot("tgbot.messages.onlinesCount", "Count=="+fmt.Sprint(len(onlines)))
	info += t.I18nBot("tgbot.messages.tcpCount", "Count=="+strconv.Itoa(t.lastStatus.TcpCount))
	info += t.I18nBot("tgbot.messages.udpCount", "Count=="+strconv.Itoa(t.lastStatus.UdpCount))
	info += t.I18nBot("tgbot.messages.traffic", "Total=="+common.FormatTraffic(int64(t.lastStatus.NetTraffic.Sent+t.lastStatus.NetTraffic.Recv)), "Upload=="+common.FormatTraffic(int64(t.lastStatus.NetTraffic.Sent)), "Download=="+common.FormatTraffic(int64(t.lastStatus.NetTraffic.Recv)))
	info += t.I18nBot("tgbot.messages.xrayStatus", "State=="+fmt.Sprint(t.lastStatus.Xray.State))
	return info
}

func (t *Tgbot) UserLoginNotify(username string, password string, ip string, time string, status LoginStatus) {
	if !t.IsRunning() {
		return
	}

	if username == "" || ip == "" || time == "" {
		logger.Warning("UserLoginNotify failed, invalid info!")
		return
	}

	loginNotifyEnabled, err := t.settingService.GetTgBotLoginNotify()
	if err != nil || !loginNotifyEnabled {
		return
	}

	msg := ""
	switch status {
	case LoginSuccess:
		msg += t.I18nBot("tgbot.messages.loginSuccess")
		msg += t.I18nBot("tgbot.messages.hostname", "Hostname=="+hostname)
	case LoginFail:
		msg += t.I18nBot("tgbot.messages.loginFailed")
		msg += t.I18nBot("tgbot.messages.hostname", "Hostname=="+hostname)
		msg += t.I18nBot("tgbot.messages.password", "Password=="+password)
	}
	msg += t.I18nBot("tgbot.messages.username", "Username=="+username)
	msg += t.I18nBot("tgbot.messages.ip", "IP=="+ip)
	msg += t.I18nBot("tgbot.messages.time", "Time=="+time)
	t.SendMsgToTgbotAdmins(msg)
}

func (t *Tgbot) getInboundUsages() string {
	info := ""
	// get traffic
	inbounds, err := t.inboundService.GetAllInbounds()
	if err != nil {
		logger.Warning("GetAllInbounds run failed:", err)
		info += t.I18nBot("tgbot.answers.getInboundsFailed")
	} else {
		// NOTE:If there no any sessions here,need to notify here
		// TODO:Sub-node push, automatic conversion format
		for _, inbound := range inbounds {
			info += t.I18nBot("tgbot.messages.inbound", "Remark=="+inbound.Remark)
			info += t.I18nBot("tgbot.messages.port", "Port=="+strconv.Itoa(inbound.Port))
			info += t.I18nBot("tgbot.messages.traffic", "Total=="+common.FormatTraffic((inbound.Up+inbound.Down)), "Upload=="+common.FormatTraffic(inbound.Up), "Download=="+common.FormatTraffic(inbound.Down))

			if inbound.ExpiryTime == 0 {
				info += t.I18nBot("tgbot.messages.expire", "Time=="+t.I18nBot("tgbot.unlimited"))
			} else {
				info += t.I18nBot("tgbot.messages.expire", "Time=="+time.Unix((inbound.ExpiryTime/1000), 0).Format("2006-01-02 15:04:05"))
			}
			info += "\r\n"
		}
	}
	return info
}
func (t *Tgbot) getInbounds() (*telego.InlineKeyboardMarkup, error) {
	inbounds, err := t.inboundService.GetAllInbounds()
	if err != nil {
		logger.Warning("GetAllInbounds run failed:", err)
		return nil, errors.New(t.I18nBot("tgbot.answers.getInboundsFailed"))
	}

	if len(inbounds) == 0 {
		logger.Warning("No inbounds found")
		return nil, errors.New(t.I18nBot("tgbot.answers.getInboundsFailed"))
	}

	var buttons []telego.InlineKeyboardButton
	for _, inbound := range inbounds {
		status := "❌"
		if inbound.Enable {
			status = "✅"
		}
		callbackData := t.encodeQuery(fmt.Sprintf("%s %d", "get_clients", inbound.Id))
		buttons = append(buttons, tu.InlineKeyboardButton(fmt.Sprintf("%v - %v", inbound.Remark, status)).WithCallbackData(callbackData))
	}

	cols := 1
	if len(buttons) >= 6 {
		cols = 2
	}

	keyboard := tu.InlineKeyboardGrid(tu.InlineKeyboardCols(cols, buttons...))
	return keyboard, nil
}

func (t *Tgbot) getInboundsAddClient() (*telego.InlineKeyboardMarkup, error) {
	inbounds, err := t.inboundService.GetAllInbounds()
	if err != nil {
		logger.Warning("GetAllInbounds run failed:", err)
		return nil, errors.New(t.I18nBot("tgbot.answers.getInboundsFailed"))
	}

	if len(inbounds) == 0 {
		logger.Warning("No inbounds found")
		return nil, errors.New(t.I18nBot("tgbot.answers.getInboundsFailed"))
	}

	excludedProtocols := map[model.Protocol]bool{
		model.Tunnel:    true,
		model.Socks:     true,
		model.WireGuard: true,
		model.HTTP:      true,
	}

	var buttons []telego.InlineKeyboardButton
	for _, inbound := range inbounds {
		if excludedProtocols[inbound.Protocol] {
			continue
		}

		status := "❌"
		if inbound.Enable {
			status = "✅"
		}
		callbackData := t.encodeQuery(fmt.Sprintf("%s %d", "add_client_to", inbound.Id))
		buttons = append(buttons, tu.InlineKeyboardButton(fmt.Sprintf("%v - %v", inbound.Remark, status)).WithCallbackData(callbackData))
	}

	cols := 1
	if len(buttons) >= 6 {
		cols = 2
	}

	keyboard := tu.InlineKeyboardGrid(tu.InlineKeyboardCols(cols, buttons...))
	return keyboard, nil
}

func (t *Tgbot) getInboundClients(id int) (*telego.InlineKeyboardMarkup, error) {
	inbound, err := t.inboundService.GetInbound(id)
	if err != nil {
		logger.Warning("getIboundClients run failed:", err)
		return nil, errors.New(t.I18nBot("tgbot.answers.getInboundsFailed"))
	}
	clients, err := t.inboundService.GetClients(inbound)
	var buttons []telego.InlineKeyboardButton

	if err != nil {
		logger.Warning("GetInboundClients run failed:", err)
		return nil, errors.New(t.I18nBot("tgbot.answers.getInboundsFailed"))
	} else {
		if len(clients) > 0 {
			for _, client := range clients {
				buttons = append(buttons, tu.InlineKeyboardButton(client.Email).WithCallbackData(t.encodeQuery("client_get_usage "+client.Email)))
			}

		} else {
			return nil, errors.New(t.I18nBot("tgbot.answers.getClientsFailed"))
		}

	}
	cols := 0
	if len(buttons) < 6 {
		cols = 3
	} else {
		cols = 2
	}
	keyboard := tu.InlineKeyboardGrid(tu.InlineKeyboardCols(cols, buttons...))

	return keyboard, nil
}

func (t *Tgbot) clientInfoMsg(
	traffic *xray.ClientTraffic,
	printEnabled bool,
	printOnline bool,
	printActive bool,
	printDate bool,
	printTraffic bool,
	printRefreshed bool,
) string {
	now := time.Now().Unix()
	expiryTime := ""
	flag := false
	diff := traffic.ExpiryTime/1000 - now
	if traffic.ExpiryTime == 0 {
		expiryTime = t.I18nBot("tgbot.unlimited")
	} else if diff > 172800 || !traffic.Enable {
		expiryTime = time.Unix((traffic.ExpiryTime / 1000), 0).Format("2006-01-02 15:04:05")
		if diff > 0 {
			days := diff / 86400
			hours := (diff % 86400) / 3600
			minutes := (diff % 3600) / 60
			remainingTime := ""
			if days > 0 {
				remainingTime += fmt.Sprintf("%d %s ", days, t.I18nBot("tgbot.days"))
			}
			if hours > 0 {
				remainingTime += fmt.Sprintf("%d %s ", hours, t.I18nBot("tgbot.hours"))
			}
			if minutes > 0 {
				remainingTime += fmt.Sprintf("%d %s", minutes, t.I18nBot("tgbot.minutes"))
			}
			expiryTime += fmt.Sprintf(" (%s)", remainingTime)
		}
	} else if traffic.ExpiryTime < 0 {
		expiryTime = fmt.Sprintf("%d %s", traffic.ExpiryTime/-86400000, t.I18nBot("tgbot.days"))
		flag = true
	} else {
		expiryTime = fmt.Sprintf("%d %s", diff/3600, t.I18nBot("tgbot.hours"))
		flag = true
	}

	total := ""
	if traffic.Total == 0 {
		total = t.I18nBot("tgbot.unlimited")
	} else {
		total = common.FormatTraffic((traffic.Total))
	}

	enabled := ""
	isEnabled, err := t.inboundService.checkIsEnabledByEmail(traffic.Email)
	if err != nil {
		logger.Warning(err)
		enabled = t.I18nBot("tgbot.wentWrong")
	} else if isEnabled {
		enabled = t.I18nBot("tgbot.messages.yes")
	} else {
		enabled = t.I18nBot("tgbot.messages.no")
	}

	active := ""
	if traffic.Enable {
		active = t.I18nBot("tgbot.messages.yes")
	} else {
		active = t.I18nBot("tgbot.messages.no")
	}

	status := t.I18nBot("tgbot.offline")
	if p.IsRunning() {
		for _, online := range p.GetOnlineClients() {
			if online == traffic.Email {
				status = t.I18nBot("tgbot.online")
				break
			}
		}
	}

	output := ""
	output += t.I18nBot("tgbot.messages.email", "Email=="+traffic.Email)
	if printEnabled {
		output += t.I18nBot("tgbot.messages.enabled", "Enable=="+enabled)
	}
	if printOnline {
		output += t.I18nBot("tgbot.messages.online", "Status=="+status)
	}
	if printActive {
		output += t.I18nBot("tgbot.messages.active", "Enable=="+active)
	}
	if printDate {
		if flag {
			output += t.I18nBot("tgbot.messages.expireIn", "Time=="+expiryTime)
		} else {
			output += t.I18nBot("tgbot.messages.expire", "Time=="+expiryTime)
		}
	}
	if printTraffic {
		output += t.I18nBot("tgbot.messages.upload", "Upload=="+common.FormatTraffic(traffic.Up))
		output += t.I18nBot("tgbot.messages.download", "Download=="+common.FormatTraffic(traffic.Down))
		output += t.I18nBot("tgbot.messages.total", "UpDown=="+common.FormatTraffic((traffic.Up+traffic.Down)), "Total=="+total)
	}
	if printRefreshed {
		output += t.I18nBot("tgbot.messages.refreshedOn", "Time=="+time.Now().Format("2006-01-02 15:04:05"))
	}

	return output
}

func (t *Tgbot) getClientUsage(chatId int64, tgUserID int64, email ...string) {
	traffics, err := t.inboundService.GetClientTrafficTgBot(tgUserID)
	if err != nil {
		logger.Warning(err)
		msg := t.I18nBot("tgbot.wentWrong")
		t.SendMsgToTgbot(chatId, msg)
		return
	}

	if len(traffics) == 0 {
		t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.answers.askToAddUserId", "TgUserID=="+strconv.FormatInt(tgUserID, 10)))
		return
	}

	output := ""

	if len(traffics) > 0 {
		if len(email) > 0 {
			for _, traffic := range traffics {
				if traffic.Email == email[0] {
					output := t.clientInfoMsg(traffic, true, true, true, true, true, true)
					t.SendMsgToTgbot(chatId, output)
					return
				}
			}
			msg := t.I18nBot("tgbot.noResult")
			t.SendMsgToTgbot(chatId, msg)
			return
		} else {
			for _, traffic := range traffics {
				output += t.clientInfoMsg(traffic, true, true, true, true, true, false)
				output += "\r\n"
			}
		}
	}

	output += t.I18nBot("tgbot.messages.refreshedOn", "Time=="+time.Now().Format("2006-01-02 15:04:05"))
	t.SendMsgToTgbot(chatId, output)
	output = t.I18nBot("tgbot.commands.pleaseChoose")
	t.SendAnswer(chatId, output, false)
}

func (t *Tgbot) searchClientIps(chatId int64, email string, messageID ...int) {
	ips, err := t.inboundService.GetInboundClientIps(email)
	if err != nil || len(ips) == 0 {
		ips = t.I18nBot("tgbot.noIpRecord")
	}

	output := ""
	output += t.I18nBot("tgbot.messages.email", "Email=="+email)
	output += t.I18nBot("tgbot.messages.ips", "IPs=="+ips)
	output += t.I18nBot("tgbot.messages.refreshedOn", "Time=="+time.Now().Format("2006-01-02 15:04:05"))

	inlineKeyboard := tu.InlineKeyboard(
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.refresh")).WithCallbackData(t.encodeQuery("ips_refresh "+email)),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.clearIPs")).WithCallbackData(t.encodeQuery("clear_ips "+email)),
		),
	)

	if len(messageID) > 0 {
		t.editMessageTgBot(chatId, messageID[0], output, inlineKeyboard)
	} else {
		t.SendMsgToTgbot(chatId, output, inlineKeyboard)
	}
}

func (t *Tgbot) clientTelegramUserInfo(chatId int64, email string, messageID ...int) {
	traffic, client, err := t.inboundService.GetClientByEmail(email)
	if err != nil {
		logger.Warning(err)
		msg := t.I18nBot("tgbot.wentWrong")
		t.SendMsgToTgbot(chatId, msg)
		return
	}
	if client == nil {
		msg := t.I18nBot("tgbot.noResult")
		t.SendMsgToTgbot(chatId, msg)
		return
	}
	tgId := "None"
	if client.TgID != 0 {
		tgId = strconv.FormatInt(client.TgID, 10)
	}

	output := ""
	output += t.I18nBot("tgbot.messages.email", "Email=="+email)
	output += t.I18nBot("tgbot.messages.TGUser", "TelegramID=="+tgId)
	output += t.I18nBot("tgbot.messages.refreshedOn", "Time=="+time.Now().Format("2006-01-02 15:04:05"))

	inlineKeyboard := tu.InlineKeyboard(
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.refresh")).WithCallbackData(t.encodeQuery("tgid_refresh "+email)),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.removeTGUser")).WithCallbackData(t.encodeQuery("tgid_remove "+email)),
		),
	)

	if len(messageID) > 0 {
		t.editMessageTgBot(chatId, messageID[0], output, inlineKeyboard)
	} else {
		t.SendMsgToTgbot(chatId, output, inlineKeyboard)
		requestUser := telego.KeyboardButtonRequestUsers{
			RequestID: int32(traffic.Id),
			UserIsBot: new(bool),
		}
		keyboard := tu.Keyboard(
			tu.KeyboardRow(
				tu.KeyboardButton(t.I18nBot("tgbot.buttons.selectTGUser")).WithRequestUsers(&requestUser),
			),
			tu.KeyboardRow(
				tu.KeyboardButton(t.I18nBot("tgbot.buttons.closeKeyboard")),
			),
		).WithIsPersistent().WithResizeKeyboard()
		t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.buttons.selectOneTGUser"), keyboard)
	}
}

func (t *Tgbot) searchClient(chatId int64, email string, messageID ...int) {
	traffic, err := t.inboundService.GetClientTrafficByEmail(email)
	if err != nil {
		logger.Warning(err)
		msg := t.I18nBot("tgbot.wentWrong")
		t.SendMsgToTgbot(chatId, msg)
		return
	}
	if traffic == nil {
		msg := t.I18nBot("tgbot.noResult")
		t.SendMsgToTgbot(chatId, msg)
		return
	}

	output := t.clientInfoMsg(traffic, true, true, true, true, true, true)

	inlineKeyboard := tu.InlineKeyboard(
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.refresh")).WithCallbackData(t.encodeQuery("client_refresh "+email)),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.resetTraffic")).WithCallbackData(t.encodeQuery("reset_traffic "+email)),
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.limitTraffic")).WithCallbackData(t.encodeQuery("limit_traffic "+email)),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.resetExpire")).WithCallbackData(t.encodeQuery("reset_exp "+email)),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.ipLog")).WithCallbackData(t.encodeQuery("ip_log "+email)),
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.ipLimit")).WithCallbackData(t.encodeQuery("ip_limit "+email)),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.setTGUser")).WithCallbackData(t.encodeQuery("tg_user "+email)),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.toggle")).WithCallbackData(t.encodeQuery("toggle_enable "+email)),
		),
	)
	if len(messageID) > 0 {
		t.editMessageTgBot(chatId, messageID[0], output, inlineKeyboard)
	} else {
		t.SendMsgToTgbot(chatId, output, inlineKeyboard)
	}
}

func (t *Tgbot) addClient(chatId int64, msg string, messageID ...int) {
	inbound, err := t.inboundService.GetInbound(receiver_inbound_ID)
	if err != nil {
		t.SendMsgToTgbot(chatId, err.Error())
		return
	}

	protocol := inbound.Protocol

	switch protocol {
	case model.VMESS, model.VLESS:
		inlineKeyboard := tu.InlineKeyboard(
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.change_email")).WithCallbackData("add_client_ch_default_email"),
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.change_id")).WithCallbackData("add_client_ch_default_id"),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.limitTraffic")).WithCallbackData("add_client_ch_default_traffic"),
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.resetExpire")).WithCallbackData("add_client_ch_default_exp"),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.change_comment")).WithCallbackData("add_client_ch_default_comment"),
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.ipLimit")).WithCallbackData("add_client_ch_default_ip_limit"),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.submitDisable")).WithCallbackData("add_client_submit_disable"),
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.submitEnable")).WithCallbackData("add_client_submit_enable"),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancel")).WithCallbackData("add_client_cancel"),
			),
		)
		if len(messageID) > 0 {
			t.editMessageTgBot(chatId, messageID[0], msg, inlineKeyboard)
		} else {
			t.SendMsgToTgbot(chatId, msg, inlineKeyboard)
		}
	case model.Trojan:
		inlineKeyboard := tu.InlineKeyboard(
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.change_email")).WithCallbackData("add_client_ch_default_email"),
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.change_password")).WithCallbackData("add_client_ch_default_pass_tr"),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.limitTraffic")).WithCallbackData("add_client_ch_default_traffic"),
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.resetExpire")).WithCallbackData("add_client_ch_default_exp"),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.change_comment")).WithCallbackData("add_client_ch_default_comment"),
				tu.InlineKeyboardButton("ip limit").WithCallbackData("add_client_ch_default_ip_limit"),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.submitDisable")).WithCallbackData("add_client_submit_disable"),
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.submitEnable")).WithCallbackData("add_client_submit_enable"),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancel")).WithCallbackData("add_client_cancel"),
			),
		)
		if len(messageID) > 0 {
			t.editMessageTgBot(chatId, messageID[0], msg, inlineKeyboard)
		} else {
			t.SendMsgToTgbot(chatId, msg, inlineKeyboard)
		}
	case model.Shadowsocks:
		inlineKeyboard := tu.InlineKeyboard(
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.change_email")).WithCallbackData("add_client_ch_default_email"),
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.change_password")).WithCallbackData("add_client_ch_default_pass_sh"),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.limitTraffic")).WithCallbackData("add_client_ch_default_traffic"),
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.resetExpire")).WithCallbackData("add_client_ch_default_exp"),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.change_comment")).WithCallbackData("add_client_ch_default_comment"),
				tu.InlineKeyboardButton("ip limit").WithCallbackData("add_client_ch_default_ip_limit"),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.submitDisable")).WithCallbackData("add_client_submit_disable"),
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.submitEnable")).WithCallbackData("add_client_submit_enable"),
			),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.cancel")).WithCallbackData("add_client_cancel"),
			),
		)

		if len(messageID) > 0 {
			t.editMessageTgBot(chatId, messageID[0], msg, inlineKeyboard)
		} else {
			t.SendMsgToTgbot(chatId, msg, inlineKeyboard)
		}
	}

}

func (t *Tgbot) searchInbound(chatId int64, remark string) {
	inbounds, err := t.inboundService.SearchInbounds(remark)
	if err != nil {
		logger.Warning(err)
		msg := t.I18nBot("tgbot.wentWrong")
		t.SendMsgToTgbot(chatId, msg)
		return
	}
	if len(inbounds) == 0 {
		msg := t.I18nBot("tgbot.noInbounds")
		t.SendMsgToTgbot(chatId, msg)
		return
	}

	for _, inbound := range inbounds {
		info := ""
		info += t.I18nBot("tgbot.messages.inbound", "Remark=="+inbound.Remark)
		info += t.I18nBot("tgbot.messages.port", "Port=="+strconv.Itoa(inbound.Port))
		info += t.I18nBot("tgbot.messages.traffic", "Total=="+common.FormatTraffic((inbound.Up+inbound.Down)), "Upload=="+common.FormatTraffic(inbound.Up), "Download=="+common.FormatTraffic(inbound.Down))

		if inbound.ExpiryTime == 0 {
			info += t.I18nBot("tgbot.messages.expire", "Time=="+t.I18nBot("tgbot.unlimited"))
		} else {
			info += t.I18nBot("tgbot.messages.expire", "Time=="+time.Unix((inbound.ExpiryTime/1000), 0).Format("2006-01-02 15:04:05"))
		}
		t.SendMsgToTgbot(chatId, info)

		if len(inbound.ClientStats) > 0 {
			output := ""
			for _, traffic := range inbound.ClientStats {
				output += t.clientInfoMsg(&traffic, true, true, true, true, true, true)
			}
			t.SendMsgToTgbot(chatId, output)
		}
	}
}

func (t *Tgbot) getExhausted(chatId int64) {
	trDiff := int64(0)
	exDiff := int64(0)
	now := time.Now().Unix() * 1000
	var exhaustedInbounds []model.Inbound
	var exhaustedClients []xray.ClientTraffic
	var disabledInbounds []model.Inbound
	var disabledClients []xray.ClientTraffic

	TrafficThreshold, err := t.settingService.GetTrafficDiff()
	if err == nil && TrafficThreshold > 0 {
		trDiff = int64(TrafficThreshold) * 1073741824
	}
	ExpireThreshold, err := t.settingService.GetExpireDiff()
	if err == nil && ExpireThreshold > 0 {
		exDiff = int64(ExpireThreshold) * 86400000
	}
	inbounds, err := t.inboundService.GetAllInbounds()
	if err != nil {
		logger.Warning("Unable to load Inbounds", err)
	}

	for _, inbound := range inbounds {
		if inbound.Enable {
			if (inbound.ExpiryTime > 0 && (inbound.ExpiryTime-now < exDiff)) ||
				(inbound.Total > 0 && (inbound.Total-(inbound.Up+inbound.Down) < trDiff)) {
				exhaustedInbounds = append(exhaustedInbounds, *inbound)
			}
			if len(inbound.ClientStats) > 0 {
				for _, client := range inbound.ClientStats {
					if client.Enable {
						if (client.ExpiryTime > 0 && (client.ExpiryTime-now < exDiff)) ||
							(client.Total > 0 && (client.Total-(client.Up+client.Down) < trDiff)) {
							exhaustedClients = append(exhaustedClients, client)
						}
					} else {
						disabledClients = append(disabledClients, client)
					}
				}
			}
		} else {
			disabledInbounds = append(disabledInbounds, *inbound)
		}
	}

	// Inbounds
	output := ""
	output += t.I18nBot("tgbot.messages.exhaustedCount", "Type=="+t.I18nBot("tgbot.inbounds"))
	output += t.I18nBot("tgbot.messages.disabled", "Disabled=="+strconv.Itoa(len(disabledInbounds)))
	output += t.I18nBot("tgbot.messages.depleteSoon", "Deplete=="+strconv.Itoa(len(exhaustedInbounds)))

	if len(exhaustedInbounds) > 0 {
		output += t.I18nBot("tgbot.messages.depleteSoon", "Deplete=="+t.I18nBot("tgbot.inbounds"))

		for _, inbound := range exhaustedInbounds {
			output += t.I18nBot("tgbot.messages.inbound", "Remark=="+inbound.Remark)
			output += t.I18nBot("tgbot.messages.port", "Port=="+strconv.Itoa(inbound.Port))
			output += t.I18nBot("tgbot.messages.traffic", "Total=="+common.FormatTraffic((inbound.Up+inbound.Down)), "Upload=="+common.FormatTraffic(inbound.Up), "Download=="+common.FormatTraffic(inbound.Down))
			if inbound.ExpiryTime == 0 {
				output += t.I18nBot("tgbot.messages.expire", "Time=="+t.I18nBot("tgbot.unlimited"))
			} else {
				output += t.I18nBot("tgbot.messages.expire", "Time=="+time.Unix((inbound.ExpiryTime/1000), 0).Format("2006-01-02 15:04:05"))
			}
			output += "\r\n"
		}
	}

	// Clients
	exhaustedCC := len(exhaustedClients)
	output += t.I18nBot("tgbot.messages.exhaustedCount", "Type=="+t.I18nBot("tgbot.clients"))
	output += t.I18nBot("tgbot.messages.disabled", "Disabled=="+strconv.Itoa(len(disabledClients)))
	output += t.I18nBot("tgbot.messages.depleteSoon", "Deplete=="+strconv.Itoa(exhaustedCC))

	if exhaustedCC > 0 {
		output += t.I18nBot("tgbot.messages.depleteSoon", "Deplete=="+t.I18nBot("tgbot.clients"))
		var buttons []telego.InlineKeyboardButton
		for _, traffic := range exhaustedClients {
			output += t.clientInfoMsg(&traffic, true, false, false, true, true, false)
			output += "\r\n"
			buttons = append(buttons, tu.InlineKeyboardButton(traffic.Email).WithCallbackData(t.encodeQuery("client_get_usage "+traffic.Email)))
		}
		cols := 0
		if exhaustedCC < 11 {
			cols = 1
		} else {
			cols = 2
		}
		output += t.I18nBot("tgbot.messages.refreshedOn", "Time=="+time.Now().Format("2006-01-02 15:04:05"))
		keyboard := tu.InlineKeyboardGrid(tu.InlineKeyboardCols(cols, buttons...))
		t.SendMsgToTgbot(chatId, output, keyboard)
	} else {
		output += t.I18nBot("tgbot.messages.refreshedOn", "Time=="+time.Now().Format("2006-01-02 15:04:05"))
		t.SendMsgToTgbot(chatId, output)
	}
}

func (t *Tgbot) notifyExhausted() {
	trDiff := int64(0)
	exDiff := int64(0)
	now := time.Now().Unix() * 1000

	TrafficThreshold, err := t.settingService.GetTrafficDiff()
	if err == nil && TrafficThreshold > 0 {
		trDiff = int64(TrafficThreshold) * 1073741824
	}
	ExpireThreshold, err := t.settingService.GetExpireDiff()
	if err == nil && ExpireThreshold > 0 {
		exDiff = int64(ExpireThreshold) * 86400000
	}
	inbounds, err := t.inboundService.GetAllInbounds()
	if err != nil {
		logger.Warning("Unable to load Inbounds", err)
	}

	var chatIDsDone []int64
	for _, inbound := range inbounds {
		if inbound.Enable {
			if len(inbound.ClientStats) > 0 {
				clients, err := t.inboundService.GetClients(inbound)
				if err == nil {
					for _, client := range clients {
						if client.TgID != 0 {
							chatID := client.TgID
							if !int64Contains(chatIDsDone, chatID) && !checkAdmin(chatID) {
								var disabledClients []xray.ClientTraffic
								var exhaustedClients []xray.ClientTraffic
								traffics, err := t.inboundService.GetClientTrafficTgBot(client.TgID)
								if err == nil && len(traffics) > 0 {
									output := t.I18nBot("tgbot.messages.exhaustedCount", "Type=="+t.I18nBot("tgbot.clients"))
									for _, traffic := range traffics {
										if traffic.Enable {
											if (traffic.ExpiryTime > 0 && (traffic.ExpiryTime-now < exDiff)) ||
												(traffic.Total > 0 && (traffic.Total-(traffic.Up+traffic.Down) < trDiff)) {
												exhaustedClients = append(exhaustedClients, *traffic)
											}
										} else {
											disabledClients = append(disabledClients, *traffic)
										}
									}
									if len(exhaustedClients) > 0 {
										output += t.I18nBot("tgbot.messages.disabled", "Disabled=="+strconv.Itoa(len(disabledClients)))
										if len(disabledClients) > 0 {
											output += t.I18nBot("tgbot.clients") + ":\r\n"
											for _, traffic := range disabledClients {
												output += " " + traffic.Email
											}
											output += "\r\n"
										}
										output += "\r\n"
										output += t.I18nBot("tgbot.messages.depleteSoon", "Deplete=="+strconv.Itoa(len(exhaustedClients)))
										for _, traffic := range exhaustedClients {
											output += t.clientInfoMsg(&traffic, true, false, false, true, true, false)
											output += "\r\n"
										}
										t.SendMsgToTgbot(chatID, output)
									}
									chatIDsDone = append(chatIDsDone, chatID)
								}
							}
						}
					}
				}
			}
		}
	}
}

func int64Contains(slice []int64, item int64) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func (t *Tgbot) onlineClients(chatId int64, messageID ...int) {
	if !p.IsRunning() {
		return
	}

	onlines := p.GetOnlineClients()
	onlinesCount := len(onlines)
	output := t.I18nBot("tgbot.messages.onlinesCount", "Count=="+fmt.Sprint(onlinesCount))
	keyboard := tu.InlineKeyboard(tu.InlineKeyboardRow(
		tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.refresh")).WithCallbackData(t.encodeQuery("onlines_refresh"))))

	if onlinesCount > 0 {
		var buttons []telego.InlineKeyboardButton
		for _, online := range onlines {
			buttons = append(buttons, tu.InlineKeyboardButton(online).WithCallbackData(t.encodeQuery("client_get_usage "+online)))
		}
		cols := 0
		if onlinesCount < 21 {
			cols = 2
		} else if onlinesCount < 61 {
			cols = 3
		} else {
			cols = 4
		}
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, tu.InlineKeyboardCols(cols, buttons...)...)
	}

	if len(messageID) > 0 {
		t.editMessageTgBot(chatId, messageID[0], output, keyboard)
	} else {
		t.SendMsgToTgbot(chatId, output, keyboard)
	}
}

func (t *Tgbot) sendBackup(chatId int64) {
	output := t.I18nBot("tgbot.messages.backupTime", "Time=="+time.Now().Format("2006-01-02 15:04:05"))
	t.SendMsgToTgbot(chatId, output)

	// Update by manually trigger a checkpoint operation
	err := database.Checkpoint()
	if err != nil {
		logger.Error("Error in trigger a checkpoint operation: ", err)
	}

	file, err := os.Open(config.GetDBPath())
	if err == nil {
		document := tu.Document(
			tu.ID(chatId),
			tu.File(file),
		)
		_, err = bot.SendDocument(context.Background(), document)
		if err != nil {
			logger.Error("Error in uploading backup: ", err)
		}
	} else {
		logger.Error("Error in opening db file for backup: ", err)
	}

	file, err = os.Open(xray.GetConfigPath())
	if err == nil {
		document := tu.Document(
			tu.ID(chatId),
			tu.File(file),
		)
		_, err = bot.SendDocument(context.Background(), document)
		if err != nil {
			logger.Error("Error in uploading config.json: ", err)
		}
	} else {
		logger.Error("Error in opening config.json file for backup: ", err)
	}
}

func (t *Tgbot) sendBanLogs(chatId int64, dt bool) {
	if dt {
		output := t.I18nBot("tgbot.messages.datetime", "DateTime=="+time.Now().Format("2006-01-02 15:04:05"))
		t.SendMsgToTgbot(chatId, output)
	}

	file, err := os.Open(xray.GetIPLimitBannedPrevLogPath())
	if err == nil {
		// Check if the file is non-empty before attempting to upload
		fileInfo, _ := file.Stat()
		if fileInfo.Size() > 0 {
			document := tu.Document(
				tu.ID(chatId),
				tu.File(file),
			)
			_, err = bot.SendDocument(context.Background(), document)
			if err != nil {
				logger.Error("Error in uploading IPLimitBannedPrevLog: ", err)
			}
		} else {
			logger.Warning("IPLimitBannedPrevLog file is empty, not uploading.")
		}
		file.Close()
	} else {
		logger.Error("Error in opening IPLimitBannedPrevLog file for backup: ", err)
	}

	file, err = os.Open(xray.GetIPLimitBannedLogPath())
	if err == nil {
		// Check if the file is non-empty before attempting to upload
		fileInfo, _ := file.Stat()
		if fileInfo.Size() > 0 {
			document := tu.Document(
				tu.ID(chatId),
				tu.File(file),
			)
			_, err = bot.SendDocument(context.Background(), document)
			if err != nil {
				logger.Error("Error in uploading IPLimitBannedLog: ", err)
			}
		} else {
			logger.Warning("IPLimitBannedLog file is empty, not uploading.")
		}
		file.Close()
	} else {
		logger.Error("Error in opening IPLimitBannedLog file for backup: ", err)
	}
}

func (t *Tgbot) sendCallbackAnswerTgBot(id string, message string) {
	params := telego.AnswerCallbackQueryParams{
		CallbackQueryID: id,
		Text:            message,
	}
	if err := bot.AnswerCallbackQuery(context.Background(), &params); err != nil {
		logger.Warning(err)
	}
}

func (t *Tgbot) editMessageCallbackTgBot(chatId int64, messageID int, inlineKeyboard *telego.InlineKeyboardMarkup) {
	params := telego.EditMessageReplyMarkupParams{
		ChatID:      tu.ID(chatId),
		MessageID:   messageID,
		ReplyMarkup: inlineKeyboard,
	}
	if _, err := bot.EditMessageReplyMarkup(context.Background(), &params); err != nil {
		logger.Warning(err)
	}
}

func (t *Tgbot) editMessageTgBot(chatId int64, messageID int, text string, inlineKeyboard ...*telego.InlineKeyboardMarkup) {
	params := telego.EditMessageTextParams{
		ChatID:    tu.ID(chatId),
		MessageID: messageID,
		Text:      text,
		ParseMode: "HTML",
	}
	if len(inlineKeyboard) > 0 {
		params.ReplyMarkup = inlineKeyboard[0]
	}
	if _, err := bot.EditMessageText(context.Background(), &params); err != nil {
		logger.Warning(err)
	}
}

func (t *Tgbot) SendMsgToTgbotDeleteAfter(chatId int64, msg string, delayInSeconds int, replyMarkup ...telego.ReplyMarkup) {
	// Determine if replyMarkup was passed; otherwise, set it to nil
	var replyMarkupParam telego.ReplyMarkup
	if len(replyMarkup) > 0 {
		replyMarkupParam = replyMarkup[0] // Use the first element
	}

	// Send the message
	sentMsg, err := bot.SendMessage(context.Background(), &telego.SendMessageParams{
		ChatID:      tu.ID(chatId),
		Text:        msg,
		ReplyMarkup: replyMarkupParam, // Use the correct replyMarkup value
	})
	if err != nil {
		logger.Warning("Failed to send message:", err)
		return
	}

	// Delete the sent message after the specified number of seconds
	go func() {
		time.Sleep(time.Duration(delayInSeconds) * time.Second) // Wait for the specified delay
		t.deleteMessageTgBot(chatId, sentMsg.MessageID)         // Delete the message
		delete(userStates, chatId)
	}()
}

func (t *Tgbot) deleteMessageTgBot(chatId int64, messageID int) {
	params := telego.DeleteMessageParams{
		ChatID:    tu.ID(chatId),
		MessageID: messageID,
	}
	if err := bot.DeleteMessage(context.Background(), &params); err != nil {
		logger.Warning("Failed to delete message:", err)
	} else {
		logger.Info("Message deleted successfully")
	}
}

func (t *Tgbot) isSingleWord(text string) bool {
	text = strings.TrimSpace(text)
	re := regexp.MustCompile(`\s+`)
	return re.MatchString(text)
}

// 〔中文注释〕: 新增方法，实现 TelegramService 接口。
// 当设备限制任务需要发送消息时，会调用此方法。
// 该方法内部调用了已有的 SendMsgToTgbotAdmins 函数，将消息发送给所有管理员。
func (t *Tgbot) SendMessage(msg string) error {
    if !t.IsRunning() {
        // 〔中文注释〕: 如果 Bot 未运行，返回错误，防止程序出错。
        return errors.New("Telegram bot is not running")
    }
    // 〔中文注释〕: 调用现有方法将消息发送给所有已配置的管理员。
    t.SendMsgToTgbotAdmins(msg)
    return nil
}

// 【新增函数】: 发送【一键配置】的选项按钮给用户
func (t *Tgbot) sendOneClickOptions(chatId int64) {
	optionsKeyboard := tu.InlineKeyboard(
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("🚀 Vless + TCP + Reality + Vision").WithCallbackData(t.encodeQuery("oneclick_reality")),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("⚡ Vless + XHTTP + Reality").WithCallbackData(t.encodeQuery("oneclick_xhttp_reality")),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("🛡️ Vless Encryption + XHTTP + TLS").WithCallbackData(t.encodeQuery("oneclick_tls")),
		),
		// 【新增占位按钮】: 为未来的 Switch + vision Seed 预留位置
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("🌀 Switch + vision Seed (开发中)").WithCallbackData(t.encodeQuery("oneclick_switch_vision")),
		),
	)
	t.SendMsgToTgbot(chatId, "请选择您要创建的【一键配置】类型：\n（以下最前面两种适合优化线路去直连）", optionsKeyboard)
}

// 【新增函数】: 检查并安装【订阅转换】
func (t *Tgbot) checkAndInstallSubconverter(chatId int64) {
	domain, err := t.getDomain()
	if err != nil {
		t.SendMsgToTgbot(chatId, fmt.Sprintf("❌ 操作失败：%v", err))
		return
	}
	subConverterUrl := fmt.Sprintf("https://%s:15268", domain)

	t.SendMsgToTgbot(chatId, fmt.Sprintf("正在检测服务状态...\n地址: `%s`", subConverterUrl))

	go func() {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
		_, err := client.Get(subConverterUrl)

		if err == nil {
			t.SendMsgToTgbot(chatId, fmt.Sprintf("✅ 服务已存在！\n\n您可以直接通过以下地址访问：\n`%s`", subConverterUrl))
		} else {
			confirmKeyboard := tu.InlineKeyboard(
				tu.InlineKeyboardRow(
					tu.InlineKeyboardButton("✅ 是，立即安装").WithCallbackData("confirm_sub_install"),
					tu.InlineKeyboardButton("❌ 否，取消").WithCallbackData("cancel_sub_install"),
				),
			)
			t.SendMsgToTgbot(chatId, "⚠️ 服务检测失败，可能尚未安装。\n\n您想现在执行安装指令吗？\n\n**【重要】**请确保服务器防火墙已放行 `8000` 和 `15268` 端口。", confirmKeyboard)
		}
	}()
}

// 远程创建【一键配置】入站，增加一个 type 参数
func (t *Tgbot) remoteCreateOneClickInbound(configType string, chatId int64) {
	var err error
	var newInbound *model.Inbound
	var ufwWarning string // 新增变量来捕获警告信息

	if configType == "reality" {
		newInbound, ufwWarning, err = t.buildRealityInbound()
	} else if configType == "xhttp_reality" {
		newInbound, ufwWarning, err = t.buildXhttpRealityInbound()
	} else if configType == "tls" {
		newInbound, ufwWarning, err = t.buildTlsInbound()
	} else if configType == "switch_vision" { // 【新增】: 处理开发中的选项
		t.SendMsgToTgbot(chatId, "此协议组合的功能还在开发中 ............")
		return // 【中文注释】: 直接返回，不执行任何创建操作
	} else {
		err = errors.New("未知的配置类型")
	}

	if err != nil {
		t.SendMsgToTgbot(chatId, fmt.Sprintf("❌ 远程创建失败: %v", err))
		return
	}

	// 〔中文注释〕: 创建一个 InboundService 实例，并将当前的 Tgbot 实例 (t) 作为 tgService 注入进去。
	inboundService := InboundService{}
	inboundService.SetTelegramService(t) // 将当前的 bot 实例注入

	createdInbound, _, err := inboundService.AddInbound(newInbound)
	
	if err != nil {
		t.SendMsgToTgbot(chatId, fmt.Sprintf("❌ 远程创建失败: 保存入站时出错: %v", err))
		return
	}

	logger.Infof("TG 机器人远程创建入站 %s 成功！", createdInbound.Remark)

	// 【新增功能】：如果端口放行失败，发送警告
    if ufwWarning != "" { 
        t.SendMsgToTgbot(chatId, ufwWarning) 
    } // END NEW LOGIC

	// 【调用 TG Bot 专属的通知方法】
    // inFromPanel 设置为 false，表示这是来自 TG 机器人的操作
    // 之前 SendOneClickConfig 的 inbound 参数是 *model.Inbound，所以我们传入 createdInbound
	// 将当前的 chatId 传入，确保配置消息发送给发起指令的用户
    err = t.SendOneClickConfig(createdInbound, false, chatId)
    if err != nil {
        // 如果发送通知失败，给用户一个提示，但不要中断流程
        t.SendMsgToTgbot(chatId, fmt.Sprintf("⚠️ 入站创建成功，但通知消息发送失败: %v", err))
        logger.Errorf("TG Bot: 远程创建入站成功，但发送通知失败: %v", err)
    } else {
        // 成功发送二维码/配置消息后，再给用户一个确认提示
        t.SendMsgToTgbot(chatId, "✅ **入站已创建，【二维码/配置链接】已发送至管理员私信。**")
    }
	// 【新增功能】：发送用法说明消息
    // 使用 ** 粗体标记，并使用多行字符串确保换行显示。
    usageMessage := `**用法说明：**
该功能已自动生成现今比较主流的入站协议，简单/直接，不用慢慢配置。
【一键配置】生成功能中的最前面两种协议组合，适合【优化线路】去直连使用。
并随机分配一个可用端口，请确保此端口放行，生成后请直接复制【**链接地址**】。
TG端 的【一键配置】生成功能，与后台 Web端 类似，跟【入站】的数据是打通的。
你可以在一键创建后于列表中，手动查看/复制或编辑详细信息，以便添加其他参数。`
	
    t.SendMsgToTgbot(chatId, usageMessage)
}

// 【新增函数】: 构建 Reality 配置对象 (1:1 复刻自 inbounds.html)
func (t *Tgbot) buildRealityInbound() (*model.Inbound, string, error) {
	keyPairMsg, err := t.serverService.GetNewX25519Cert()
	if err != nil {
		return nil, "", fmt.Errorf("获取 Reality 密钥对失败: %v", err)
	}
	uuidMsg, err := t.serverService.GetNewUUID()
	if err != nil {
		return nil, "", fmt.Errorf("获取 UUID 失败: %v", err)
	}

	keyPair := keyPairMsg.(map[string]any)
	privateKey, publicKey := keyPair["privateKey"].(string), keyPair["publicKey"].(string)
	uuid := uuidMsg["uuid"]
	remark := t.randomString(8, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789")
	port := 10000 + common.RandomInt(55535 - 10000 + 1)

	var ufwWarning string = "" // NEW

    // 【新增功能】：调用 ufw 放行端口
	if err := t.openPortWithUFW(port); err != nil {
        // 【核心修改】：如果端口放行失败，不中断入站创建流程，但生成警告信息
		logger.Warningf("自动放行端口 %d 失败: %v", port, err)
        ufwWarning = fmt.Sprintf("⚠️ **警告：端口放行失败**\n\n自动执行 `ufw allow %d` 命令失败，入站创建流程已继续，但请务必**手动**在您的 VPS 上放行端口 `%d`，否则服务将无法访问。失败详情：%v", port, port, err)
	} // END NEW LOGIC

	// 按照要求格式：inbound-端口号
    tag := fmt.Sprintf("inbound-%d", port) 

	realityDests := []string{"tesla.com:443", "sega.com:443", "apple.com:443", "icloud.com:443", "lovelive-anime.jp:443", "meta.com:443"}
	randomDest := realityDests[common.RandomInt(len(realityDests))]
	randomSni := strings.Split(randomDest, ":")[0]
	shortIds := t.generateShortIds()

                 // Settings (clients + decryption + fallbacks)
                settings, _ := json.Marshal(map[string]any{
                   "clients": []map[string]any{{
                         "id":     uuid,                      // 客户端 UUID
                         "flow":   "xtls-rprx-vision",        // JS 中指定的 flow
                         "email":  remark,
                         "level":  0,
                         "enable": true,
                      }},
                   "decryption": "none",
                   "fallbacks":  []any{}, // 保留空数组（与前端一致）
                 })

                // StreamSettings => reality
                streamSettings, _ := json.Marshal(map[string]any{
                        "network":  "tcp",
                        "security": "reality",
                        "realitySettings": map[string]any{
                               "show":        false,            // 前端 show: false
                               "target":      randomDest,       // e.g. "apple.com:443"
                               "xver":        0,
                               "serverNames": []string{randomSni, "www." + randomSni},
                              // 注意：realitySettings.settings 是一个对象（map），不是数组
                               "settings": map[string]any{
                                   "publicKey":    publicKey,
                                   "spiderX":      "/",          // 前端写了 spiderX: "/"
                                   "mldsa65Verify": "",
                           },
                        "privateKey":   privateKey,
                        "maxClientVer": "",
                        "minClientVer": "",
                        "maxTimediff":  0,
                        "mldsa65Seed":  "",             // 一般留空（JS 注释）
                        "shortIds":     shortIds,       // 传入的短 id 列表
                   },
                       // TCP 子对象
                      "tcpSettings": map[string]any{
                      "acceptProxyProtocol": false,
                      "header": map[string]any{
                      "type": "none",
                    },
                 },
              })

             // sniffing 完整保留（enabled + destOverride + metadataOnly + routeOnly）
              sniffing, _ := json.Marshal(map[string]any{
                   "enabled":      true,
                   "destOverride": []string{"http", "tls", "quic", "fakedns"},
                   "metadataOnly": false,
                   "routeOnly":    false,
              })

            // 返回 model.Inbound —— 请根据你项目中的 model.Inbound 增减字段（此处包含常见字段）
             return &model.Inbound{
                    UserId:         1,                      // 示例：创建者/系统用户 ID，如需动态请替换
                    Remark:         remark,
                    Enable:         true,
                    Listen:         "",                     // 对应前端 listen: ''
                    Port:           port,
				    Tag:            tag,
                    Protocol:       "vless",
                   // 如果你的 model.Inbound 有这些字段（前端 data 也包含），可以设置或保持默认
                    ExpiryTime:     0,                      // 前端 expiryTime: 0
                    DeviceLimit:    0,                      // 前端 deviceLimit: 0
                    Settings:       string(settings),
                    StreamSettings: string(streamSettings),
                    Sniffing:       string(sniffing),
                }, ufwWarning, nil // MODIFIED RETURN
            }

// 【新增函数】: 构建 TLS 配置对象 (1:1 复刻自 inbounds.html)
func (t *Tgbot) buildTlsInbound() (*model.Inbound, string, error) { // 更改签名
	encMsg, err := t.serverService.GetNewVlessEnc()
	if err != nil {
		return nil, "", fmt.Errorf("获取 VLESS 加密配置失败: %v", err)
	}
	uuidMsg, err := t.serverService.GetNewUUID()
	if err != nil {
		return nil, "", fmt.Errorf("获取 UUID 失败: %v", err)
	}

	var decryption, encryption string

	// 确认顶层类型是 map[string]interface{}
	encMsgMap, ok := encMsg.(map[string]interface{})
	if !ok {
		return nil, "", fmt.Errorf("VLESS 加密配置格式不正确: 期望得到 map[string]interface {}，但收到了 %T", encMsg)
	}
	
	// 从顶层 map 中直接获取 "auths" 键的值
	authsVal, found := encMsgMap["auths"]
	
	if !found {
		return nil, "", errors.New("VLESS 加密配置 auths 格式不正确: 未能在响应中找到 'auths' 数组")
	}

	// 将 auths 的值断言为正确的类型 []map[string]string 
    // 这是因为 server.go 中的 GetNewVlessEnc 明确返回这个类型。
	auths, ok := authsVal.([]map[string]string)
	if !ok {
        // 如果断言失败，则意味着 auths 数组的内部元素类型不匹配
		return nil, "", fmt.Errorf("VLESS 加密配置 auths 格式不正确: 'auths' 数组的内部元素类型应为 map[string]string，但收到了 %T", authsVal)
	}
	
	// 遍历 auths 数组寻找 ML-KEM-768
	for _, auth := range auths {
        // 现在 auth 已经是 map[string]string 类型，可以直接安全访问
		if label, ok2 := auth["label"]; ok2 && label == "ML-KEM-768, Post-Quantum" {
			decryption = auth["decryption"]
			encryption = auth["encryption"]
			break // 找到后跳出循环
		}
	}

	if decryption == "" || encryption == "" {
		return nil, "", errors.New("未能在 auths 数组中找到 ML-KEM-768 加密密钥，请检查 Xray 版本")
	}

	domain, err := t.getDomain()
	if err != nil {
		return nil, "", err
	}

	uuid := uuidMsg["uuid"]
	remark := t.randomString(8, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789")
	allowedPorts := []int{2053, 2083, 2087, 2096, 8443}
	port := allowedPorts[common.RandomInt(len(allowedPorts))]
	
	var ufwWarning string = "" // NEW

    // 【新增功能】：调用 ufw 放行端口
	if err := t.openPortWithUFW(port); err != nil {
        // 【核心修改】：如果端口放行失败，不中断入站创建流程，但生成警告信息
		logger.Warningf("自动放行端口 %d 失败: %v", port, err)
        ufwWarning = fmt.Sprintf("⚠️ **警告：端口放行失败**\n\n自动执行 `ufw allow %d` 命令失败，入站创建流程已继续，但请务必**手动**在您的 VPS 上放行端口 `%d`，否则服务将无法访问。失败详情：%v", port, port, err)
	} // END NEW LOGIC
	
	// 按照要求格式：inbound-端口号
    tag := fmt.Sprintf("inbound-%d", port) 
	path := "/" + t.randomString(8, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")
	certPath := fmt.Sprintf("/root/cert/%s/fullchain.pem", domain)
	keyPath := fmt.Sprintf("/root/cert/%s/privkey.pem", domain)

                // Settings: clients + decryption + encryption + selectedAuth
                 settings, _ := json.Marshal(map[string]any{
                      "clients": []map[string]any{{
                            "id":       uuid,
                            "flow":     "",       // JS 中 flow: ""
                            "email":    remark,
                            "level":    0,
                            "password": "",       // JS 中 password: ""
                            "enable":   true,
                       }},
                     "decryption":   decryption,                  // 从 API 获取
                     "encryption":   encryption,                  // 从 API 获取（新增）
                     "selectedAuth": "ML-KEM-768, Post-Quantum",  // 前端硬编码选择项
                 })

               // streamSettings：network=xhttp, security=tls, tlsSettings + xhttpSettings
               streamSettings, _ := json.Marshal(map[string]any{
                       "network":  "xhttp",
                       "security": "tls",
                       "tlsSettings": map[string]any{
                              "alpn":                    []string{"h2", "http/1.1"},
                              "certificates": []map[string]any{{
                                      "buildChain":     false,
                                      "certificateFile": certPath,
                                      "keyFile":         keyPath,
                                      "oneTimeLoading":  false,
                                      "usage":           "encipherment",
                      }},
                             "cipherSuites":             "",
                             "disableSystemRoot":        false,
                             "echForceQuery":            "none",
                             "echServerKeys":            "",
                             "enableSessionResumption":  false,
                             "maxVersion":               "1.3",
                             "minVersion":               "1.2",
                             "rejectUnknownSni":         false,
                             "serverName":               domain,
                             "verifyPeerCertInNames":    []string{"dns.google", "cloudflare-dns.com"},
                        },
                    "xhttpSettings": map[string]any{
                            "headers":            map[string]any{}, // 可按需填充（JS 为 {}）
                            "host":               "",               // 前端留空
                            "mode":               "packet-up",
                            "noSSEHeader":        false,
                            "path":               path,             // 随机 8 位路径
                            "scMaxBufferedPosts": 30,
                            "scMaxEachPostBytes": "1000000",
                            "scStreamUpServerSecs": "20-80",
                            "xPaddingBytes":      "100-1000",
                      },
                 })

              // sniffing: 与前端一致（enabled:false）
              sniffing, _ := json.Marshal(map[string]any{
                   "enabled":      false,
                   "destOverride": []string{"http", "tls", "quic", "fakedns"},
                   "metadataOnly": false,
                   "routeOnly":    false,
               })

              return &model.Inbound{
                     UserId:         1,
                     Remark:         remark,
                     Enable:         true,
                     Listen:         "",
                     Port:           port,
				     Tag:            tag,
                     Protocol:       "vless",
                     ExpiryTime:     0,
                     DeviceLimit:    0,
                     Settings:       string(settings),
                     StreamSettings: string(streamSettings),
                     Sniffing:       string(sniffing),
               }, ufwWarning, nil // MODIFIED RETURN
           }

// 【新增函数】: 构建 VLESS + XHTTP + Reality 配置对象
func (t *Tgbot) buildXhttpRealityInbound() (*model.Inbound, string, error) {
	keyPairMsg, err := t.serverService.GetNewX25519Cert()
	if err != nil {
		return nil, "", fmt.Errorf("获取 Reality 密钥对失败: %v", err)
	}
	uuidMsg, err := t.serverService.GetNewUUID()
	if err != nil {
		return nil, "", fmt.Errorf("获取 UUID 失败: %v", err)
	}

	keyPair := keyPairMsg.(map[string]any)
	privateKey, publicKey := keyPair["privateKey"].(string), keyPair["publicKey"].(string)
	uuid := uuidMsg["uuid"]
	remark := t.randomString(8, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789")
	port := 10000 + common.RandomInt(55535-10000+1)
	path := "/" + t.randomString(8, "abcdefghijklmnopqrstuvwxyz")

	var ufwWarning string
	if err := t.openPortWithUFW(port); err != nil {
		logger.Warningf("自动放行端口 %d 失败: %v", port, err)
        ufwWarning = fmt.Sprintf("⚠️ **警告：端口放行失败**\n\n自动执行 `ufw allow %d` 命令失败，但入站创建已继续。请务必**手动**在您的 VPS 上放行端口 `%d`，否则服务将无法访问。", port, port)
	}

	tag := fmt.Sprintf("inbound-%d", port)

	realityDests := []string{"tesla.com:443", "sega.com:443", "apple.com:443", "icloud.com:443", "lovelive-anime.jp:443", "meta.com:443"}
	randomDest := realityDests[common.RandomInt(len(realityDests))]
	randomSni := strings.Split(randomDest, ":")[0]
	shortIds := t.generateShortIds()

	settings, _ := json.Marshal(map[string]any{
		"clients": []map[string]any{{
			"id":       uuid,
            "flow":     "",       // 在 XHTTP 中 flow: ""
            "email":    remark,
            "level":    0,
            "password": "",       // JS 中 password: ""
            "enable":   true,
		}},
		"decryption":   "none",
		"selectedAuth": "X25519, not Post-Quantum",
	})

	streamSettings, _ := json.Marshal(map[string]any{
		"network":  "xhttp",
		"security": "reality",
		"realitySettings": map[string]any{
			"show":         false,
			"target":       randomDest,
			"xver":         0,
			"serverNames":  []string{randomSni, "www." + randomSni},
			"privateKey":   privateKey,
			"maxClientVer": "",
			"minClientVer": "",
			"maxTimediff":  0,
			"mldsa65Seed":  "",
			"shortIds":     shortIds,
			"settings": map[string]any{
				"publicKey":    publicKey,
                "spiderX":      "/",          // 前端写了 spiderX: "/"
                "mldsa65Verify": "",
			},
		},
		"xhttpSettings": map[string]any{
			"headers":              map[string]any{},
			"host":                 "",
			"mode":                 "stream-up",
			"noSSEHeader":          false,
			"path":                 path,
			"scMaxBufferedPosts":   30,
			"scMaxEachPostBytes":   "1000000",
			"scStreamUpServerSecs": "20-80",
			"xPaddingBytes":        "100-1000",
		},
	})

	sniffing, _ := json.Marshal(map[string]any{
		"enabled":      true,
		"destOverride": []string{"http", "tls", "quic", "fakedns"},
		"metadataOnly": false,
		"routeOnly":    false,
	})

	return &model.Inbound{
		UserId:         1,
		Remark:         remark,
		Enable:         true,
		Listen:         "",
		Port:           port,
		Tag:            tag,
		Protocol:       "vless",
		ExpiryTime:     0,
		DeviceLimit:    0,
		Settings:       string(settings),
		StreamSettings: string(streamSettings),
		Sniffing:       string(sniffing),
	}, ufwWarning, nil
}

// 【修改后函数】: 发送【一键配置】的专属消息，增加链接类型判断
func (t *Tgbot) SendOneClickConfig(inbound *model.Inbound, inFromPanel bool, targetChatId int64) error {	
	var link string
	var err error
	var linkType string
	var dbLinkType string // 【新增】: 用于存入数据库的类型标识

	var streamSettings map[string]any
	json.Unmarshal([]byte(inbound.StreamSettings), &streamSettings)

                 // --- 1. 确定链接和协议类型 ---
	if security, ok := streamSettings["security"].(string); ok {
		if security == "reality" {
			if network, ok := streamSettings["network"].(string); ok && network == "xhttp" {
				link, err = t.generateXhttpRealityLink(inbound)
				linkType = "VLESS + XHTTP + Reality"
				dbLinkType = "vless_xhttp_reality"
			} else {
				link, err = t.generateRealityLink(inbound)
				linkType = "VLESS + TCP + Reality"
				dbLinkType = "vless_reality"
			}
		} else if security == "tls" {
			link, err = t.generateTlsLink(inbound)
            linkType = "Vless Encryption + XHTTP + TLS" // 协议类型
			dbLinkType = "vless_tls_encryption"
		} else {
			return fmt.Errorf("未知的入站 security 类型: %s", security)
		}
	} else {
		return errors.New("无法解析 streamSettings 中的 security 字段")
	}

	if err != nil {
		return err
	}

	// 尝试生成二维码，如果失败，则 qrCodeBytes 为 nil 或空
	qrCodeBytes, err := qrcode.Encode(link, qrcode.Medium, 256)
	if err != nil {
		logger.Warningf("生成二维码失败，将尝试发送纯文本链接: %v", err)
		qrCodeBytes = nil // 确保 qrCodeBytes 为 nil，用于后续判断
	}

	// --- 2. 获取生成时间 ---
	now := time.Now().Format("2006-01-02 15:04:05")

	// --- 3. 构造包含所有信息并严格遵循格式的描述消息 ---
	baseCaption := fmt.Sprintf(
		"入站备注：\n\n`%s`\n\n用户 Email：\n\n`%s`\n\n协议类型：\n\n`%s`\n\n设备限制：0（无限制）\n\n生成时间：\n\n`%s`",
		inbound.Remark,
		inbound.Remark, // 默认使用 Remark 作为 Email
		linkType,
		now,
	)

	var caption string
	if inFromPanel {
		caption = fmt.Sprintf("✅ **面板【一键配置】入站已创建成功！**\n\n%s\n\n👇 **可点击下方链接直接【复制/导入】** 👇", baseCaption)
	} else {
		caption = fmt.Sprintf("✅ **TG端 远程【一键配置】创建成功！**\n\n%s\n\n👇 **可点击下方链接直接【复制/导入】** 👇", baseCaption)
	}
	// 发送主消息（包含描述和二维码）
	if len(qrCodeBytes) > 0 {
        // 尝试发送图片消息
        photoParams := tu.Photo(
            tu.ID(targetChatId),
            tu.FileFromBytes(qrCodeBytes, "qrcode.png"),
        ).WithCaption(caption).WithParseMode(telego.ModeMarkdown)

        if _, err := bot.SendPhoto(context.Background(), photoParams); err != nil {
            logger.Warningf("发送带二维码的 TG 消息给 %d 失败: %v", targetChatId, err)
            // 如果图片发送失败，回退到发送纯文本描述
            t.SendMsgToTgbot(targetChatId, caption)
        }
    } else {
        // 如果二维码生成失败，直接发送纯文本描述
        t.SendMsgToTgbot(targetChatId, caption)
    }

    // 链接单独发送，不带任何 Markdown 格式。
    // 这将确保 Telegram 客户端可以将其正确识别为可点击的 vless:// 链接。
    t.SendMsgToTgbot(targetChatId, link)

	// 使用正确的类型保存历史记录
	t.saveLinkToHistory(dbLinkType, link)

	return nil
}

// 【新增辅助函数】: 生成 Reality 链接
func (t *Tgbot) generateRealityLink(inbound *model.Inbound) (string, error) {
	var settings map[string]any
	json.Unmarshal([]byte(inbound.Settings), &settings)
	clients, _ := settings["clients"].([]interface{})
	client := clients[0].(map[string]interface{})
	uuid := client["id"].(string)

	var streamSettings map[string]any
	json.Unmarshal([]byte(inbound.StreamSettings), &streamSettings)
	realitySettings := streamSettings["realitySettings"].(map[string]interface{})
	serverNames := realitySettings["serverNames"].([]interface{})
	sni := serverNames[0].(string)
	
	// publicKey 在 realitySettings 下的 settings 子对象中
	settingsMap, ok := realitySettings["settings"].(map[string]interface{})
	if !ok {
		return "", errors.New("realitySettings中缺少settings子对象")
	}
	publicKey, ok := settingsMap["publicKey"].(string)
	if !ok {
		// 再次检查，以防结构有变，但主要依赖 settingsMap
		return "", errors.New("publicKey字段缺失或格式错误 (可能在settings子对象中)")
	}

	shortIdsInterface := realitySettings["shortIds"].([]interface{})
	// 确保 shortIdsInterface 不为空，否则可能 panic
	if len(shortIdsInterface) == 0 {
		return "", errors.New("无法生成 Reality 链接：Short IDs 列表为空")
	}
	sid := shortIdsInterface[common.RandomInt(len(shortIdsInterface))].(string)

	domain, err := t.getDomain()
	if err != nil {
		return "", err
	}

	// ---------------------- URL 编码 ----------------------
	// 必须对查询参数的值（pbk, sni, sid）
	// Go 标准库中的 net/url.QueryEscape 会处理 Base64 字符串中的 + / 等字符。
	escapedPublicKey := url.QueryEscape(publicKey) 
	escapedSni := url.QueryEscape(sni)
	escapedSid := url.QueryEscape(sid)
	escapedRemark := url.QueryEscape(inbound.Remark) 
	
	return fmt.Sprintf("vless://%s@%s:%d?type=tcp&encryption=none&security=reality&pbk=%s&fp=chrome&sni=%s&sid=%s&spx=%%2F&flow=xtls-rprx-vision#%s-%s",
		uuid, domain, inbound.Port, escapedPublicKey, escapedSni, escapedSid, escapedRemark, escapedRemark), nil
}

// 【新增辅助函数】: 生成 TLS 链接
func (t *Tgbot) generateTlsLink(inbound *model.Inbound) (string, error) {
	var settings map[string]any
	json.Unmarshal([]byte(inbound.Settings), &settings)
	clients, _ := settings["clients"].([]interface{})
	client := clients[0].(map[string]interface{})
	uuid := client["id"].(string)
	encryption := settings["encryption"].(string)

	var streamSettings map[string]any
	json.Unmarshal([]byte(inbound.StreamSettings), &streamSettings)
	tlsSettings := streamSettings["tlsSettings"].(map[string]interface{})
	sni := tlsSettings["serverName"].(string)

	domain, err := t.getDomain()
	if err != nil {
		return "", err
	}

	// 链接格式简化，根据您的前端代码，xhttp 未在链接中体现 path
	return fmt.Sprintf("vless://%s@%s:%d?type=tcp&encryption=%s&security=tls&fp=chrome&alpn=http%%2F1.1&sni=%s&flow=xtls-rprx-vision#%s-%s",
		uuid, domain, inbound.Port, encryption, sni, inbound.Remark, inbound.Remark), nil
}

// 生成 VLESS + XHTTP + Reality 链接的函数
func (t *Tgbot) generateXhttpRealityLink(inbound *model.Inbound) (string, error) {
	var settings map[string]any
	json.Unmarshal([]byte(inbound.Settings), &settings)
	clients, _ := settings["clients"].([]interface{})
	client := clients[0].(map[string]interface{})
	uuid := client["id"].(string)

	var streamSettings map[string]any
	json.Unmarshal([]byte(inbound.StreamSettings), &streamSettings)

	realitySettings := streamSettings["realitySettings"].(map[string]interface{})
	serverNames := realitySettings["serverNames"].([]interface{})
	sni := serverNames[0].(string)

	settingsMap, _ := realitySettings["settings"].(map[string]interface{})
	publicKey, _ := settingsMap["publicKey"].(string)

	shortIdsInterface, _ := realitySettings["shortIds"].([]interface{})
	if len(shortIdsInterface) == 0 {
		return "", errors.New("无法生成 Reality 链接：Short IDs 列表为空")
	}
	sid := shortIdsInterface[common.RandomInt(len(shortIdsInterface))].(string)

	xhttpSettings, _ := streamSettings["xhttpSettings"].(map[string]interface{})
	path := xhttpSettings["path"].(string)

	domain, err := t.getDomain()
	if err != nil {
		return "", err
	}

	// 【中文注释】: 对所有URL查询参数进行编码
	escapedPath := url.QueryEscape(path)
	escapedPublicKey := url.QueryEscape(publicKey)
	escapedSni := url.QueryEscape(sni)
	escapedSid := url.QueryEscape(sid)
	escapedRemark := url.QueryEscape(inbound.Remark)

	// 【中文注释】: 严格按照最新格式构建链接
	return fmt.Sprintf("vless://%s@%s:%d?type=xhttp&encryption=none&path=%s&host=&mode=stream-up&security=reality&pbk=%s&fp=chrome&sni=%s&sid=%s&spx=%%2F#%s-%s",
		uuid, domain, inbound.Port, escapedPath, escapedPublicKey, escapedSni, escapedSid, escapedRemark, escapedRemark), nil
}

// 【新增辅助函数】: 发送【订阅转换】安装成功的通知
func (t *Tgbot) SendSubconverterSuccess() {
// func (t *Tgbot) SendSubconverterSuccess(targetChatId int64) { 
	domain, err := t.getDomain()
	if err != nil {
		domain = "[您的面板域名]"
	}

	msgText := fmt.Sprintf(
		"🎉 **恭喜！【订阅转换】模块已成功安装！**\n\n"+
			"您现在可以使用以下地址访问 Web 界面：\n\n"+
			"🔗 **登录地址**: `https://%s:15268`\n\n"+
			"默认用户名: `admin`\n"+
			"默认 密码: `123456`\n\n"+
			"可登录订阅转换后台修改您的密码！",
		domain,
	)
	t.SendMsgToTgbotAdmins(msgText)
	// t.SendMsgToTgbot(targetChatId, msgText)
}

// 【新增辅助函数】: 获取域名（shell 方案）
func (t *Tgbot) getDomain() (string, error) {
	cmd := exec.Command("/usr/local/x-ui/x-ui", "setting", "-getCert", "true")
	output, err := cmd.Output()
	if err != nil {
		return "", errors.New("执行命令获取证书路径失败，请确保已为面板配置 SSL 证书")
	}

	lines := strings.Split(string(output), "\n")
	certLine := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "cert:") {
			certLine = line
			break
		}
	}

	if certLine == "" {
		return "", errors.New("无法从 x-ui 命令输出中找到证书路径")
	}

	certPath := strings.TrimSpace(strings.TrimPrefix(certLine, "cert:"))
	if certPath == "" {
		return "", errors.New("证书路径为空，请确保已为面板配置 SSL 证书")
	}

	domain := filepath.Base(filepath.Dir(certPath))
	return domain, nil
}

// 【新增辅助函数】: 1:1 复刻自 inbounds.html
func (t *Tgbot) generateShortIds() []string {
	chars := "0123456789abcdef"
	lengths := []int{2, 4, 6, 8, 10, 12, 14, 16}
	shortIds := make([]string, len(lengths))
	for i, length := range lengths {
		shortIds[i] = t.randomString(length, chars)
	}
	return shortIds
}

// 【新增辅助函数】: 随机字符串生成器
func (t *Tgbot) randomString(length int, charset string) string {
	bytes := make([]byte, length)
	for i := range bytes {
		randomIndex, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		bytes[i] = charset[randomIndex.Int64()]
	}
	return string(bytes)
}

// 【新增辅助函数】: 保存链接历史到数据库
func (t *Tgbot) saveLinkToHistory(linkType string, link string) {
	history := &database.LinkHistory{
		Type:      linkType,
		Link:      link,
		CreatedAt: time.Now(),
	}
	if err := database.AddLinkHistory(history); err != nil {
		logger.Warningf("保存链接历史到数据库失败: %v", err)
	}
	database.Checkpoint()
}

// 需要的 imports（合并到文件顶部）：
// "fmt"
// "strings"
// th "github.com/mymmrac/telego/telegohandler"
// tu "github.com/mymmrac/telego/telegoutil"
// "github.com/mymmrac/telego"

func (t *Tgbot) handleCallbackQuery(ctx *th.Context, cq telego.CallbackQuery) error {
    // 1) 确保 Message 可访问 —— 注意必须调用 cq.Message.Message() 而不是直接访问 .Message
    if cq.Message == nil || cq.Message.Message == nil {
        _ = ctx.Bot().AnswerCallbackQuery(ctx, tu.CallbackQuery(cq.ID).WithText("消息对象不存在"))
        return nil
    }

    // 关键修正：这里要调用方法 Message()
    msg := cq.Message.Message()   // <- 调用方法，返回 *telego.Message
    // 现在 msg 是 *telego.Message，可以访问 Chat / MessageID
    chatIDInt64 := msg.Chat.ID
    messageID := msg.MessageID

    // 解码回调数据（沿用你已有函数）
    data, err := t.decodeQuery(cq.Data)
    if err != nil {
        _ = ctx.Bot().AnswerCallbackQuery(ctx, tu.CallbackQuery(cq.ID).WithText("回调数据解析失败"))
        return nil
    }

    // 移除内联键盘（telegoutil 构造 params）
    if _, err := ctx.Bot().EditMessageReplyMarkup(ctx, tu.EditMessageReplyMarkup(tu.ID(chatIDInt64), messageID, nil)); err != nil {
        logger.Warningf("TG Bot: 移除内联键盘失败: %v", err)
    }

    // ---------- oneclick_ 分支 ----------
    if strings.HasPrefix(data, "oneclick_") {
        configType := strings.TrimPrefix(data, "oneclick_")

        var creationMessage string
        switch configType {
        case "reality":
            creationMessage = "🚀 Vless + TCP + Reality + Vision"
        case "xhttp_reality":
            creationMessage = "⚡ Vless + XHTTP + Reality"
        case "tls":
            creationMessage = "🛡️ Vless Encryption + XHTTP + TLS"
		case "switch_vision": // 【新增】: 为占位按钮提供单独的提示
			t.SendMsgToTgbot(chatIDInt64, "此协议组合的功能还在开发中 ............")
			_ = ctx.Bot().AnswerCallbackQuery(ctx, tu.CallbackQuery(cq.ID).WithText("开发中..."))
			return nil
        default:
            creationMessage = strings.ToUpper(configType)
        }

        // 注意：不要把无返回值函数当作表达式使用，直接调用即可
        t.SendMsgToTgbot(chatIDInt64, fmt.Sprintf("🛠️ 正在为您远程创建 %s 配置，请稍候...", creationMessage))
        t.remoteCreateOneClickInbound(configType, chatIDInt64)

        _ = ctx.Bot().AnswerCallbackQuery(ctx, tu.CallbackQuery(cq.ID).WithText("配置已创建，请查收管理员私信。"))
        return nil
    }

    // ---------- confirm_sub_install 分支 ----------
    if data == "confirm_sub_install" {
        t.SendMsgToTgbot(chatIDInt64, "🛠️ **已接收到订阅转换安装指令，** 后台正在异步执行...")

        if err := t.serverService.InstallSubconverter(); err != nil {
            // 直接调用发送函数（无返回值）
            t.SendMsgToTgbot(chatIDInt64, fmt.Sprintf("❌ **安装指令启动失败：**\n`%v`", err))
        } else {
            t.SendMsgToTgbot(chatIDInt64, "✅ **安装指令已成功发送到后台。**\n\n请等待安装完成的管理员通知。")
        }

        _ = ctx.Bot().AnswerCallbackQuery(ctx, tu.CallbackQuery(cq.ID))
        return nil
    }

    // 默认回答，避免用户界面卡住
    _ = ctx.Bot().AnswerCallbackQuery(ctx, tu.CallbackQuery(cq.ID).WithText("操作已完成。"))
    return nil
}

// 新增一个公共方法 (大写 G) 来包装私有方法
func (t *Tgbot) GetDomain() (string, error) {
    return t.getDomain()
}

// openPortWithUFW 检查/安装 ufw 并放行指定的端口
func (t *Tgbot) openPortWithUFW(port int) error {
	// 将 Shell 逻辑整合为一个可执行的命令，并使用 /bin/bash -c 执行
	shellCommand := fmt.Sprintf(`
	PORT_TO_OPEN=%d
	
	echo "正在为入站配置自动检查并放行端口 $PORT_TO_OPEN"

	# 1. 检查/安装 ufw
	if ! command -v ufw &>/dev/null; then
		echo "ufw 防火墙未安装，正在安装..."
		# 使用绝对路径执行 apt-get，避免 PATH 问题
		DEBIAN_FRONTEND=noninteractive /usr/bin/apt-get update -qq >/dev/null
		DEBIAN_FRONTEND=noninteractive /usr/bin/apt-get install -y ufw
		if [ $? -ne 0 ]; then echo "❌ ufw 安装失败。"; exit 1; fi
	fi

	# 2. 放行端口
	echo "正在执行 ufw allow $PORT_TO_OPEN..."
	ufw allow $PORT_TO_OPEN
	if [ $? -ne 0 ]; then echo "❌ ufw 端口放行失败。"; exit 1; fi

	# 3. 检查/激活防火墙
	if ! ufw status | grep -q "Status: active"; then
		echo "ufw 状态：未激活。正在尝试激活..."
		ufw --force enable
		if [ $? -ne 0 ]; then echo "❌ ufw 激活失败。"; exit 1; fi
	fi
	echo "✅ 端口 $PORT_TO_OPEN 已成功放行/检查。"
	`, port) // 仅需传递一次 port 参数给 Shell 变量定义

	// 使用 exec.CommandContext 运行命令
	cmd := exec.CommandContext(context.Background(), "/bin/bash", "-c", shellCommand)
	
	// 捕获命令的输出
	output, err := cmd.CombinedOutput()
	
	// 记录日志，以便诊断
	logOutput := string(output)
	logger.Infof("执行 ufw 端口放行命令（端口 %d）结果：\n%s", port, logOutput)

	if err != nil {
		// 返回详细的错误信息，包括 Shell 脚本的输出
		return fmt.Errorf("执行 ufw 端口放行脚本失败: %v, Shell 输出: %s", err, logOutput)
	}
    return nil
}


// =========================================================================================
// 【核心数据结构：XML 解析专用】
// =========================================================================================

// 〔中文注释〕: 内部通用的新闻数据结构，用于避免类型不匹配错误。
type NewsItem struct{
    Title string
    Description string // 用于链接或 GitHub 描述
}

// 用于解析 Google News 或通用 RSS 格式
type RssFeed struct {
	XMLName xml.Name `xml:"rss"`
	Channel RssChannel `xml:"channel"`
}

type RssChannel struct {
	Title string `xml:"title"`
	Items []RssItem `xml:"item"`
}

type RssItem struct {
	Title string `xml:"title"`
	Link  string `xml:"link"`
}

// 用于解析 YouTube 官方 Atom Feed 格式
type AtomFeed struct {
	XMLName xml.Name `xml:"feed"`
	Entries []AtomEntry `xml:"entry"`
}

type AtomEntry struct {
	Title string `xml:"title"`
	Link  struct {
		Href string `xml:"href,attr"`
	} `xml:"link"`
}

// 〔中文注释〕: 内部辅助函数：生成一个安全的随机数。
func safeRandomInt(max int) int {
	if max <= 0 { return 0 }
	result, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return time.Now().Nanosecond() % max
	}
	return int(result.Int64())
}

// =========================================================================================
// 【辅助函数：每日一语】 (最终修复：严格遵循官方文档 Token 机制，增强健壮性)
// =========================================================================================

// 〔中文注释〕: 辅助函数：获取完整的古诗词。严格遵循官方 Token 文档，确保稳定性。
func (t *Tgbot) getDailyVerse() (string, error) {
    client := &http.Client{Timeout: 8 * time.Second}
    
    // 1. 获取 Token
    tokenResp, err := client.Get("https://v2.jinrishici.com/token")
    if err != nil {
        return "", fmt.Errorf("步骤 1: 请求 Token API 失败: %v", err)
    }
    defer tokenResp.Body.Close()

    tokenBody, err := ioutil.ReadAll(tokenResp.Body)
    if err != nil {
        return "", fmt.Errorf("步骤 1: 读取 Token 响应失败: %v", err)
    }
    
    var tokenResult struct {
        Status string `json:"status"`
        Token  string `json:"data"`
    }

    if json.Unmarshal(tokenBody, &tokenResult) != nil || tokenResult.Status != "success" || tokenResult.Token == "" {
        return "", fmt.Errorf("步骤 1: 解析 Token JSON 失败或状态异常: %s", string(tokenBody))
    }
    
    // 2. 使用 Token 获取诗句
    sentenceURL := "https://v2.jinrishici.com/sentence" // 简化 URL
    req, err := http.NewRequest("GET", sentenceURL, nil)
    if err != nil {
        return "", fmt.Errorf("步骤 2: 创建请求失败: %v", err)
    }
    // 严格按照文档，将 Token 放在 X-User-Token Header 中
    req.Header.Add("X-User-Token", tokenResult.Token)
    // 增加 User-Agent 伪装成浏览器请求
    req.Header.Add("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
    
    sentenceResp, err := client.Do(req)
    if err != nil {
        return "", fmt.Errorf("步骤 2: 请求诗句 API 失败: %v", err)
    }
    defer sentenceResp.Body.Close()

    sentenceBody, err := ioutil.ReadAll(sentenceResp.Body)
    if err != nil {
        return "", fmt.Errorf("步骤 2: 读取诗句响应失败: %v", err)
    }

    var result struct {
        Status string `json:"status"`
        Data struct {
            Content string `json:"content"`
            Origin  struct {
                Title   string `json:"title"`
                Author  string `json:"author"`
            } `json:"origin"`
        } `json:"data"`
    }

    if json.Unmarshal(sentenceBody, &result) != nil || result.Status != "success" || result.Data.Content == "" {
        // 如果失败，记录完整的 JSON 响应，便于调试
        return "", fmt.Errorf("步骤 2: 解析诗句 JSON 失败或内容为空。返回状态码: %d, 响应体: %s", sentenceResp.StatusCode, string(sentenceBody))
    }

    poemContent := strings.ReplaceAll(result.Data.Content, "，", "，\n")
    return fmt.Sprintf("📜 **【每日一语】**\n\n%s\n\n`—— %s ·《%s》`", poemContent, result.Data.Origin.Author, result.Data.Origin.Title), nil
}


// =========================================================================================
// 【辅助函数：图片发送】 (保持稳定版本)
// =========================================================================================

// 〔中文注释〕: 【最终重构】图片发送函数：按优先级尝试3个不同的图片源。
func (t *Tgbot) sendRandomImageWithFallback() {
    var imageURL string
    var sourceName string
    var found bool // 使用 flag 替代 goto

    // --- 来源 1: 动漫图 (waifu.pics) ---
    if !found {
        sourceName = "waifu.pics"
        apiURL := "https://api.waifu.pics/sfw/waifu"
        if resp, e := http.Get(apiURL); e == nil {
            defer resp.Body.Close()
            if body, e := ioutil.ReadAll(resp.Body); e == nil {
                var res struct { URL string `json:"url"` }
                if json.Unmarshal(body, &res) == nil && res.URL != "" {
                    imageURL = res.URL
                    found = true
                }
            }
        }
        if !found { logger.Warningf("图片来源 1 (%s) 失败", sourceName) }
    }

    // --- 来源 2: 风景/自然图 (Picsum Photos) ---
    if !found {
        sourceName = "Picsum Photos"
        // 〔中文注释〕: 获取前10页的图片列表，随机选择一张。
        apiURL := fmt.Sprintf("https://picsum.photos/v2/list?page=%d&limit=100", safeRandomInt(10)+1)
        if resp, e := http.Get(apiURL); e == nil {
            defer resp.Body.Close()
            if body, e := ioutil.ReadAll(resp.Body); e == nil {
                var list []struct { ID string `json:"id"` }
                if json.Unmarshal(body, &list) == nil && len(list) > 0 {
                    randomIndex := safeRandomInt(len(list))
                    imageURL = fmt.Sprintf("https://picsum.photos/id/%s/1024/768", list[randomIndex].ID)
                    found = true
                }
            }
        }
        if !found { logger.Warningf("图片来源 2 (%s) 失败", sourceName) }
    }

    // --- 来源 3: 每日高清大图 (Bing redirect) ---
    if !found {
        sourceName = "Bing 每日图片"
        apiURL := "https://api.adicw.cn/api/images/bing" 
        if resp, e := http.Get(apiURL); e == nil {
            // 〔中文注释〕: 这个 API 会直接重定向到图片URL
            if resp.Request.URL.String() != apiURL {
                imageURL = resp.Request.URL.String()
                found = true
            }
        }
        if !found { logger.Warningf("图片来源 3 (%s) 失败", sourceName) }
    }

    if !found {
        logger.Warning("所有图片来源均失败，跳过图片发送。")
        return
    }

    // --- SEND_IMAGE 逻辑 ---
    for _, adminId := range adminIds {
        photo := tu.Photo(
            tu.ID(adminId),
            tu.FileFromURL(imageURL),
        ).WithCaption(fmt.Sprintf("🎨 **【今日美图】**\n（来源：%s）", sourceName))

        _, err := bot.SendPhoto(context.Background(), photo)
        if err != nil {
            logger.Warningf("发送图片给管理员 %d 失败: %v", adminId, err)
        }
        time.Sleep(300 * time.Millisecond)
    }
}


// =========================================================================================
// 【辅助函数：新闻资讯核心抓取逻辑】 (最终稳定版：支持 XML/RSS 直连解析)
// =========================================================================================

// 〔中文注释〕: 辅助函数：核心逻辑，从给定的 API 获取新闻简报或视频列表。
func fetchNewsFromGlobalAPI(apiURL string, sourceName string, limit int) (string, error) {
    client := &http.Client{Timeout: 15 * time.Second} 
    
    // 对于 YouTube 和 Google News 源，直接请求原始 URL，不经过 rss2json 桥接服务
    var directURL string
    isXMLSource := strings.Contains(sourceName, "YouTube") || strings.Contains(sourceName, "Google News")
    
    // 初始化 newsItems 列表
    var newsItems []NewsItem 
    var err error

    if isXMLSource {
        // --- XML/RSS 直连解析逻辑 ---
        
        // 1. 从 apiURL 中提取真正的 RSS/Atom URL
        // apiURL 格式示例：https://api.rss2json.com/v1/api.json?rss_url=...
        directURL = apiURL // 默认为整个 apiURL，以防解析失败

        if u, parseErr := url.Parse(apiURL); parseErr == nil {
            if rssURL := u.Query().Get("rss_url"); rssURL != "" {
                directURL = rssURL // 提取原始 RSS URL
            }
        }
        
        // 2. 发起 HTTP 请求，并伪装 User-Agent
        req, reqErr := http.NewRequest("GET", directURL, nil)
        if reqErr != nil { return "", fmt.Errorf("创建请求失败: %v", reqErr) }
        req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
        
        resp, respErr := client.Do(req)
        if respErr != nil {
            return "", fmt.Errorf("请求 %s XML API 失败: %v", sourceName, respErr)
        }
        defer resp.Body.Close()

        if resp.StatusCode != http.StatusOK {
            return "", fmt.Errorf("请求 %s XML API 返回非 200 状态码: %d", sourceName, resp.StatusCode)
        }
        
        body, readErr := ioutil.ReadAll(resp.Body)
        if readErr != nil { return "", fmt.Errorf("读取 %s 响应失败: %v", sourceName, readErr) }

        
        // 3. XML 解析
        if strings.Contains(sourceName, "YouTube") {
            // 解析 YouTube Atom Feed 格式
            var atomFeed AtomFeed
            if xmlErr := xml.Unmarshal(body, &atomFeed); xmlErr == nil && len(atomFeed.Entries) > 0 {
                for _, entry := range atomFeed.Entries {
                    newsItems = append(newsItems, NewsItem{
                        Title: entry.Title,
                        Description: entry.Link.Href,
                    })
                }
            } else {
                err = fmt.Errorf("解析 YouTube Atom XML 失败: %v", xmlErr)
            }
        } else if strings.Contains(sourceName, "Google News") {
            // 解析 Google News RSS 格式
            var rssFeed RssFeed
            if xmlErr := xml.Unmarshal(body, &rssFeed); xmlErr == nil && len(rssFeed.Channel.Items) > 0 {
                 for _, item := range rssFeed.Channel.Items {
                    newsItems = append(newsItems, NewsItem{
                        Title: item.Title,
                        Description: item.Link,
                    })
                }
            } else {
                err = fmt.Errorf("解析 Google News RSS XML 失败: %v", xmlErr)
            }
        }
        
    } else {
        // --- JSON API 访问（CoinMarketCap） ---
        resp, respErr := client.Get(apiURL)
        if respErr != nil {
            return "", fmt.Errorf("请求 %s API 失败: %v", sourceName, respErr)
        }
        defer resp.Body.Close()

        if resp.StatusCode != http.StatusOK {
            return "", fmt.Errorf("请求 %s API 返回非 200 状态码: %d", sourceName, resp.StatusCode)
        }

        body, readErr := ioutil.ReadAll(resp.Body)
        if readErr != nil { return "", fmt.Errorf("读取 %s 响应失败: %v", sourceName, readErr) }
        
        // 适配 CoinMarketCap 格式
        if sourceName == "币圈头条" {
            var result struct { Articles []struct { Title string `json:"title"` } `json:"articles"` }
            if jsonErr := json.Unmarshal(body, &result); jsonErr == nil {
                 for _, item := range result.Articles { 
                     newsItems = append(newsItems, NewsItem{Title: item.Title}) 
                 }
            } else {
                 err = fmt.Errorf("解析 CoinMarketCap JSON 失败: %v", jsonErr)
            }
        } else if strings.Contains(sourceName, "GitHub") { 
            // 如果您决定保留 GitHub 源，这是它的解析逻辑
             var result []struct {
                RepoName string `json:"repo_name"`
                Desc string `json:"desc"`
            }
            if jsonErr := json.Unmarshal(body, &result); jsonErr == nil {
                for _, item := range result {
                    newsItems = append(newsItems, NewsItem{
                        Title: fmt.Sprintf("⭐ %s", item.RepoName),
                        Description: item.Desc,
                    })
                }
            } else {
                 err = fmt.Errorf("解析 GitHub Trending JSON 失败: %v", jsonErr)
            }
        }
    }
    
    if err != nil {
        return "", err // 如果在解析阶段失败，直接返回错误
    }

    if len(newsItems) == 0 {
        return "", errors.New(sourceName + " 简报内容为空")
    }
    
    // --- 最终消息构建 ---
    var builder strings.Builder
    builder.WriteString(fmt.Sprintf("📰 **【%s 简报】**\n", sourceName))

    for i, item := range newsItems {
        if i >= limit { break }
        if item.Title != "" {
            // 移除 RSS 源标题中可能包含的来源信息，让内容更整洁
            cleanTitle := strings.ReplaceAll(item.Title, " - YouTube", "")
            cleanTitle = strings.ReplaceAll(cleanTitle, " | Google News", "")
            // 移除 HTML 标签（RSS/Atom Title中常见）
            cleanTitle = regexp.MustCompile("<[^>]*>").ReplaceAllString(cleanTitle, "")

            builder.WriteString(fmt.Sprintf("\n%d. %s", i+1, cleanTitle))
            // 链接/描述只有在特定源时才显示
            if item.Description != "" && (strings.Contains(sourceName, "YouTube") || strings.Contains(sourceName, "Google News") || strings.Contains(sourceName, "GitHub")) {
                 builder.WriteString(fmt.Sprintf("\n  `%s`", item.Description))
            }
        }
    }

    return builder.String(), nil
}

// =========================================================================================
// 【核心函数：getNewsBriefingWithFallback】 (最终稳定版，随机+冗余尝试)
// =========================================================================================

// 〔中文注释〕: 【最终重构】新闻资讯获取函数：随机排列源并逐个尝试，直到成功或全部失败。
func (t *Tgbot) getNewsBriefingWithFallback() (string, error) {
	// 将 Google News 的 URL 计算移到数组定义外部
    rssQuery2 := url.QueryEscape("AI 科技 国际时事 区块链 IT AI绘画") 
    // 使用 hl=zh-CN (Host Language) 和 gl=CN (Geo-Location)
    rssURL2 := fmt.Sprintf("https://news.google.com/rss/search?q=%s&hl=zh-CN&gl=CN", rssQuery2) 
    // 定义所有可用的新闻源
    newsSources := []struct {
        Name string
        API  string
    }{
        {
            Name: "YouTube 中文热搜 (AI/IT/旅游)",
            API:  fmt.Sprintf("https://api.rss2json.com/v1/api.json?rss_url=%s&count=5", url.QueryEscape(fmt.Sprintf("https://www.youtube.com/feeds/videos.xml?search_query=%s", url.QueryEscape("AI 绘画 IT 旅游 中文")))),
        },
        {
            Name: "Google News 中文简报",
			API:  fmt.Sprintf("https://api.rss2json.com/v1/api.json?rss_url=%s&count=5", url.QueryEscape(rssURL2)),
        },
        {
            Name: "币圈头条",
            API:  "https://api.coinmarketcap.cn/v1/news/headlines?limit=5",
        },
    }

    // 解决 rand.Shuffle 兼容性问题：手动实现 Fisher-Yates 洗牌算法
    sourceCount := len(newsSources)
    
    // 使用 rng (别名) 来调用 math/rand 中的函数
    r := rng.New(rng.NewSource(time.Now().UnixNano()))

    // 执行洗牌
    for i := sourceCount - 1; i > 0; i-- {
        // 在 [0, i] 范围内随机选择一个索引
        j := r.Intn(i + 1) 
        // 交换元素
        newsSources[i], newsSources[j] = newsSources[j], newsSources[i]
    }
    
    // 逐个尝试所有来源，直到成功
    for i, source := range newsSources {
        logger.Infof("新闻资讯：开始尝试来源 (随机顺序 [%d/%d]): %s", i+1, len(newsSources), source.Name)
        
        // 调用核心抓取逻辑
        newsMsg, err := fetchNewsFromGlobalAPI(source.API, source.Name, 5)
        
        if err == nil && newsMsg != "" { 
            // 成功获取到内容
            logger.Infof("新闻资讯：来源 [%s] 成功获取内容。", source.Name)
            return newsMsg, nil 
        }
        
        // 失败，记录警告，继续尝试下一个
        logger.Warningf("新闻资讯来源 [%s] 尝试失败: %v", source.Name, err)
    }
    
    // 所有来源都失败，返回空字符串和 nil error，以确保不中断 SendReport 流程
    return "", nil 
}
