package web

import (
	"context"
	"crypto/tls"
	"embed"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"x-ui/config"
	"x-ui/logger"
	"x-ui/util/common"
	"x-ui/web/controller"
	"x-ui/web/job"
	"x-ui/web/locale"
	"x-ui/web/middleware"
	"x-ui/web/network"
	"x-ui/web/service"

	"github.com/gin-contrib/gzip"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"
)

//go:embed assets/*
var assetsFS embed.FS

//go:embed html/*
var htmlFS embed.FS

//go:embed translation/*
var i18nFS embed.FS

var startTime = time.Now()

type wrapAssetsFS struct {
	embed.FS
}

// Keep-Alive 监听器包装器：用于拦截新连接并设置 Keep-Alive 选项
type keepAliveListener struct {
	*net.TCPListener
	KeepAlivePeriod time.Duration
}

// Accept 方法：拦截连接并设置 Keep-Alive
func (l keepAliveListener) Accept() (net.Conn, error) {
	// 1. 接受底层 TCP 连接
	tc, err := l.TCPListener.AcceptTCP()
	if err != nil {
		return nil, err
	}
	
	// 2. 在 *net.TCPConn 上设置 Keep-Alive 属性 (这里的方法是正确的)
	if err := tc.SetKeepAlive(true); err != nil {
		logger.Warning("Failed to set KeepAlive:", err)
	}
	// 设置心跳包周期为 5 秒
	if err := tc.SetKeepAlivePeriod(l.KeepAlivePeriod); err != nil {
		logger.Warning("Failed to set KeepAlivePeriod:", err)
	}
	
	return tc, nil
}

func (f *wrapAssetsFS) Open(name string) (fs.File, error) {
	file, err := f.FS.Open("assets/" + name)
	if err != nil {
		return nil, err
	}
	return &wrapAssetsFile{
		File: file,
	}, nil
}

type wrapAssetsFile struct {
	fs.File
}

func (f *wrapAssetsFile) Stat() (fs.FileInfo, error) {
	info, err := f.File.Stat()
	if err != nil {
		return nil, err
	}
	return &wrapAssetsFileInfo{
		FileInfo: info,
	}, nil
}

type wrapAssetsFileInfo struct {
	fs.FileInfo
}

func (f *wrapAssetsFileInfo) ModTime() time.Time {
	return startTime
}

type Server struct {
	httpServer *http.Server
	listener   net.Listener

	index  *controller.IndexController
	server *controller.ServerController
	panel  *controller.XUIController
	api    *controller.APIController

	xrayService    service.XrayService
	settingService service.SettingService
	tgbotService    service.TelegramService
	// 〔中文注释〕: 添加这个字段，用来“持有”从 main.go 传递过来的 serverService 实例。
	serverService  service.ServerService

	cron *cron.Cron

	ctx    context.Context
	cancel context.CancelFunc
}

// 【新增方法】：用于 main.go 将创建好的 tgBotService 注入进来
func (s *Server) SetTelegramService(tgService service.TelegramService) {
    s.tgbotService = tgService
}

// 〔中文注释〕: 1. 让 NewServer 能够接收一个 serverService 实例作为参数。
// 【修改】: 增加 xrayService 和 settingService 作为参数
func NewServer(
	serverService service.ServerService,
	xrayService service.XrayService,
	settingService service.SettingService,
) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		ctx:    ctx,
		cancel: cancel,
		// 〔中文注释〕: 2. 将传入的 serverService 存储到 Server 结构体的字段中。
		// 【修改】: 同时初始化所有接收到的服务
		serverService:  serverService,
		xrayService:    xrayService,
		settingService: settingService,
	}
}

