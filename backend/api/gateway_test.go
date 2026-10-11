package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestGateway_MiddlewareChain2(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Create test router
	router := gin.New()

	// Add middleware
	router.Use(
		RecoveryMiddleware(),
		LoggerMiddleware(),
	)

	// Add test route
	router.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "success"})
	})

	// Create test request
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)

	// Execute request
	router.ServeHTTP(w, req)

	// Assert response
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "success")
}