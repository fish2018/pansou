package api

import (
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"pansou/model"
	"pansou/service"
)

var (
	checkService     *service.CheckService
	checkServiceOnce sync.Once
)

func getCheckService() *service.CheckService {
	checkServiceOnce.Do(func() {
		checkService = service.NewCheckService()
	})
	return checkService
}

func CheckHandler(c *gin.Context) {
	var req model.CheckRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(400, "无效的检测请求: "+err.Error()))
		return
	}

	if len(req.Items) == 0 {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(400, "items不能为空"))
		return
	}

	// items 数量必须封顶：每个 item 都会触发一次上游请求，且请求体里的 proxy_url
	// 由调用方指定（见下），不限量即可把本服务当成放大器使用。
	if itemsOverLimit(len(req.Items)) {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(400,
			fmt.Sprintf("items 数量 %d 超过上限 %d", len(req.Items), maxCheckItems)))
		return
	}

	proxyURL := strings.TrimSpace(req.ProxyURL)
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(req.Proxy)
	}

	response, err := getCheckService().CheckWithProxy(req.Items, proxyURL)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.NewErrorResponse(400, "无效的代理参数: "+err.Error()))
		return
	}

	c.JSON(http.StatusOK, response)
}
