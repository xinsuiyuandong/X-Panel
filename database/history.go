package database

import (
	"time"
	"gorm.io/gorm" // 【中文注释】: 确保 gorm 被导入，以便在函数签名中使用
)

// LinkHistory GORM aodel for link_history table
type LinkHistory struct {
	Id         int       `gorm:"primaryKey"`
	Type       string    `gorm:"type:varchar(255);not null"`
	Link       string    `gorm:"type:text;not null"`
	CreatedAt  time.Time `gorm:"not null"`
}

// AddLinkHistory 在一个事务中添加新链接记录并修剪旧记录。
// 它确保了操作的原子性：所有更改要么全部应用，要么全部回滚。
func AddLinkHistory(record *LinkHistory) error {
	// 【核心修正】: 使用 GORM 的事务功能来包装所有的数据库写入和删除操作。
	// 这样可以确保数据的一致性。
	return db.Transaction(func(tx *gorm.DB) error {
		// 1. 添加新记录
		// 【重要】: 在事务内部，必须使用 tx 变量，而不是全局的 db 变量。
		if err := tx.Create(record).Error; err != nil {
			return err // 如果这里出错，事务将自动回滚
		}

		// 2. 修剪旧记录，仅保留最近的 10 条
		var count int64
		// 【重要】: 使用 tx 进行计数
		if err := tx.Model(&LinkHistory{}).Count(&count).Error; err != nil {
			return err
		}

		if count > 10 {
			limit := int(count) - 10
			var recordsToDelete []LinkHistory
			// 【重要】: 使用 tx 查找要删除的记录
			if err := tx.Order("created_at asc").Limit(limit).Find(&recordsToDelete).Error; err != nil {
				return err
			}
			if len(recordsToDelete) > 0 {
				// 【重要】: 使用 tx 删除记录
				if err := tx.Delete(&recordsToDelete).Error; err != nil {
					return err
				}
			}
		}

		// 【核心修正】: 从此函数中移除了 Checkpoint() 调用。
		// 事务成功后返回 nil，GORM 会自动提交事务。
		return nil
	})
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
