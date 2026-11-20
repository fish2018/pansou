package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"pansou/discover"
)

func registerDiscoverRoutes(group *gin.RouterGroup, service *discover.Service) {
	group.GET("/discover", func(c *gin.Context) {
		category, collection := c.DefaultQuery("category", "movie"), c.DefaultQuery("collection", "hot")
		if !discover.ValidSelection(category, collection) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的推荐分类或栏目"})
			return
		}
		page, pageErr := strconv.Atoi(c.DefaultQuery("page", "1"))
		pageSize, sizeErr := strconv.Atoi(c.DefaultQuery("page_size", "24"))
		options := discover.ListOptions{Page: page, PageSize: pageSize, Genre: c.Query("genre"), Query: c.Query("q"), Tag: c.Query("tag"), Year: c.Query("year"), YearFrom: c.Query("year_from"), YearTo: c.Query("year_to")}
		query := c.Request.URL.Query()
		hasRange := query.Has("year_from") || query.Has("year_to")
		if pageErr != nil || sizeErr != nil || !options.Valid() || (hasRange && (options.YearFrom == "" || options.YearTo == "")) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的页码、每页数量或筛选条件"})
			return
		}
		response, err := service.Query(c.Request.Context(), category, collection, options)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "暂时无法获取推荐，请稍后重试"})
			return
		}
		c.Header("Cache-Control", "private, no-cache")
		c.JSON(http.StatusOK, response)
	})
	group.GET("/discover/poster", func(c *gin.Context) {
		address := c.Query("url")
		if !discover.ValidPosterURL(address) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的海报地址"})
			return
		}
		data, kind, err := service.Poster(c.Request.Context(), address)
		if err != nil {
			c.Status(http.StatusBadGateway)
			return
		}
		c.Header("Cache-Control", "private, max-age=86400")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Data(http.StatusOK, kind, data)
	})
	group.GET("/discover/subject", func(c *gin.Context) {
		id, mediaType := c.Query("id"), c.Query("media_type")
		if !discover.ValidSubject(id, mediaType) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的影视条目"})
			return
		}
		item, err := service.Subject(c.Request.Context(), id, mediaType)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "暂时无法获取作品介绍，请稍后重试"})
			return
		}
		c.Header("Cache-Control", "private, no-cache")
		c.JSON(http.StatusOK, item)
	})
}
