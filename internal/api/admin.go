package api

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kuini-1/llm-gateway/internal/store"
)

func (g *Gateway) adminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/overview", g.admin(g.overview))
	mux.HandleFunc("GET /admin/api/requests", g.admin(g.recent))
	mux.HandleFunc("GET /admin/api/projects", g.admin(g.listProjects))
	mux.HandleFunc("POST /admin/api/projects", g.admin(g.createProject))
	mux.HandleFunc("PATCH /admin/api/projects/{id}", g.admin(g.updateProject))
	mux.HandleFunc("GET /admin/api/keys", g.admin(g.listKeys))
	mux.HandleFunc("POST /admin/api/keys", g.admin(g.createKey))
	mux.HandleFunc("DELETE /admin/api/keys/{id}", g.admin(g.revokeKey))
	if g.Dashboard != nil {
		mux.Handle("GET /admin/", http.StripPrefix("/admin/", http.FileServer(http.FS(g.Dashboard))))
		mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/admin/", http.StatusFound) })
	}
}

func (g *Gateway) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if g.AdminToken == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(g.AdminToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "admin token required")
			return
		}
		next(w, r)
	}
}

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func (g *Gateway) overview(w http.ResponseWriter, r *http.Request) {
	window, bucket := time.Hour, "minute"
	switch r.URL.Query().Get("window") {
	case "24h":
		window, bucket = 24*time.Hour, "hour"
	case "7d":
		window, bucket = 7*24*time.Hour, "hour"
	}
	ctx := r.Context()
	stats, err := g.Store.Stats(ctx, time.Now().Add(-window), bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	projects, err := g.Store.Projects(ctx, monthStart(time.Now()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if projects == nil {
		projects = []store.Project{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stats": stats, "projects": projects, "targets": g.Router.Status(), "routes": g.Router.Models(),
	})
}

func (g *Gateway) recent(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := g.Store.RecentRequests(r.Context(), r.URL.Query().Get("filter"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if rows == nil {
		rows = []store.Request{}
	}
	writeJSON(w, http.StatusOK, rows)
}

func (g *Gateway) listProjects(w http.ResponseWriter, r *http.Request) {
	ps, err := g.Store.Projects(r.Context(), monthStart(time.Now()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if ps == nil {
		ps = []store.Project{}
	}
	writeJSON(w, http.StatusOK, ps)
}

func (g *Gateway) createProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string  `json:"name"`
		BudgetUSD float64 `json:"monthly_budget_usd"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" || body.BudgetUSD < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "name is required and the budget can't be negative")
		return
	}
	p, err := g.Store.CreateProject(r.Context(), strings.TrimSpace(body.Name), body.BudgetUSD)
	if errors.Is(err, store.ErrExists) {
		writeError(w, http.StatusConflict, "exists", "a project with this name exists")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (g *Gateway) updateProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BudgetUSD float64 `json:"monthly_budget_usd"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.BudgetUSD < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "the budget can't be negative")
		return
	}
	if err := g.Store.SetProjectBudget(r.Context(), r.PathValue("id"), body.BudgetUSD); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "project not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) listKeys(w http.ResponseWriter, r *http.Request) {
	ks, err := g.Store.Keys(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if ks == nil {
		ks = []store.Key{}
	}
	writeJSON(w, http.StatusOK, ks)
}

func (g *Gateway) createKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProjectID string  `json:"project_id"`
		Name      string  `json:"name"`
		BudgetUSD float64 `json:"monthly_budget_usd"`
		RPM       int     `json:"rpm"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.ProjectID == "" || strings.TrimSpace(body.Name) == "" || body.BudgetUSD < 0 || body.RPM < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "project_id and name are required; budget and rpm can't be negative")
		return
	}
	k, secret, err := g.Store.CreateKey(r.Context(), body.ProjectID, strings.TrimSpace(body.Name), body.BudgetUSD, body.RPM)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "project not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": k, "secret": secret,
		"note": "Store the secret now; it is not shown again."})
}

func (g *Gateway) revokeKey(w http.ResponseWriter, r *http.Request) {
	if err := g.Store.RevokeKey(r.Context(), r.PathValue("id")); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "key not found or already revoked")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
