package controller

import (
	"fmt"
	"net/http"
	"regexp"
	"time"

	"x-ui/web/global"
	"x-ui/web/service"

	"github.com/gin-gonic/gin"
)

var filenameRegex = regexp.MustCompile(`^[a-zA-Z0-9_\-.]+$`)

type ServerController struct {
	BaseController

	serverService  service.ServerService
	settingService service.SettingService

	lastStatus        *service.Status
	lastGetStatusTime time.Time

	lastVersions        []string
	lastGetVersionsTime time.Time
}

// 〔中文注释〕: 1. 在函数参数中，增加 serverService service.ServerService，让它可以接收一个服务实例。
func NewServerController(g *gin.RouterGroup, serverService service.ServerService) *ServerController {
	a := &ServerController{
		lastGetStatusTime: time.Now(),
		// 〔中文注释〕: 2. 将传入的 serverService 赋值给 a.serverService。
		//    这样一来，这个 Controller 内部使用的就是我们在 main.go 中创建的那个功能完整的服务了。
		serverService:  serverService,
	}
	a.initRouter(g)
	a.startTask()
	return a
}

func (a *ServerController) initRouter(g *gin.RouterGroup) {
	g.GET("/status", a.status)
	g.GET("/getXrayVersion", a.getXrayVersion)
	g.GET("/getConfigJson", a.getConfigJson)
	g.GET("/getDb", a.getDb)
	g.GET("/getNewUUID", a.getNewUUID)
	g.GET("/getNewX25519Cert", a.getNewX25519Cert)
	g.GET("/getNewmldsa65", a.getNewmldsa65)
	g.GET("/getNewmlkem768", a.getNewmlkem768)
	g.GET("/getNewVlessEnc", a.getNewVlessEnc)

	g.POST("/stopXrayService", a.stopXrayService)
	g.POST("/restartXrayService", a.restartXrayService)
	g.POST("/installXray/:version", a.installXray)
	g.POST("/updateGeofile", a.updateGeofile)
	g.POST("/updateGeofile/:fileName", a.updateGeofile)
	g.POST("/logs/:count", a.getLogs)
	g.POST("/xraylogs/:count", a.getXrayLogs)
	g.POST("/importDB", a.importDB)
	g.POST("/getNewEchCert", a.getNewEchCert)
	g.POST("/history/save", a.saveHistory)
	g.GET("/history/load", a.loadHistory)
	g.POST("/install/subconverter", a.installSubconverter)
	g.POST("/openPort", a.openPort)
}

func (a *ServerController) refreshStatus() {
	a.lastStatus = a.serverService.GetStatus(a.lastStatus)
}

func (a *ServerController) startTask() {
	webServer := global.GetWebServer()
	c := webServer.GetCron()
	c.AddFunc("@every 2s", func() {
		now := time.Now()
		if now.Sub(a.lastGetStatusTime) > time.Minute*3 {
			return
		}
		a.refreshStatus()
	})
}

func (a *ServerController) status(c *gin.Context) {
	a.lastGetStatusTime = time.Now()

	jsonObj(c, a.lastStatus, nil)
}

func (a *ServerController) getXrayVersion(c *gin.Context) {
	now := time.Now()
	if now.Sub(a.lastGetVersionsTime) <= time.Minute {
		jsonObj(c, a.lastVersions, nil)
		return
	}

	versions, err := a.serverService.GetXrayVersions()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "getVersion"), err)
		return
	}

	a.lastVersions = versions
	a.lastGetVersionsTime = time.Now()

	jsonObj(c, versions, nil)
}

func (a *ServerController) installXray(c *gin.Context) {
	version := c.Param("version")
	err := a.serverService.UpdateXray(version)
	jsonMsg(c, I18nWeb(c, "pages.index.xraySwitchVersionPopover"), err)
}

func (a *ServerController) updateGeofile(c *gin.Context) {
	fileName := c.Param("fileName")
	err := a.serverService.UpdateGeofile(fileName)
	jsonMsg(c, I18nWeb(c, "pages.index.geofileUpdatePopover"), err)
}

