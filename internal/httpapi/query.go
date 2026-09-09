package httpapi

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"

	"audiorecording/internal/recording"
	"github.com/gin-gonic/gin"
)

// URL 参数先是字符串；转成正整数后才交给业务函数。
func positiveNumber(raw string, max uint64) (uint64, bool) {
	if raw == "" {
		return 0, false
	}
	for _, char := range raw {
		if char < '0' || char > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	return value, err == nil && value > 0 && value <= max
}

func queryError(c *gin.Context, err error) {
	if errors.Is(err, recording.ErrNotFound) {
		writeError(c, http.StatusNotFound, "not_found", "resource not found")
	} else {
		log.Printf("query failed: %v", err)
		writeError(c, http.StatusInternalServerError, "internal_error", "cannot query resource")
	}
}

func getTask(service *recording.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := positiveNumber(c.Param("id"), ^uint64(0))
		if !ok {
			writeError(c, 400, "invalid_id", "id must be a positive uint64 integer")
			return
		}
		result, err := service.Task(c.Request.Context(), id)
		if err != nil {
			queryError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func getRecording(service *recording.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := positiveNumber(c.Param("id"), ^uint64(0))
		if !ok {
			writeError(c, 400, "invalid_id", "id must be a positive uint64 integer")
			return
		}
		result, err := service.Detail(c.Request.Context(), id)
		if err != nil {
			queryError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func listRecordings(service *recording.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Query 来自 ? 后的查询串；Param 来自路由的 :id，两者不是同一种参数。
		// 不提供时使用默认值；显式空值、重复参数、负数、小数、溢出都拒绝。
		values, err := url.ParseQuery(c.Request.URL.RawQuery)
		if err != nil {
			writeError(c, 400, "invalid_pagination", "invalid query string")
			return
		}
		rawPage, rawSize := "1", "20"
		if v, exists := values["page"]; exists {
			if len(v) != 1 {
				writeError(c, 400, "invalid_pagination", "page must occur once")
				return
			}
			rawPage = v[0]
		}
		if v, exists := values["page_size"]; exists {
			if len(v) != 1 {
				writeError(c, 400, "invalid_pagination", "page_size must occur once")
				return
			}
			rawSize = v[0]
		}
		page, validPage := positiveNumber(rawPage, 1_000_000)
		size, validSize := positiveNumber(rawSize, 100)
		if !validPage || !validSize {
			writeError(c, 400, "invalid_pagination", "page must be 1..1000000 and page_size must be 1..100")
			return
		}
		result, err := service.List(c.Request.Context(), int(page), int(size))
		if err != nil {
			queryError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}
