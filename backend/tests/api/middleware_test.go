package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestRecoveryMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Create test router
	router := gin.New()

	// Add recovery middleware
	router.Use(RecoveryMiddleware())

	// Add test route that panics
	router.GET("/panic", func(c *gin.Context) {
		panic("test panic")
	})

	// Create test request
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/panic", nil)

	// Execute request
	router.ServeHTTP(w, req)

	// Assert that recovery middleware handled the panic
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "Internal server error")
}

func TestLoggerMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Create test router
	router := gin.New()

	// Add logger middleware
	router.Use(LoggerMiddleware())

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

func TestRateLimitMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Create test router
	router := gin.New()

	// Add rate limit middleware
	router.Use(RateLimitMiddleware(100))

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