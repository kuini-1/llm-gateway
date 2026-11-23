// Package store keeps projects, API keys and the request log in Postgres.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
)

type Store struct {
	DB *pgxpool.Pool

	mu   sync.Mutex
	keys map[string]cachedKey // by hash
}

type cachedKey struct {
	k   *Key
	exp time.Time
}

func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{DB: pool, keys: map[string]cachedKey{}}, nil
}

func (s *Store) Close() { s.DB.Close() }

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func isUnique(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// ---- projects ----

type Project struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	BudgetUSD float64   `json:"monthly_budget_usd"`
	SpentUSD  float64   `json:"spent_this_month_usd"`
	Requests  int64     `json:"requests_this_month"`
	Keys      int       `json:"active_keys"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) CreateProject(ctx context.Context, name string, budget float64) (*Project, error) {
	p := &Project{ID: newID("prj_"), Name: name, BudgetUSD: budget}
	err := s.DB.QueryRow(ctx, `INSERT INTO projects (id, name, monthly_budget_usd) VALUES ($1, $2, $3) RETURNING created_at`,
		p.ID, name, budget).Scan(&p.CreatedAt)
	if isUnique(err) {
		return nil, ErrExists
	}
	return p, err
}

func (s *Store) SetProjectBudget(ctx context.Context, id string, budget float64) error {
	tag, err := s.DB.Exec(ctx, `UPDATE projects SET monthly_budget_usd = $2 WHERE id = $1`, id, budget)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.flushKeyCache()
	return err
}

func (s *Store) Projects(ctx context.Context, monthStart time.Time) ([]Project, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT p.id, p.name, p.monthly_budget_usd::float8, p.created_at,
		       COALESCE(r.cost, 0)::float8, COALESCE(r.n, 0),
		       (SELECT count(*) FROM api_keys k WHERE k.project_id = p.id AND k.revoked_at IS NULL)
		FROM projects p
		LEFT JOIN (SELECT project_id, sum(cost_usd) AS cost, count(*) AS n FROM requests WHERE ts >= $1 GROUP BY project_id) r
		       ON r.project_id = p.id
		ORDER BY p.created_at`, monthStart)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Project, error) {
		var p Project
		err := r.Scan(&p.ID, &p.Name, &p.BudgetUSD, &p.CreatedAt, &p.SpentUSD, &p.Requests, &p.Keys)
		return p, err
	})
}

// ---- API keys ----

type Key struct {
	ID            string     `json:"id"`
	ProjectID     string     `json:"project_id"`
	ProjectName   string     `json:"project_name"`
	Name          string     `json:"name"`
	Prefix        string     `json:"prefix"`
	BudgetUSD     float64    `json:"monthly_budget_usd"`
	ProjectBudget float64    `json:"-"`
	RPM           int        `json:"rpm"`
	CreatedAt     time.Time  `json:"created_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
}

var keyEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

func hashKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// CreateKey returns the key and its secret. The secret is shown once and
// only its hash is stored.
func (s *Store) CreateKey(ctx context.Context, projectID, name string, budget float64, rpm int) (*Key, string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return nil, "", err
	}
	secret := "gw_" + strings.ToLower(keyEnc.EncodeToString(b))
	k := &Key{ID: newID("key_"), ProjectID: projectID, Name: name, Prefix: secret[:10], BudgetUSD: budget, RPM: rpm}
	err := s.DB.QueryRow(ctx, `INSERT INTO api_keys (id, project_id, name, key_hash, prefix, monthly_budget_usd, rpm)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`,
		k.ID, projectID, name, hashKey(secret), k.Prefix, budget, rpm).Scan(&k.CreatedAt)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23503" {
		return nil, "", ErrNotFound // unknown project
	}
	return k, secret, err
}

// KeyBySecret authenticates a request. Results are cached for 30 seconds,
// so revocation takes effect within that time.
func (s *Store) KeyBySecret(ctx context.Context, secret string) (*Key, error) {
	h := hashKey(secret)
	s.mu.Lock()
	if c, ok := s.keys[h]; ok && time.Now().Before(c.exp) {
		s.mu.Unlock()
		if c.k == nil {
			return nil, ErrNotFound
		}
		return c.k, nil
	}
	s.mu.Unlock()

	k := &Key{}
	err := s.DB.QueryRow(ctx, `SELECT k.id, k.project_id, p.name, k.name, k.prefix, k.monthly_budget_usd::float8,
			p.monthly_budget_usd::float8, k.rpm, k.created_at
		FROM api_keys k JOIN projects p ON p.id = k.project_id
		WHERE k.key_hash = $1 AND k.revoked_at IS NULL`, h).
		Scan(&k.ID, &k.ProjectID, &k.ProjectName, &k.Name, &k.Prefix, &k.BudgetUSD, &k.ProjectBudget, &k.RPM, &k.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		k, err = nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if len(s.keys) > 10_000 {
		s.keys = map[string]cachedKey{}
	}
	s.keys[h] = cachedKey{k: k, exp: time.Now().Add(30 * time.Second)}
	s.mu.Unlock()
	return k, err
}

func (s *Store) flushKeyCache() {
	s.mu.Lock()
	s.keys = map[string]cachedKey{}
	s.mu.Unlock()
}

func (s *Store) Keys(ctx context.Context) ([]Key, error) {
	rows, err := s.DB.Query(ctx, `SELECT k.id, k.project_id, p.name, k.name, k.prefix, k.monthly_budget_usd::float8,
			k.rpm, k.created_at, k.revoked_at
		FROM api_keys k JOIN projects p ON p.id = k.project_id ORDER BY k.created_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Key, error) {
		var k Key
		err := r.Scan(&k.ID, &k.ProjectID, &k.ProjectName, &k.Name, &k.Prefix, &k.BudgetUSD, &k.RPM, &k.CreatedAt, &k.RevokedAt)
		return k, err
	})
}

