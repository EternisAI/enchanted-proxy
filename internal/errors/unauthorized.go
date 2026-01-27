package errors

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// AbortWithUnauthorized sends a 401 Unauthorized response and aborts the request.
func AbortWithUnauthorized(c *gin.Context, message string, details map[string]interface{}) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, NewAPIError(message, details))
}

// Unauthorized sends a 401 Unauthorized response without aborting.
func Unauthorized(c *gin.Context, message string, details map[string]interface{}) {
	c.JSON(http.StatusUnauthorized, NewAPIError(message, details))
}
