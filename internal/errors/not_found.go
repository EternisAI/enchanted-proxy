package errors

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// AbortWithNotFound sends a 404 Not Found response and aborts the request.
func AbortWithNotFound(c *gin.Context, message string, details map[string]interface{}) {
	c.AbortWithStatusJSON(http.StatusNotFound, NewAPIError(message, details))
}

// NotFound sends a 404 Not Found response without aborting.
func NotFound(c *gin.Context, message string, details map[string]interface{}) {
	c.JSON(http.StatusNotFound, NewAPIError(message, details))
}
