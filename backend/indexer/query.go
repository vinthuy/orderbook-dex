package indexer

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"web3-amm-dex-backend/ledger"
	"web3-amm-dex-backend/services"
)

// QueryServer exposes read-only history queries and the reconcile endpoint. It
// is assembled by main and mounted onto the existing gin engine.
type QueryServer struct {
	Store   *Store
	Adapter services.BlockchainAdapter
	Chain   string
	Running *ledger.Ledger // optional: the running ledger to reconcile against
}

// Register mounts the /v1 read routes. Read endpoints are unauthenticated
// (consistent with subproject 4).
func (q *QueryServer) Register(r gin.IRouter) {
	v1 := r.Group("/v1")
	v1.GET("/trades", q.trades)
	v1.GET("/fund-events", q.fundEvents)
	v1.GET("/roots", q.roots)
	v1.POST("/reconcile", q.reconcile)
}

func (q *QueryServer) trades(c *gin.Context) {
	market, _ := strconv.ParseUint(c.DefaultQuery("market", "0"), 10, 32)
	account := c.DefaultQuery("account", "")
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	out, err := q.Store.QueryTrades(uint32(market), account, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, out)
}

func (q *QueryServer) fundEvents(c *gin.Context) {
	account := c.DefaultQuery("account", "")
	asset, _ := strconv.ParseUint(c.DefaultQuery("asset", "0"), 10, 32)
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	out, err := q.Store.QueryFundEvents(account, uint32(asset), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, out)
}

func (q *QueryServer) roots(c *gin.Context) {
	from, _ := strconv.Atoi(c.DefaultQuery("from", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	out, err := q.Store.QueryRoots(from, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, out)
}

func (q *QueryServer) reconcile(c *gin.Context) {
	if q.Adapter == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "reconcile requires an adapter"})
		return
	}
	rebuilt, err := Rebuild(q.Adapter, q.Chain, 1)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if q.Running == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "reconcile requires the running ledger"})
		return
	}
	deltas, err := CompareTotals(rebuilt, q.Running)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	consistent := true
	for _, d := range deltas {
		if !d.Match {
			consistent = false
			break
		}
	}
	c.JSON(http.StatusOK, gin.H{"assets": deltas, "consistent": consistent})
}
