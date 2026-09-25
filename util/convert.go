package util

import (
	"strconv"
)

// StringToInt 将字符串转换为整数，如果转换失败则返回0
func StringToInt(s string) int {
	if s == "" {
		return 0
	}

	i, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return i
}
