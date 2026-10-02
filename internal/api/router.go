package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kc1wfotfm4/qry-lakehouse-maintainer/internal/store"
)

// NewRouter wires the public HTTP surface: the health entry plus the snapshot
// registration, expiry listing and maintenance task routes.
func NewRouter(st *store.Store) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	h := &handler{st: st}

	router.GET("/healthz", func(c *gin.Context) {
		if err := st.Ping(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "storage_unavailable", "message": "database is not available"}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "ok"})
	})

	router.POST("/api/v1/lake-tables/:id/snapshots", h.registerSnapshot)
	router.GET("/api/v1/lake-tables/:id/expired", h.listExpiredSnapshots)
	router.POST("/api/v1/maintenance", h.createMaintenanceTask)
	router.GET("/api/v1/maintenance/:taskId", h.getMaintenanceTask)

	router.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "route_not_found", "message": "no route matches this path"}})
	})
	return router
}
