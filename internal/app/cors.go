package app

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// corsMiddleware implements the minimal CORS the REST commands need.
//
// This is the M4 carry-over from the FE M2 live gate FAIL: with the
// Boggle-mirrored cors block commented out, every browser preflight
// (OPTIONS + Access-Control-Request-Method) fell through to gin's 404,
// so the FE could never POST answer/walk/lifeline from the Vite dev
// origin. Hand-rolled instead of gin-contrib/cors to keep the behavior
// explicit and dependency-free:
//   - preflights answer 204 + ACAM/ACAH (and ACAO when the Origin is
//     allowed); disallowed origins answer 204 with NO ACAO, so browsers
//     block the follow-up request themselves;
//   - non-preflight requests from an allowed Origin get ACAO echoed, so
//     the actual POSTs (Content-Type application/json) succeed;
//   - everything else passes through untouched — the WS origin check
//     (upgrader CheckOrigin) and the REST rate limiter are unchanged
//     (Boggle-parity nuance: this must not regress the WS check).
//
// The middleware is registered ENGINE-LEVEL (not on the /api group)
// deliberately: gin only runs group middleware when a route matches, and
// there are intentionally no OPTIONS routes — a group-only middleware
// would 404 every preflight again. Engine-level middleware also runs on
// gin's NoRoute chain, so preflights to unknown paths (and to routes that
// do not exist yet, e.g. POST /api/rooms/:id/lifeline in M5) already get
// the correct preflight answer today.
func corsMiddleware(allowed []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if origin == "" {
			c.Next() // same-origin / non-browser request — nothing to do
			return
		}

		// Correct caching behavior for every intermediate cache.
		c.Header("Vary", "Origin")

		allowOrigin := originAllowed(origin, allowed)
		if allowOrigin {
			c.Header("Access-Control-Allow-Origin", origin)
		}

		isPreflight := c.Request.Method == http.MethodOptions &&
			c.Request.Header.Get("Access-Control-Request-Method") != ""
		if isPreflight {
			if allowOrigin {
				c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Accept, Authorization")
				c.Header("Access-Control-Max-Age", "43200")
			}
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// originAllowed matches the request origin against the allowlist,
// ignoring a trailing slash on either side (same normalization the WS
// checkOrigin uses).
func originAllowed(origin string, allowed []string) bool {
	origin = strings.TrimSuffix(origin, "/")
	for _, a := range allowed {
		if origin == strings.TrimSuffix(a, "/") {
			return true
		}
	}
	return false
}
