package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kc1wfotfm4/qry-lakehouse-maintainer/internal/store"
)

// NewRouter wires the public HTTP surface. The service contract in README.md
// describes the error shape every entry must keep.
func NewRouter(st *store.Store) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	router.GET("/healthz", func(c *gin.Context) {
		if err := st.Ping(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "storage_unavailable", "message": "database is not available"}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "ok"})
	})

	router.POST("/api/v1/lake-tables/:id/snapshots", registerSnapshot(st))
	router.GET("/api/v1/lake-tables/:id/expired", listExpiredSnapshots(st))
	router.POST("/api/v1/maintenance", createMaintenance(st))
	router.GET("/api/v1/maintenance/:taskId", getMaintenance(st))

	router.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "route_not_found", "message": "no route matches this path"}})
	})
	return router
}
