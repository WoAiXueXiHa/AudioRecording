package httpapi

import (
	"audiorecording/internal/recording"
	"errors"
	"github.com/gin-gonic/gin"
	"net/http"
)

func deleteRecording(service *recording.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := positiveNumber(c.Param("id"), ^uint64(0))
		if !ok {
			writeError(c, 400, "invalid_id", "id must be a positive uint64 integer")
			return
		}
		err := service.Delete(c.Request.Context(), id)
		if errors.Is(err, recording.ErrConflict) {
			writeError(c, 409, "state_conflict", "active recordings cannot be deleted")
			return
		}
		if err != nil {
			queryError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}
