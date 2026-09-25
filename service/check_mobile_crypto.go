package service

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	utiljson "pansou/util/json"
	"reflect"
)

var mobileCryptoKey = []byte("PVGDwmcvfs1uV3d1")

func encryptMobilePayload(data interface{}) (string, error) {
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", fmt.Errorf("生成随机向量失败: %w", err)
	}

	plaintext, err := marshalMobilePayload(data)
	if err != nil {
		return "", err
	}

	plaintext = addMobilePadding(plaintext, aes.BlockSize)

	block, err := aes.NewCipher(mobileCryptoKey)
	if err != nil {
		return "", fmt.Errorf("初始化加密器失败: %w", err)
	}

	ciphertext := make([]byte, len(plaintext))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, plaintext)
	return base64.StdEncoding.EncodeToString(append(iv, ciphertext...)), nil
}

func decryptMobilePayload(cipherText string) (string, error) {
	encrypted, err := base64.StdEncoding.DecodeString(cipherText)
	if err != nil {
		return "", fmt.Errorf("解码响应失败: %w", err)
	}

	if len(encrypted) < aes.BlockSize {
		return "", fmt.Errorf("响应长度异常")
	}

	iv := encrypted[:aes.BlockSize]
	body := encrypted[aes.BlockSize:]
	if len(body)%aes.BlockSize != 0 {
		return "", fmt.Errorf("响应块长度异常")
	}

	block, err := aes.NewCipher(mobileCryptoKey)
	if err != nil {
		return "", fmt.Errorf("初始化解密器失败: %w", err)
	}

	plaintext := make([]byte, len(body))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plaintext, body)

	plaintext, err = removeMobilePadding(plaintext)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}

func marshalMobilePayload(data interface{}) ([]byte, error) {
	switch value := data.(type) {
	case string:
		return []byte(value), nil
	case []byte:
		return value, nil
	default:
		kind := reflect.TypeOf(value)
		if kind == nil {
			return nil, fmt.Errorf("请求数据不能为空")
		}
		if kind.Kind() == reflect.Struct || kind.Kind() == reflect.Map {
			raw, err := utiljson.Marshal(value)
			if err != nil {
				return nil, fmt.Errorf("序列化请求失败: %w", err)
			}
			return raw, nil
		}
		return nil, fmt.Errorf("不支持的请求类型: %T", value)
	}
}

func addMobilePadding(data []byte, blockSize int) []byte {
	paddingSize := blockSize - len(data)%blockSize
	padding := make([]byte, paddingSize)
	for index := range padding {
		padding[index] = byte(paddingSize)
	}
	return append(data, padding...)
}

func removeMobilePadding(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("空响应无法去填充")
	}

	paddingSize := int(data[len(data)-1])
	if paddingSize <= 0 || paddingSize > len(data) {
		return nil, fmt.Errorf("填充长度非法")
	}

	for index := len(data) - paddingSize; index < len(data); index++ {
		if data[index] != byte(paddingSize) {
			return nil, fmt.Errorf("填充校验失败")
		}
	}

	return data[:len(data)-paddingSize], nil
}
