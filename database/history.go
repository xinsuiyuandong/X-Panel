package database

import (
	"time"
)

// LinkHistory GORM aodel for link_history table
type LinkHistory struct {
	Id         int       `gorm:"primaryKey"`
	Type       string    `gorm:"type:varchar(255);not null"`
	Link       string    `gorm:"type:text;not null"`
	CreatedAt  time.Time `gorm:"not null"`
}

// AddLinkHistory adds a new link record, trims old ones, and ensures data is persisted.
func AddLinkHistory(record *LinkHistory) error {
	// 1. Add the new record
	if err := db.Create(record).Error; err != nil {
		return err
	}

	// 2. Trim old records, keeping only the 10 most recent
	var count int64
	db.Model(&LinkHistory{}).Count(&count)
	if count > 10 {
		limit := int(count) - 10
		var recordsToDelete []LinkHistory
		if err := db.Order("created_at asc").Limit(limit).Find(&recordsToDelete).Error; err != nil {
			return err
		}
		if len(recordsToDelete) > 0 {
			if err := db.Delete(&recordsToDelete).Error; err != nil {
				return err
			}
		}
	}

	// 【核心修正】: 在所有数据库写入和删除操作完成后，
	// 立即在此处调用 Checkpoint，确保本次操作被完整持久化到数据库文件。
	return Checkpoint()
}

// GetLinkHistory retrieves the 10 most recent link records
func GetLinkHistory() ([]*LinkHistory, error) {
	var histories []*LinkHistory
	err := db.Order("created_at desc").Limit(10).Find(&histories).Error
	if err != nil {
		return nil, err
	}
	return histories, nil
}

