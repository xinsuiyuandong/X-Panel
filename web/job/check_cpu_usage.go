package job 
 
import ( 
 "strconv" 
 "time" 
 
 "x-ui/web/service" 
 
 "github.com/shirou/gopsutil/v4/cpu" 
) 
 
// 连续超阈值告警实现 
type CheckCpuJob struct { 
 tgbotService        service.Tgbot 
 settingService      service.SettingService 
 overThresholdCount  int       // 连续超阈值计数器 
 lastNotifyTime      time.Time // 最近一次告警时间 
} 
 
func NewCheckCpuJob() *CheckCpuJob { 
 return &CheckCpuJob{} 
} 
 
// run 是 Job 接口方法 
func (j *CheckCpuJob) Run() { 
 threshold, _ := j.settingService.GetTgCpu() 
 notifyInterval := 10 * time.Minute // 两次告警最小间隔，可做成配置项 
 
 percent, err := cpu.Percent(10*time.Second, false) // 10秒采样 
 if err != nil || len(percent) == 0 { 
  return 
 } 
 
 now := time.Now() 
 if percent[0] > float64(threshold) { 
  j.overThresholdCount++ 
 } else { 
  j.overThresholdCount = 0 
 } 
 
 // 连续3次超阈值，且距离上次告警超过告警间隔 
 if j.overThresholdCount >= 3 && now.Sub(j.lastNotifyTime) > notifyInterval { 
  msg := j.tgbotService.I18nBot( 
   "tgbot.messages.cpuThreshold", 
   "Percent=="+strconv.FormatFloat(percent[0], 'f', 2, 64), 
   "Threshold=="+strconv.Itoa(threshold),
   "SampleInterval==10s",
   "NotifyPolicy==连续3次超阈值",
  ) 
  j.tgbotService.SendMsgToTgbotAdmins(msg) 
  j.lastNotifyTime = now 
  j.overThresholdCount = 0 
 } 
}
