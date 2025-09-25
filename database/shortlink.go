package database

import (
	"time"
)

// 〔中文注释〕：这是 ShortLink 模型的定义，用于在数据库中存储短链接和原始长链接的映射关系。
// GORM 会根据这个结构体自动创建名为 "short_links" 的表。
type ShortLink struct {
	Id        int       `gorm:"primaryKey"`                 // 〔中文注释〕：主键ID，自增长
	Code      string    `gorm:"type:varchar(255);not null;uniqueIndex"` // 〔中文注释〕：随机生成的短代码，例如 "aK9sLpW1"，并设置为唯一索引以保证不重复
	FullLink  string    `gorm:"type:text;not null"`         // 〔中文注释〕：原始的、非常长的 VLESS 链接
	CreatedAt time.Time `gorm:"not null"`                 // 〔中文注释〕：记录创建时间
}


// AddShortLink 向数据库中插入一条新的短链接记录
func AddShortLink(link *ShortLink) error {
	return db.Create(link).Error
}

// GetShortLink 根据短代码从数据库中查询记录
func GetShortLink(code string) (*ShortLink, error) {
	var link ShortLink
	err := db.Where("code = ?", code).First(&link).Error
	if err != nil {
		if IsNotFound(err) {
			// 〔中文注释〕：如果没有找到记录，返回 nil 和 nil error，这属于正常情况
			return nil, nil
		}
		// 〔中文注释〕：如果发生其他数据库错误，则返回错误
		return nil, err
	}
	return &link, nil
}