func (s *Store) RevokeKey(ctx context.Context, id string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.flushKeyCache()
	return err
}

// MonthSpend is what a project or key has spent since monthStart, from the
// request log. It seeds the Redis spend counters.
func (s *Store) MonthSpend(ctx context.Context, kind, id string, monthStart time.Time) (float64, error) {
	col := "project_id"
	if kind == "key" {
		col = "key_id"
	}
	var v float64
	err := s.DB.QueryRow(ctx, `SELECT COALESCE(sum(cost_usd), 0)::float8 FROM requests WHERE `+col+` = $1 AND ts >= $2`, id, monthStart).Scan(&v)
	return v, err
}

// ---- request log ----

type Request struct {
	ID               string          `json:"id"`
	Time             time.Time       `json:"ts"`
	ProjectID        string          `json:"project_id"`
	KeyID            string          `json:"key_id"`
	Endpoint         string          `json:"endpoint"`
	Route            string          `json:"route"`
	Provider         string          `json:"provider"`
	Model            string          `json:"model"`
	Stream           bool            `json:"stream"`
	Status           string          `json:"status"`
	HTTPStatus       int             `json:"http_status"`
	ErrorKind        string          `json:"error_kind"`
	Error            string          `json:"error"`
	LatencyMs        int64           `json:"latency_ms"`
	TTFBMs           *int64          `json:"ttfb_ms"`
	InputTokens      int64           `json:"input_tokens"`
	OutputTokens     int64           `json:"output_tokens"`
	CacheReadTokens  int64           `json:"cache_read_tokens"`
	CacheWriteTokens int64           `json:"cache_write_tokens"`
	CostUSD          float64         `json:"cost_usd"`
	EmbInputs        int             `json:"emb_inputs"`
	EmbCacheHits     int             `json:"emb_cache_hits"`
	SavedUSD         float64         `json:"saved_usd"`
	FallbackFrom     string          `json:"fallback_from"`
	Attempts         json.RawMessage `json:"attempts"`
}

func NewRequestID() string { return newID("req_") }

var requestCols = []string{"id", "ts", "project_id", "key_id", "endpoint", "route", "provider", "model", "stream", "status",
	"http_status", "error_kind", "error", "latency_ms", "ttfb_ms", "input_tokens", "output_tokens", "cache_read_tokens",
	"cache_write_tokens", "cost_usd", "emb_inputs", "emb_cache_hits", "saved_usd", "fallback_from", "attempts"}

func (r *Request) row() []any {
	att := r.Attempts
	if len(att) == 0 {
		att = json.RawMessage("[]")
	}
	return []any{r.ID, r.Time, r.ProjectID, r.KeyID, r.Endpoint, r.Route, r.Provider, r.Model, r.Stream, r.Status,
		r.HTTPStatus, r.ErrorKind, r.Error, r.LatencyMs, r.TTFBMs, r.InputTokens, r.OutputTokens, r.CacheReadTokens,
		r.CacheWriteTokens, r.CostUSD, r.EmbInputs, r.EmbCacheHits, r.SavedUSD, r.FallbackFrom, string(att)}
}

// InsertRequests writes a batch with COPY.
func (s *Store) InsertRequests(ctx context.Context, rs []*Request) error {
	rows := make([][]any, len(rs))
	for i, r := range rs {
		rows[i] = r.row()
	}
	_, err := s.DB.CopyFrom(ctx, pgx.Identifier{"requests"}, requestCols, pgx.CopyFromRows(rows))
	return err
}

// Logger batches request rows off the request path: handlers enqueue and
// return; a goroutine writes every second or every 200 rows.
type Logger struct {
	store *Store
	ch    chan *Request
	done  chan struct{}
	OnErr func(error)
}

func NewLogger(s *Store) *Logger {
	l := &Logger{store: s, ch: make(chan *Request, 10_000), done: make(chan struct{})}
	go l.run()
	return l
}

// Log never blocks; if the buffer is full the row is dropped and reported.
func (l *Logger) Log(r *Request) {
	select {
	case l.ch <- r:
	default:
		if l.OnErr != nil {
			l.OnErr(errors.New("request log buffer full; dropped a row"))
		}
	}
}

func (l *Logger) run() {
	defer close(l.done)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var batch []*Request
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := l.store.InsertRequests(ctx, batch); err != nil && l.OnErr != nil {
			l.OnErr(fmt.Errorf("writing %d request rows: %w", len(batch), err))
		}
		cancel()
		batch = batch[:0]
	}
	for {
		select {
		case r, ok := <-l.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, r)
			if len(batch) >= 200 {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

// Close flushes pending rows.
func (l *Logger) Close() {
	close(l.ch)
	<-l.done
}