func (s *Server) getHtmlFiles() ([]string, error) {
	files := make([]string, 0)
	dir, _ := os.Getwd()
	err := fs.WalkDir(os.DirFS(dir), "web/html", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func (s *Server) getHtmlTemplate(funcMap template.FuncMap) (*template.Template, error) {
	t := template.New("").Funcs(funcMap)
	err := fs.WalkDir(htmlFS, "html", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			newT, err := t.ParseFS(htmlFS, path+"/*.html")
			if err != nil {
				// ignore
				return nil
			}
			t = newT
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (s *Server) initRouter() (*gin.Engine, error) {
	if config.IsDebug() {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.DefaultWriter = io.Discard
		gin.DefaultErrorWriter = io.Discard
		gin.SetMode(gin.ReleaseMode)
	}

	engine := gin.Default()

	webDomain, err := s.settingService.GetWebDomain()
	if err != nil {
		return nil, err
	}

	if webDomain != "" {
		engine.Use(middleware.DomainValidatorMiddleware(webDomain))
	}

	secret, err := s.settingService.GetSecret()
	if err != nil {
		return nil, err
	}

	basePath, err := s.settingService.GetBasePath()
	if err != nil {
		return nil, err
	}
	engine.Use(gzip.Gzip(gzip.DefaultCompression, gzip.WithExcludedPaths([]string{basePath + "panel/api/"})))
	assetsBasePath := basePath + "assets/"

	store := cookie.NewStore(secret)
	engine.Use(sessions.Sessions("3x-ui", store))
	engine.Use(func(c *gin.Context) {
		c.Set("base_path", basePath)
	})
	engine.Use(func(c *gin.Context) {
		uri := c.Request.RequestURI
		if strings.HasPrefix(uri, assetsBasePath) {
			c.Header("Cache-Control", "max-age=31536000")
		}
	})

	// init i18n
	err = locale.InitLocalizer(i18nFS, &s.settingService)
	if err != nil {
		return nil, err
	}

	// Apply locale middleware for i18n
	i18nWebFunc := func(key string, params ...string) string {
		return locale.I18n(locale.Web, key, params...)
	}
	engine.FuncMap["i18n"] = i18nWebFunc
	engine.Use(locale.LocalizerMiddleware())

	// set static files and template
	if config.IsDebug() {
		// for development
		files, err := s.getHtmlFiles()
		if err != nil {
			return nil, err
		}
		engine.LoadHTMLFiles(files...)
		engine.StaticFS(basePath+"assets", http.FS(os.DirFS("web/assets")))
	} else {
		// for production
		template, err := s.getHtmlTemplate(engine.FuncMap)
		if err != nil {
			return nil, err
		}
		engine.SetHTMLTemplate(template)
		engine.StaticFS(basePath+"assets", http.FS(&wrapAssetsFS{FS: assetsFS}))
	}

	// Apply the redirect middleware (`/xui` to `/panel`)
	engine.Use(middleware.RedirectMiddleware(basePath))

	g := engine.Group(basePath)

	s.index = controller.NewIndexController(g)
	// 〔中文注释〕: 调用我们刚刚改造过的 NewServerController，并将 s.serverService 作为参数传进去。
	s.server = controller.NewServerController(g, s.serverService)
	s.panel = controller.NewXUIController(g)
	s.api = controller.NewAPIController(g)

	return engine, nil
}

func (s *Server) startTask() {
	err := s.xrayService.RestartXray(true)
	if err != nil {
		logger.Warning("start xray failed:", err)
	}
	// Check whether xray is running every second
	s.cron.AddJob("@every 1s", job.NewCheckXrayRunningJob())

	// Check if xray needs to be restarted every 30 seconds
	s.cron.AddFunc("@every 30s", func() {
		if s.xrayService.IsNeedRestartAndSetFalse() {
			err := s.xrayService.RestartXray(false)
			if err != nil {
				logger.Error("restart xray failed:", err)
			}
		}
	})

	go func() {
		time.Sleep(time.Second * 5)
		// Statistics every 10 seconds, start the delay for 5 seconds for the first time, and staggered with the time to restart xray
		s.cron.AddJob("@every 10s", job.NewXrayTrafficJob())
	}()

	// check client ips from log file every 10 sec
	s.cron.AddJob("@every 10s", job.NewCheckClientIpJob())

	// check client ips from log file every day
	s.cron.AddJob("@daily", job.NewClearLogsJob())

	// Make a traffic condition every day, 8:30
	var entry cron.EntryID
	isTgbotenabled, err := s.settingService.GetTgbotEnabled()
	if (err == nil) && (isTgbotenabled) {
		runtime, err := s.settingService.GetTgbotRuntime()
		if err != nil || runtime == "" {
			logger.Errorf("Add NewStatsNotifyJob error[%s], Runtime[%s] invalid, will run default", err, runtime)
			runtime = "@daily"
		}
		logger.Infof("Tg notify enabled,run at %s", runtime)

		// 【中文注释】在注册每日任务时增加运行时检查，防止 Bot 未启动导致中断。
		// ======================================================
		_, err = s.cron.AddFunc(runtime, func() {
			// 【中文注释】: 若 bot 尚未初始化或未运行，则跳过
			if s.tgbotService == nil {
				logger.Warning("StatsNotifyJob: tgbotService 为 nil，跳过执行。")
				return
			}
			if bot, ok := s.tgbotService.(*service.Tgbot); ok {
				if !bot.IsRunning() {
					logger.Warning("StatsNotifyJob: TG Bot 尚未运行，跳过执行。")
					return
				}
			}

			// 【中文注释】: 捕获 panic，避免单次错误影响 cron 运行
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf("StatsNotifyJob panic: %v", r)
				}
			}()

			// 【中文注释】: 调用原有每日报告任务
			job.NewStatsNotifyJob().Run()
		})
		if err != nil {
			logger.Warning("Add NewStatsNotifyJob error", err)
			return
		}

		// check for Telegram bot callback query hash storage reset
		s.cron.AddJob("@every 2m", job.NewCheckHashStorageJob())

		// Check CPU load and alarm to TgBot if threshold passes
		cpuThreshold, err := s.settingService.GetTgCpu()
		if (err == nil) && (cpuThreshold > 0) {
			s.cron.AddJob("@every 10s", job.NewCheckCpuJob())
		}
	} else {
		s.cron.Remove(entry)
	}
}

func (s *Server) Start() (err error) {
	// This is an anonymous function, no function name
	defer func() {
		if err != nil {
			s.Stop()
		}
	}()

	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return err
	}
	s.cron = cron.New(cron.WithLocation(loc), cron.WithSeconds())
	s.cron.Start()

	engine, err := s.initRouter()
	if err != nil {
		return err
	}

	certFile, err := s.settingService.GetCertFile()
	if err != nil {
		return err
	}
	keyFile, err := s.settingService.GetKeyFile()
	if err != nil {
		return err
	}
	listen, err := s.settingService.GetListen()
	if err != nil {
		return err
	}
	port, err := s.settingService.GetPort()
	if err != nil {
		return err
	}
	listenAddr := net.JoinHostPort(listen, strconv.Itoa(port))

    // 1. 使用 baseListener 临时变量接收 net.Listen 的结果
	baseListener, err := net.Listen("tcp", listenAddr) 
	if err != nil {
		return err
	}

    var listener net.Listener

	// 2. 将 net.Listener 断言为 *net.TCPListener，以便包装
	tcpListener, ok := baseListener.(*net.TCPListener)
	if !ok {
        // 如果不是 TCPListener，则使用原生的，不设置 Keep-Alive
		logger.Warning("Listener is not a TCPListener, cannot set KeepAlive.")
        listener = baseListener
	} else {
        // 3. 【核心修正】：使用包装器设置 Keep-Alive 属性给每个新连接
        kaListener := &keepAliveListener{
            TCPListener: tcpListener,
            KeepAlivePeriod: 5 * time.Second, // 设置为 5 秒
        }
        listener = net.Listener(kaListener) // 将包装器赋值给最终的 listener
	}
	
	if certFile != "" || keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err == nil {
			c := &tls.Config{
				Certificates: []tls.Certificate{cert},
			}
			listener = network.NewAutoHttpsListener(listener)
			listener = tls.NewListener(listener, c)
			logger.Info("Web server running HTTPS on", listener.Addr())
		} else {
			logger.Error("Error loading certificates:", err)
			logger.Info("Web server running HTTP on", listener.Addr())
		}
	} else {
		logger.Info("Web server running HTTP on", listener.Addr())
	}
	s.listener = listener

	// 【核心修正位置】：修改 s.httpServer 的初始化代码
	s.httpServer = &http.Server{
		Handler: engine,
		// 【新增】：设置 120 秒的读写超时，确保 ufw 命令有足够的时间完成
		ReadTimeout:  120 * time.Second, 
		WriteTimeout: 120 * time.Second, 
	}

	go func() {
		s.httpServer.Serve(listener)
	}()

    // 启动 TG Bot
    isTgbotenabled, err := s.settingService.GetTgbotEnabled()
	if (err == nil) && (isTgbotenabled) {
        // 现在直接在注入的实例上调用 Start 方法，而不是 NewTgbot()
        // 因为 main.go 已经注入了完整的实例
		if tgbot, ok := s.tgbotService.(*service.Tgbot); ok {
			logger.Info("启动 Telegram Bot（在注册定时任务之前）...")
			tgbot.Start(i18nFS)
		} else {
			logger.Warning("Telegram Bot 已启用，但注入的实例类型不正确或为 nil，无法启动。")
		}
	} else {
		logger.Infof("Telegram Bot 未启用或读取设置失败: %v", err)
	}
	
	s.startTask()

    return nil
}

func (s *Server) Stop() error {
	s.cancel()
	s.xrayService.StopXray()
	if s.cron != nil {
		s.cron.Stop()
	}
	// 只有在断言成功后，才能调用只在 *service.Tgbot 上定义的 Stop() 和 IsRunning() 方法。
	if tgBot, ok := s.tgbotService.(*service.Tgbot); ok {
		if tgBot.IsRunning() {
			tgBot.Stop()
		}
	}
	var err1 error
	var err2 error
	if s.httpServer != nil {
		err1 = s.httpServer.Shutdown(s.ctx)
	}
	if s.listener != nil {
		err2 = s.listener.Close()
	}
	return common.Combine(err1, err2)
}

func (s *Server) GetCtx() context.Context {
	return s.ctx
}

func (s *Server) GetCron() *cron.Cron {
	return s.cron
}
