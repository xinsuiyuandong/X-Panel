package common

import (
	"crypto/rand"
	"math/big"
)

// RandomInt 返回一个 0 .. max-1 之间的随机整数（使用 crypto/rand）
func RandomInt(max int) int {
	if max <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return 0
	}
	return int(n.Int64())
}
