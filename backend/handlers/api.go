package handlers

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"web3-amm-dex-backend/models"
	"web3-amm-dex-backend/services"
)

func GetPool(c *gin.Context) {
	poolAddr := c.Param("pool")
	poolInfo, err := services.GetPoolInfo(poolAddr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, poolInfo)
}

func GetAllPools(c *gin.Context) {
	pools, err := services.GetAllPools()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, pools)
}

func Swap(c *gin.Context) {
	var req models.SwapRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	resp, err := services.Swap(req.TokenIn, req.TokenOut, req.AmountIn, req.AmountOutMin)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func AddLiquidity(c *gin.Context) {
	var req models.LiquidityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	resp, err := services.AddLiquidity(req.Token0Addr, req.Token1Addr, req.Amount0, req.Amount1)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func RemoveLiquidity(c *gin.Context) {
	token0 := c.Query("token0")
	token1 := c.Query("token1")
	liquidity := c.Query("liquidity")

	resp, err := services.RemoveLiquidity(token0, token1, liquidity)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func GetPoolInfo(c *gin.Context) {
	poolAddr := c.Param("pool")
	info, err := services.GetPoolInfo(poolAddr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, info)
}

func HealthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "amm-dex-backend"})
}

func GetEvents(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	events, err := services.GetRecentEvents(limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, events)
}
