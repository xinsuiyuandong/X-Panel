package model

import "time"

// LotteryWin 用于记录用户的中奖历史
type LotteryWin struct {
	ID        int64     `gorm:"primaryKey"`
	UserID    int64     `gorm:"index"` // Telegram 用户 ID
	Prize     string    // 奖品等级，如 "一等奖"
	WinDate   time.Time // 中奖日期
}