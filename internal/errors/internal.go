package errors

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// AbortWithInternal sends a 500 Internal Server Error response and aborts the request.
func AbortWithInternal(c *gin.Context, message string, details map[string]interface{}) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, NewAPIError(message, details))
}

// Internal sends a 500 Internal Server Error response without aborting.
func Internal(c *gin.Context, message string, details map[string]interface{}) {
	c.JSON(http.StatusInternalServerError, NewAPIError(message, details))
}
