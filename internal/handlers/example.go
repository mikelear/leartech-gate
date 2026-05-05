package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ExampleHandler is the placeholder authenticated endpoint for the template.
// Delete this and add your real handlers when cloning.
type ExampleHandler struct {
	pool *pgxpool.Pool
}

// NewExampleHandler constructs an ExampleHandler.
func NewExampleHandler(pool *pgxpool.Pool) *ExampleHandler {
	return &ExampleHandler{pool: pool}
}

// RegisterRoutes wires the example routes onto the given router group.
func (h *ExampleHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/example", h.get)
}

// ExampleResponse is returned by GET /api/v1/example.
type ExampleResponse struct {
	Message string `json:"message" example:"hello"`
	Service string `json:"service" example:"leartech-gate"`
}

// get returns a static example response — replace with real business logic.
//
// @Summary Example endpoint
// @Tags example
// @Security BearerAuth
// @Produce json
// @Success 200	{object}	ExampleResponse
// @Failure 401	{object}	map[string]string
// @Router /api/v1/example [get]
func (h *ExampleHandler) get(c *gin.Context) {
	c.JSON(http.StatusOK, ExampleResponse{
		Message: "hello",
		Service: "leartech-gate",
	})
}
