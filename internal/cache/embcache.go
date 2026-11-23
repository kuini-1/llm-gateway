// Package cache stores embedding vectors in Redis, keyed by model and a hash
// of the exact input text, so repeated inputs (re-indexing unchanged
// documents, popular queries) are never paid for twice.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
)

type Embeddings struct {
	RDB *redis.Client
	TTL time.Duration
}

// Key is per model, not per provider: the same model served by two
// providers returns the same vectors.
func Key(model, text string) string {
	sum := sha256.Sum256([]byte(text))
	return "emb:" + model + ":" + hex.EncodeToString(sum[:])
}

// Entry is a cached vector and the tokens the provider billed for it, so a
// hit can be credited with what it actually saved.
type Entry struct {
	Vector []float32
	Tokens int64
}

// Get returns an entry for every input; nil where there was no hit.
func (c *Embeddings) Get(ctx context.Context, model string, inputs []string) ([]*Entry, error) {
	out := make([]*Entry, len(inputs))
	if len(inputs) == 0 {
		return out, nil
	}
	keys := make([]string, len(inputs))
	for i, in := range inputs {
		keys[i] = Key(model, in)
	}
	vals, err := c.RDB.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	for i, v := range vals {
		if s, ok := v.(string); ok && len(s) >= 8 {
			out[i] = decode([]byte(s))
		}
	}
	return out, nil
}

// Put stores entries in one pipeline.
func (c *Embeddings) Put(ctx context.Context, model string, inputs []string, entries []Entry) error {
	if len(inputs) != len(entries) {
		return errors.New("inputs and entries differ in length")
	}
	pipe := c.RDB.Pipeline()
	for i, in := range inputs {
		pipe.Set(ctx, Key(model, in), encode(entries[i]), c.TTL)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// Layout: 8-byte token count, then float32 little-endian values (4 bytes
// per dimension, about a quarter of the JSON size).
func encode(e Entry) []byte {
	b := make([]byte, 8+4*len(e.Vector))
	binary.LittleEndian.PutUint64(b, uint64(e.Tokens))
	for i, x := range e.Vector {
		binary.LittleEndian.PutUint32(b[8+4*i:], math.Float32bits(x))
	}
	return b
}

func decode(b []byte) *Entry {
	e := &Entry{Tokens: int64(binary.LittleEndian.Uint64(b)), Vector: make([]float32, (len(b)-8)/4)}
	for i := range e.Vector {
		e.Vector[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[8+4*i:]))
	}
	return e
}
