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

// 预定义 IPv4 私网和回环网段
var privateIPv4Nets []*net.IPNet

type wrapAssetsFS struct {
	embed.FS
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
	tgbotService   service.Tgbot

	cron *cron.Cron

	ctx    context.Context
	cancel context.CancelFunc
}

func NewServer() *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		ctx:    ctx,
		cancel: cancel,
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
    // 这里用 htmlFS（//go:embed html/*）而不是“templates”
    t := template.New("").Funcs(funcMap)

    // 递归遍历 embed 的 html 目录，解析所有 .html 模板
    err := fs.WalkDir(htmlFS, "html", func(path string, d fs.DirEntry, err error) error {
        if err != nil {
            return err
        }
        if d.IsDir() {
            return nil
        }
        if !strings.HasSuffix(path, ".html") {
            return nil
        }

        // 读出模板内容
        b, err := htmlFS.ReadFile(path)
        if err != nil {
            return err
        }

        // 去掉前缀“html/”，让 {{template "form/inbound"}} 这种名字能被正确找到
        name := strings.TrimPrefix(path, "html/")
        _, err = t.New(name).Parse(string(b))
        return err
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
	s.server = controller.NewServerController(g)
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
		_, err = s.cron.AddJob(runtime, job.NewStatsNotifyJob())
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
	var listenAddr string

	if certFile != "" && keyFile != "" {
		// 方式一：配置了证书，启用 HTTPS
		// 检查证书是否有效，如果无效则直接报错退出，不允许回退到 HTTP
		_, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			logger.Errorf("Error loading certificates, please check the file path and content: %v", err)
			return err
		}
		// 监听用户配置的地址
		listenAddr = net.JoinHostPort(listen, strconv.Itoa(port))
	} else {
		// 方式二：未配置证书，强制监听在本地回环地址，仅供 SSH 转发使用
		logger.Info("No certificate configured. Forcing listen address to localhost for security.")
		logger.Info("Access is only possible via SSH tunnel (e.g., http://127.0.0.1).")
		
		// 无论用户在 listen 中填写什么，都强制使用回环地址
		listen = fallbackToLocalhost(listen)
		listenAddr = net.JoinHostPort(listen, strconv.Itoa(port))
	}

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}

	// 再次检查证书，配置 TLS Listener
	if certFile != "" && keyFile != "" {
		cert, _ := tls.LoadX509KeyPair(certFile, keyFile) // 这里我们忽略错误，因为上面已经检查过了
		c := &tls.Config{
			Certificates: []tls.Certificate{cert},
		}
		listener = network.NewAutoHttpsListener(listener)
		listener = tls.NewListener(listener, c)
		logger.Info("Web server running HTTPS on", listener.Addr())
	} else {
		logger.Info("Web server running HTTP on", listener.Addr())
	}
	s.listener = listener

	s.httpServer = &http.Server{
		Handler: engine,
	}

	go func() {
		s.httpServer.Serve(listener)
	}()

	s.startTask()

	isTgbotenabled, err := s.settingService.GetTgbotEnabled()
	if (err == nil) && (isTgbotenabled) {
		tgBot := s.tgbotService.NewTgbot()
		tgBot.Start(i18nFS)
	}

	return nil
}

func (s *Server) Stop() error {
	s.cancel()
	s.xrayService.StopXray()
	if s.cron != nil {
		s.cron.Stop()
	}
	if s.tgbotService.IsRunning() {
		s.tgbotService.Stop()
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

// isInternalIP 判断是否为私网或回环IP（支持IPv4和IPv6）
func isInternalIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}

	if ip4 := ip.To4(); ip4 != nil {
		// IPv4 判断是否在私网/回环网段内
		for _, privateNet := range privateIPv4Nets {
			if privateNet.Contains(ip4) {
				return true
			}
		}
		return false
	}

	// IPv6 判断回环或链路本地地址
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return true
	}

	// 判断 IPv6 fc00::/7 私网地址段
	if ip[0]&0xfe == 0xfc {
		return true
	}

	return false
}

// fallbackToLocalhost 根据传入地址返回对应的本地回环地址
func fallbackToLocalhost(listen string) string {
	ip := net.ParseIP(listen)
	if ip == nil {
		// 无法解析则默认回退 IPv4 回环
		return "127.0.0.1"
	}
	if ip.To4() != nil {
		// IPv4 回退 IPv4 回环
		return "127.0.0.1"
	}
	// IPv6 回退 IPv6 回环
	return "::1"
}

func init() {
	for _, cidr := range []string{
		"10.0.0.0/8",     // A类私网
		"172.16.0.0/12",  // B类私网
		"192.168.0.0/16", // C类私网
		"100.64.0.0/10",  // CGNAT地址段
		"127.0.0.0/8",    // 回环
	} {
		_, netw, err := net.ParseCIDR(cidr)
		if err == nil {
			privateIPv4Nets = append(privateIPv4Nets, netw)
		}
	}
}