func (a *ServerController) stopXrayService(c *gin.Context) {
	a.lastGetStatusTime = time.Now()
	err := a.serverService.StopXrayService()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.xray.stopError"), err)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.xray.stopSuccess"), err)
}

func (a *ServerController) restartXrayService(c *gin.Context) {
	err := a.serverService.RestartXrayService()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.xray.restartError"), err)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.xray.restartSuccess"), err)
}

func (a *ServerController) getLogs(c *gin.Context) {
	count := c.Param("count")
	level := c.PostForm("level")
	syslog := c.PostForm("syslog")
	logs := a.serverService.GetLogs(count, level, syslog)
	jsonObj(c, logs, nil)
}

func (a *ServerController) getXrayLogs(c *gin.Context) {
	count := c.Param("count")
	filter := c.PostForm("filter")
	showDirect := c.PostForm("showDirect")
	showBlocked := c.PostForm("showBlocked")
	showProxy := c.PostForm("showProxy")

	var freedoms []string
	var blackholes []string

	//getting tags for freedom and blackhole outbounds
	config, err := a.settingService.GetDefaultXrayConfig()
	if err == nil && config != nil {
		if cfgMap, ok := config.(map[string]interface{}); ok {
			if outbounds, ok := cfgMap["outbounds"].([]interface{}); ok {
				for _, outbound := range outbounds {
					if obMap, ok := outbound.(map[string]interface{}); ok {
						switch obMap["protocol"] {
						case "freedom":
							if tag, ok := obMap["tag"].(string); ok {
								freedoms = append(freedoms, tag)
							}
						case "blackhole":
							if tag, ok := obMap["tag"].(string); ok {
								blackholes = append(blackholes, tag)
							}
						}
					}
				}
			}
		}
	}

	if len(freedoms) == 0 {
		freedoms = []string{"direct"}
	}
	if len(blackholes) == 0 {
		blackholes = []string{"blocked"}
	}

	logs := a.serverService.GetXrayLogs(count, filter, showDirect, showBlocked, showProxy, freedoms, blackholes)
	jsonObj(c, logs, nil)
}

func (a *ServerController) getConfigJson(c *gin.Context) {
	configJson, err := a.serverService.GetConfigJson()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.index.getConfigError"), err)
		return
	}
	jsonObj(c, configJson, nil)
}

func (a *ServerController) getDb(c *gin.Context) {
	db, err := a.serverService.GetDb()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.index.getDatabaseError"), err)
		return
	}

	filename := "x-ui.db"

	if !isValidFilename(filename) {
		c.AbortWithError(http.StatusBadRequest, fmt.Errorf("invalid filename"))
		return
	}

	// Set the headers for the response
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", "attachment; filename="+filename)

	// Write the file contents to the response
	c.Writer.Write(db)
}

func isValidFilename(filename string) bool {
	// Validate that the filename only contains allowed characters
	return filenameRegex.MatchString(filename)
}

func (a *ServerController) importDB(c *gin.Context) {
	// Get the file from the request body
	file, _, err := c.Request.FormFile("db")
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.index.readDatabaseError"), err)
		return
	}
	defer file.Close()
	// Always restart Xray before return
	defer a.serverService.RestartXrayService()
	defer func() {
		a.lastGetStatusTime = time.Now()
	}()
	// Import it
	err = a.serverService.ImportDB(file)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.index.importDatabaseError"), err)
		return
	}
	jsonObj(c, I18nWeb(c, "pages.index.importDatabaseSuccess"), nil)
}

func (a *ServerController) getNewX25519Cert(c *gin.Context) {
	cert, err := a.serverService.GetNewX25519Cert()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.getNewX25519CertError"), err)
		return
	}
	jsonObj(c, cert, nil)
}

func (a *ServerController) getNewmldsa65(c *gin.Context) {
	cert, err := a.serverService.GetNewmldsa65()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.getNewmldsa65Error"), err)
		return
	}
	jsonObj(c, cert, nil)
}

