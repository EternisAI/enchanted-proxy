package errors

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// AbortWithBadRequest sends a 400 Bad Request response and aborts the request.
func AbortWithBadRequest(c *gin.Context, message string, details map[string]interface{}) {
	c.AbortWithStatusJSON(http.StatusBadRequest, NewAPIError(message, details))
}

// BadRequest sends a 400 Bad Request response without aborting.
// Use when you need to return an error but continue processing (rare).
func BadRequest(c *gin.Context, message string, details map[string]interface{}) {
	c.JSON(http.StatusBadRequest, NewAPIError(message, details))
}
