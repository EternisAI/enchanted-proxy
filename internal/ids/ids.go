// Package ids validates identifiers supplied by clients, such as chat and
// message IDs, before they reach URLs, Firestore paths or database rows.
package ids

import (
	"regexp"

	"github.com/eternisai/enchanted-proxy/internal/errors"
	"github.com/gin-gonic/gin"
)

// MaxLength is the longest identifier accepted.
const MaxLength = 64

var pattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Valid reports whether s is a well-formed identifier: 1 to MaxLength
// characters, each an ASCII letter, a digit, '_' or '-'. Clients send
// UUIDs, which always match.
func Valid(s string) bool {
	return pattern.MatchString(s)
}

// ValidOptional reports whether s is empty or a well-formed identifier.
func ValidOptional(s string) bool {
	return s == "" || Valid(s)
}

// requestHeaders are the headers through which clients pass chat and message IDs.
var requestHeaders = []string{"X-Chat-ID", "X-Message-ID", "X-User-Message-ID"}

// routeParams are the route parameters that carry chat and message IDs.
var routeParams = []string{"chatId", "messageId"}

// Middleware refuses a request with 400 when one of the ID headers or ID route
// parameters is present but malformed. Absent values are left to the handlers.
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		for _, name := range requestHeaders {
			if !ValidOptional(c.GetHeader(name)) {
				errors.AbortWithBadRequest(c, "Invalid "+name+" header", nil)
				return
			}
		}
		for _, name := range routeParams {
			if !ValidOptional(c.Param(name)) {
				errors.AbortWithBadRequest(c, "Invalid "+name, nil)
				return
			}
		}
		c.Next()
	}
}