func (a *ServerController) getNewEchCert(c *gin.Context) {
	sni := c.PostForm("sni")
	cert, err := a.serverService.GetNewEchCert(sni)
	if err != nil {
		jsonMsg(c, "get ech certificate", err)
		return
	}
	jsonObj(c, cert, nil)
}

func (a *ServerController) getNewVlessEnc(c *gin.Context) {
	out, err := a.serverService.GetNewVlessEnc()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.inbounds.toasts.getNewVlessEncError"), err)
		return
	}
	jsonObj(c, out, nil)
}

func (a *ServerController) getNewUUID(c *gin.Context) {
	uuidResp, err := a.serverService.GetNewUUID()
	if err != nil {
		jsonMsg(c, "Failed to generate UUID", err)
		return
	}

	jsonObj(c, uuidResp, nil)
}

func (a *ServerController) getNewmlkem768(c *gin.Context) {
	out, err := a.serverService.GetNewmlkem768()
	if err != nil {
		jsonMsg(c, "Failed to generate mlkem768 keys", err)
		return
	}
	jsonObj(c, out, nil)
}

func (a *ServerController) saveHistory(c *gin.Context) {
	/* 【中文注释】: 旧的错误代码，因为它期望一个 JSON 请求体，但前端发送的是表单数据
	var req struct {
		Type string `json:"type"`
		Link string `json:"link"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonMsg(c, "Invalid request", err)
		return
	}
	err := a.serverService.SaveLinkHistory(req.Type, req.Link)
	*/

	// 【中文注释】: 修改后的新代码，直接从 POST 表单中获取 'type' 和 'link' 参数
	// 【中文注释】: 这与其他 POST 方法（如 getLogs, getXrayLogs）的处理方式保持一致，解决了数据格式不匹配的问题。
	historyType := c.PostForm("type")
	link := c.PostForm("link")

	// 【中文注释】: 调用服务层方法来保存历史记录
	err := a.serverService.SaveLinkHistory(historyType, link)
	if err != nil {
		jsonMsg(c, "Failed to save history", err)
		return
	}
	jsonMsg(c, "History saved successfully", nil)
}

func (a *ServerController) loadHistory(c *gin.Context) {
	history, err := a.serverService.LoadLinkHistory()
	if err != nil {
		jsonMsg(c, "Failed to load history", err)
		return
	}
	jsonObj(c, history, nil)
}


// 〔新增接口〕: 安装 Subconverter
// 〔中文注释〕: 这个函数是暴露给前端的 API 接口，用于处理安装 Subconverter 的请求。
func (a *ServerController) installSubconverter(c *gin.Context) {
    // 〔中文注释〕: 调用服务层中我们刚刚创建的 InstallSubconverter 方法。
    err := a.serverService.InstallSubconverter()
    if err != nil {
        // 〔中文注释〕: 如果 service 层返回了错误，则向前台返回失败的 JSON 消息。
        jsonMsg(c, "Subconverter 安装指令执行失败", err)
        return
    }
    // 〔中文注释〕: 如果没有错误，则向前台返回成功的 JSON 消息。
    jsonMsg(c, "Subconverter 安装指令已成功发送", nil)
}

// 【新增接口实现】: 前端放行端口
func (a *ServerController) openPort(c *gin.Context) {

	// 直接使用 c.PostForm("port") 获取表单数据
	port := c.PostForm("port")

	// 1. 手动进行参数校验
	if port == "" {
		jsonMsg(c, "请求端口参数失败", fmt.Errorf("无效的请求参数，请确保端口号存在"))
		return
	}

	// 【中文注释】: 2. 调用服务层方法，该方法会立即返回，并在后台启动一个协程执行任务。
	a.serverService.OpenPort(port)

	// 【中文注释】: 3. 因为服务层方法是异步的，不再检查它的 error 返回值。
	//    直接向前端返回一个成功的消息，告知用户指令已发送。
	jsonMsg(c, "端口放行指令已成功发送，正在后台执行...", nil)
}
