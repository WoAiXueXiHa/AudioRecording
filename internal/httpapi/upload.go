package httpapi

import (
	"errors"
	"log"
	"net/http"

	"audiorecording/internal/recording"

	"github.com/gin-gonic/gin"
)

func uploadRecording(uploads *recording.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 限制整个请求体，给 multipart 边界和表单头额外留 1 MiB。
		// ParseMultipartForm 的 8 MiB 只是内存阈值，超过后落临时磁盘，不是上传上限。
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, recording.MaxFileBytes+(1<<20)) // 请求体上限 51 MiB
		err := c.Request.ParseMultipartForm(8 << 20)                                                   // 内存阈值 8 MiB
		if c.Request.MultipartForm != nil {
			defer c.Request.MultipartForm.RemoveAll() // 清理解析器的临时文件，与业务音频文件不同。
		}
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(c, 413, "file_too_large", "request body exceeds upload limit")
			} else {
				writeError(c, 400, "invalid_multipart", "expected a valid multipart/form-data request")
			}
			return
		}
		form := c.Request.MultipartForm
		files := form.File["file"]
		if len(files) != 1 || len(form.File) != 1 {
			writeError(c, 400, "invalid_file", "provide exactly one audio file in field file")
			return
		}
		if files[0].Size > recording.MaxFileBytes {
			writeError(c, 413, "file_too_large", "audio file must not exceed 50 MiB")
			return
		}
		src, err := files[0].Open()
		if err != nil {
			log.Printf("open multipart file failed: %v", err)
			writeError(c, 500, "internal_error", "cannot read uploaded file")
			return
		}
		defer src.Close()
		// 使用标准 context 传播请求取消，Gin Context 留在 HTTP 层。
		result, err := uploads.Upload(c.Request.Context(), files[0].Filename, src)
		switch {
		case errors.Is(err, recording.ErrInvalidFile):
			writeError(c, 400, "invalid_file", "provide a non-empty wav, mp3, m4a or aac file with a valid filename")
		case errors.Is(err, recording.ErrTooLarge):
			writeError(c, 413, "file_too_large", "audio file must not exceed 50 MiB")
		case errors.Is(err, recording.ErrCommitUnknown):
			writeError(c, 500, "upload_result_unknown", "upload result is uncertain; contact the operator before retrying")
		case err != nil:
			log.Printf("upload failed: %v", err)
			writeError(c, 500, "internal_error", "cannot save recording")
		default:
			log.Printf("upload created recording_id=%d task_id=%d", result.RecordingID, result.TaskID)
			c.JSON(http.StatusCreated, result)
		}
	}
}
