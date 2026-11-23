package cache

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRoundTripAndPartialHits(t *testing.T) {
	url := os.Getenv("GATEWAY_TEST_REDIS")
	if url == "" {
		t.Skip("set GATEWAY_TEST_REDIS")
	}
	opt, _ := redis.ParseURL(url)
	rdb := redis.NewClient(opt)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	c := &Embeddings{RDB: rdb, TTL: time.Minute}
	if err := c.Put(ctx, "m", []string{"a", "c"}, []Entry{{Vector: []float32{0.5, -1}, Tokens: 3}, {Vector: []float32{3.25, 0}, Tokens: 7}}); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(ctx, "m", []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Vector[0] != 0.5 || got[0].Vector[1] != -1 || got[0].Tokens != 3 || got[1] != nil || got[2].Tokens != 7 {
		t.Errorf("got %+v %+v %+v", got[0], got[1], got[2])
	}
	if other, _ := c.Get(ctx, "other-model", []string{"a"}); other[0] != nil {
		t.Error("cache leaked across models")
	}
}

func TestEncoding(t *testing.T) {
	e := Entry{Vector: []float32{0, 1.5, -2.25, 3.4028235e38}, Tokens: 1 << 40}
	got := decode(encode(e))
	if got.Tokens != e.Tokens {
		t.Fatalf("tokens %d", got.Tokens)
	}
	for i := range e.Vector {
		if got.Vector[i] != e.Vector[i] {
			t.Fatalf("got %v", got.Vector)
		}
	}
}
