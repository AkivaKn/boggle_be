package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// newCORSTestEngine builds a minimal engine with the real CORS middleware
// and the REST command routes (stubs suffice — the middleware runs before
// any handler). POST /api/rooms/:id/lifeline is deliberately NOT
// registered: its preflight must still pass via the engine-level
// middleware on gin's NoRoute chain (M5 coverage before the route exists).
func newCORSTestEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(corsMiddleware([]string{
		"http://localhost:5173",
		"http://127.0.0.1:5173",
		"http://host.docker.internal:5173",
	}))
	r.POST("/api/rooms/:id/answer", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	r.POST("/api/rooms/:id/walk", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	return r
}

func do(r *gin.Engine, method, path, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// preflight is a browser-style preflight: OPTIONS + Access-Control-Request-Method.
func preflight(r *gin.Engine, path, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodOptions, path, nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestCORSPreflightAllowedOrigins is the FE-side contract: the Vite dev
// origin preflights all three REST command endpoints and gets 2xx (204)
// + Access-Control-Allow-Origin — including the not-yet-existing
// /lifeline route (which must NOT 404).
func TestCORSPreflightAllowedOrigins(t *testing.T) {
	r := newCORSTestEngine()
	paths := []string{
		"/api/rooms/abc-123/answer",
		"/api/rooms/abc-123/walk",
		"/api/rooms/abc-123/lifeline", // M5 route, preflight must already work
	}
	for _, path := range paths {
		for _, origin := range []string{
			"http://localhost:5173",
			"http://localhost:5173/", // trailing slash normalizes away
			"http://127.0.0.1:5173",
			"http://host.docker.internal:5173",
		} {
			w := preflight(r, path, origin)
			if w.Code < 200 || w.Code > 299 {
				t.Errorf("preflight %s from %s: code %d, want 2xx (body %q)", path, origin, w.Code, w.Body.String())
			}
			if got := w.Header().Get("Access-Control-Allow-Origin"); got != origin {
				t.Errorf("preflight %s from %s: ACAO = %q", path, origin, got)
			}
			if got := w.Header().Get("Access-Control-Allow-Methods"); got == "" {
				t.Errorf("preflight %s from %s: no Access-Control-Allow-Methods", path, origin)
			}
			if got := w.Header().Get("Access-Control-Allow-Headers"); got == "" {
				t.Errorf("preflight %s from %s: no Access-Control-Allow-Headers (content-type is needed)", path, origin)
			}
		}
	}
}

// TestCORSPreflightDisallowedOrigin: no ACAO, ever, for foreign origins —
// the browser then blocks the follow-up POST. Still 204 (not 404) so the
// preflight itself is well-formed.
func TestCORSPreflightDisallowedOrigin(t *testing.T) {
	r := newCORSTestEngine()
	for _, path := range []string{"/api/rooms/abc/answer", "/api/rooms/abc/walk", "/api/rooms/abc/lifeline"} {
		w := preflight(r, path, "http://evil.example")
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("preflight %s from disallowed origin: ACAO = %q, want none", path, got)
		}
		if w.Code != http.StatusNoContent {
			t.Errorf("preflight %s: code %d, want 204", path, w.Code)
		}
	}
}

// TestCORSActualPOST: the real POSTs (Content-Type application/json) from
// the allowed origin carry ACAO on the response; disallowed origins get
// no ACAO; requests without Origin (curl, tests) are untouched.
func TestCORSActualPOST(t *testing.T) {
	r := newCORSTestEngine()

	w := do(r, http.MethodPost, "/api/rooms/abc/answer", "http://localhost:5173")
	if w.Code != 200 {
		t.Fatalf("POST answer: code %d, want 200 (handler must still run)", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("POST answer ACAO = %q", got)
	}

	w = do(r, http.MethodPost, "/api/rooms/abc/answer", "http://evil.example")
	if w.Code != 200 {
		t.Fatalf("disallowed POST answer: code %d, want 200 (no CORS rejection)", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("disallowed POST ACAO = %q, want none", got)
	}

	w = do(r, http.MethodPost, "/api/rooms/abc/answer", "")
	if w.Code != 200 {
		t.Fatalf("origin-less POST: code %d, want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("origin-less POST ACAO = %q, want none", got)
	}
}
