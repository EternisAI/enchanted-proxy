package errors

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// AbortWithConflict sends a 409 Conflict response and aborts the request.
func AbortWithConflict(c *gin.Context, message string, details map[string]interface{}) {
	c.AbortWithStatusJSON(http.StatusConflict, NewAPIError(message, details))
}

// Conflict sends a 409 Conflict response without aborting.
func Conflict(c *gin.Context, message string, details map[string]interface{}) {
	c.JSON(http.StatusConflict, NewAPIError(message, details))
}
