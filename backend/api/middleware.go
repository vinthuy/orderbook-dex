package api

import (
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// RecoveryMiddleware returns a gin.HandlerFunc that recovers from panics
func RecoveryMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("Recovery from panic: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": "Internal server error",
				})
				c.Abort()
			}
		}()
		c.Next()
	}
}

// LoggerMiddleware returns a gin.HandlerFunc that logs request details
func LoggerMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Start timer
		startTime := time.Now()

		// Process request
		c.Next()

		// Calculate latency
		latency := time.Since(startTime)

		// Log request details
		log.Printf("[GIN] %s %s %d %s",
			c.Request.Method,
			c.Request.URL.Path,
			c.Writer.Status(),
			latency,
		)
	}
}

// RateLimitMiddleware returns a gin.HandlerFunc that implements rate limiting
func RateLimitMiddleware(limit int) gin.HandlerFunc {
	// This is a placeholder implementation
	// In production, you would use a proper rate limiter like token bucket or sliding window
	return func(c *gin.Context) {
		// For now, just pass through all requests
		c.Next()
	}
}