package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type Totals struct {
	Requests     int64   `json:"requests"`
	Errors       int64   `json:"errors"`
	Fallbacks    int64   `json:"fallbacks"`
	CostUSD      float64 `json:"cost_usd"`
	SavedUSD     float64 `json:"saved_usd"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	EmbInputs    int64   `json:"emb_inputs"`
	EmbHits      int64   `json:"emb_cache_hits"`
	P50Ms        float64 `json:"p50_ms"`
	P95Ms        float64 `json:"p95_ms"`
}

type TargetStats struct {
	Provider  string  `json:"provider"`
	Model     string  `json:"model"`
	Requests  int64   `json:"requests"`
	Errors    int64   `json:"errors"`
	CostUSD   float64 `json:"cost_usd"`
	P50Ms     float64 `json:"p50_ms"`
	P95Ms     float64 `json:"p95_ms"`
	P50TTFBMs float64 `json:"p50_ttfb_ms"`
}

type Bucket struct {
	Start    time.Time `json:"start"`
	Requests int64     `json:"requests"`
	Errors   int64     `json:"errors"`
	CostUSD  float64   `json:"cost_usd"`
	P50Ms    float64   `json:"p50_ms"`
}

type Stats struct {
	Since   time.Time     `json:"since"`
	Bucket  string        `json:"bucket"`
	Totals  Totals        `json:"totals"`
	Targets []TargetStats `json:"targets"`
	Series  []Bucket      `json:"series"`
}

// Stats summarises the log since a time, with a time series bucketed by
// minute or hour.
func (s *Store) Stats(ctx context.Context, since time.Time, bucket string) (*Stats, error) {
	if bucket != "minute" {
		bucket = "hour"
	}
	st := &Stats{Since: since, Bucket: bucket, Targets: []TargetStats{}, Series: []Bucket{}}
	t := &st.Totals
	err := s.DB.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE status = 'error'), count(*) FILTER (WHERE fallback_from <> ''),
		       COALESCE(sum(cost_usd), 0)::float8, COALESCE(sum(saved_usd), 0)::float8,
		       COALESCE(sum(input_tokens), 0), COALESCE(sum(output_tokens), 0),
		       COALESCE(sum(emb_inputs), 0), COALESCE(sum(emb_cache_hits), 0),
		       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms), 0),
		       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0)
		FROM requests WHERE ts >= $1`, since).
		Scan(&t.Requests, &t.Errors, &t.Fallbacks, &t.CostUSD, &t.SavedUSD, &t.InputTokens, &t.OutputTokens,
			&t.EmbInputs, &t.EmbHits, &t.P50Ms, &t.P95Ms)
	if err != nil {
		return nil, err
	}

	rows, err := s.DB.Query(ctx, `
		SELECT provider, model, count(*), count(*) FILTER (WHERE status = 'error'), COALESCE(sum(cost_usd), 0)::float8,
		       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE status = 'ok'), 0),
		       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE status = 'ok'), 0),
		       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY ttfb_ms) FILTER (WHERE ttfb_ms IS NOT NULL), 0)
		FROM requests WHERE ts >= $1 AND provider <> ''
		GROUP BY provider, model ORDER BY count(*) DESC`, since)
	if err != nil {
		return nil, err
	}
	if st.Targets, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (TargetStats, error) {
		var x TargetStats
		err := r.Scan(&x.Provider, &x.Model, &x.Requests, &x.Errors, &x.CostUSD, &x.P50Ms, &x.P95Ms, &x.P50TTFBMs)
		return x, err
	}); err != nil {
		return nil, err
	}

	rows, err = s.DB.Query(ctx, `
		SELECT date_trunc('`+bucket+`', ts) AS b, count(*), count(*) FILTER (WHERE status = 'error'),
		       COALESCE(sum(cost_usd), 0)::float8,
		       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms), 0)
		FROM requests WHERE ts >= $1 GROUP BY b ORDER BY b`, since)
	if err != nil {
		return nil, err
	}
	st.Series, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Bucket, error) {
		var x Bucket
		err := r.Scan(&x.Start, &x.Requests, &x.Errors, &x.CostUSD, &x.P50Ms)
		return x, err
	})
	return st, err
}

// RecentRequests lists the latest log rows, optionally only errors or only
// requests that fell back.
func (s *Store) RecentRequests(ctx context.Context, filter string, limit int) ([]Request, error) {
	where := "true"
	switch filter {
	case "error":
		where = "status = 'error'"
	case "fallback":
		where = "fallback_from <> ''"
	}
	rows, err := s.DB.Query(ctx, `SELECT id, ts, project_id, key_id, endpoint, route, provider, model, stream, status,
			http_status, error_kind, error, latency_ms, ttfb_ms, input_tokens, output_tokens, cache_read_tokens,
			cache_write_tokens, cost_usd::float8, emb_inputs, emb_cache_hits, saved_usd::float8, fallback_from, attempts
		FROM requests WHERE `+where+` ORDER BY ts DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Request, error) {
		var x Request
		var att []byte
		err := r.Scan(&x.ID, &x.Time, &x.ProjectID, &x.KeyID, &x.Endpoint, &x.Route, &x.Provider, &x.Model, &x.Stream,
			&x.Status, &x.HTTPStatus, &x.ErrorKind, &x.Error, &x.LatencyMs, &x.TTFBMs, &x.InputTokens, &x.OutputTokens,
			&x.CacheReadTokens, &x.CacheWriteTokens, &x.CostUSD, &x.EmbInputs, &x.EmbCacheHits, &x.SavedUSD,
			&x.FallbackFrom, &att)
		x.Attempts = att
		return x, err
	})
}
