package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestExampleHandler_Get confirms GET /api/v1/example returns the expected
// placeholder JSON. Replace this when the example handler is deleted.
func TestExampleHandler_Get(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewExampleHandler(nil)
	r := gin.New()
	rg := r.Group("/api/v1")
	h.RegisterRoutes(rg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/example", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp ExampleResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if resp.Message != "hello" {
		t.Errorf("message=%q want=hello", resp.Message)
	}
	if resp.Service != "leartech-gate" {
		t.Errorf("service=%q want=leartech-gate", resp.Service)
	}
}
