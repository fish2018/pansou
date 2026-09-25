package util

import (
	"bytes"
	"compress/gzip"
	"io/ioutil"
	"strings"

	"github.com/gin-gonic/gin"
	"pansou/config"
)

// 压缩响应的包装器
type gzipResponseWriter struct {
	gin.ResponseWriter
	gzipWriter *gzip.Writer
}

// 实现Write接口
func (g *gzipResponseWriter) Write(data []byte) (int, error) {
	return g.gzipWriter.Write(data)
}

// 实现WriteString接口
func (g *gzipResponseWriter) WriteString(s string) (int, error) {
	return g.gzipWriter.Write([]byte(s))
}

// 关闭gzip写入器
func (g *gzipResponseWriter) Close() {
	g.gzipWriter.Close()
}

// GzipMiddleware 返回一个Gin中间件，用于压缩HTTP响应
func GzipMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 如果未启用压缩，直接跳过
		if !config.AppConfig.EnableCompression {
			c.Next()
			return
		}

		// 检查客户端是否支持gzip
		if !strings.Contains(c.Request.Header.Get("Accept-Encoding"), "gzip") {
			c.Next()
			return
		}

		// 创建一个缓冲响应写入器
		buffer := &bytes.Buffer{}
		blw := &bodyLogWriter{body: buffer, ResponseWriter: c.Writer}
		c.Writer = blw

		// 处理请求
		c.Next()

		// 获取响应内容
		responseData := buffer.Bytes()

		// 如果响应大小小于最小压缩大小，直接返回原始内容
		if len(responseData) < config.AppConfig.MinSizeToCompress {
			c.Writer.Write(responseData)
			return
		}

		// 设置gzip响应头
		c.Header("Content-Encoding", "gzip")
		c.Header("Vary", "Accept-Encoding")

		// 创建gzip写入器
		gz, err := gzip.NewWriterLevel(c.Writer, gzip.BestSpeed)
		if err != nil {
			c.Writer.Write(responseData)
			return
		}
		defer gz.Close()

		// 写入压缩内容
		gz.Write(responseData)
	}
}

// bodyLogWriter 是一个用于记录响应体的写入器
type bodyLogWriter struct {
	gin.ResponseWriter
	body *bytes.Buffer
}

// Write 实现ResponseWriter接口
func (w bodyLogWriter) Write(b []byte) (int, error) {
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}

// WriteString 实现ResponseWriter接口
func (w bodyLogWriter) WriteString(s string) (int, error) {
	w.body.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

// CompressData 压缩数据
func CompressData(data []byte) ([]byte, error) {
	var buf bytes.Buffer

	// 创建gzip写入器
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}

	// 写入数据
	if _, err := gz.Write(data); err != nil {
		return nil, err
	}

	// 关闭写入器
	if err := gz.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// DecompressData 解压数据
func DecompressData(data []byte) ([]byte, error) {
	// 创建gzip读取器
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	// 读取解压后的数据
	return ioutil.ReadAll(gz)
}
