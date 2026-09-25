package api

import (
	"github.com/gin-gonic/gin"
)

// Gateway represents the API gateway
type Gateway struct {
	router *gin.Engine
}

// NewGateway creates a new Gateway instance
func NewGateway() *Gateway {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()

	// Add default middleware
	router.Use(
		RecoveryMiddleware(),
		LoggerMiddleware(),
	)

	return &Gateway{
		router: router,
	}
}

// RegisterRoute registers a new route with the given method, path, and handler
func (g *Gateway) RegisterRoute(method, path string, handler gin.HandlerFunc) {
	switch method {
	case "GET":
		g.router.GET(path, handler)
	case "POST":
		g.router.POST(path, handler)
	case "PUT":
		g.router.PUT(path, handler)
	case "DELETE":
		g.router.DELETE(path, handler)
	case "PATCH":
		g.router.PATCH(path, handler)
	case "HEAD":
		g.router.HEAD(path, handler)
	case "OPTIONS":
		g.router.OPTIONS(path, handler)
	}
}

// Run starts the HTTP server
func (g *Gateway) Run(addr string) error {
	return g.router.Run(addr)
}

// GetRouter returns the underlying gin.Engine
func (g *Gateway) GetRouter() *gin.Engine {
	return g.router
}