package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func TestPostgresAuthoritySerializesAndSeparatesDomains(t *testing.T) {
	dsn := os.Getenv("WB2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("WB2API_TEST_POSTGRES_DSN is required")
	}
	ctx := context.Background()
	domain := fmt.Sprintf("coord-test/%d", time.Now().UnixNano())
	a, err := OpenPostgres(ctx, dsn, domain)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenPostgres(ctx, dsn, domain)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	other, err := OpenPostgres(ctx, dsn, domain+"/other")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := a
			if i%2 == 1 {
				s = b
			}
			errs <- s.Update(ctx, "counter", func(raw json.RawMessage) (json.RawMessage, error) {
				n := 0
				if string(raw) != "null" {
					if err := json.Unmarshal(raw, &n); err != nil {
						return nil, err
					}
				}
				return json.Marshal(n + 1)
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, err := a.View(ctx, "counter")
	if err != nil || string(raw) != "20" {
		t.Fatalf("lost concurrent update: %s %v", raw, err)
	}
	marker := errors.New("rollback")
	if err = a.Update(ctx, "counter", func(json.RawMessage) (json.RawMessage, error) { return json.RawMessage("999"), marker }); !errors.Is(err, marker) {
		t.Fatal(err)
	}
	raw, err = b.View(ctx, "counter")
	if err != nil || string(raw) != "20" {
		t.Fatalf("callback error committed: %s %v", raw, err)
	}
	raw, err = other.View(ctx, "counter")
	if err != nil || string(raw) != "null" {
		t.Fatalf("domain leaked: %s %v", raw, err)
	}
}

func TestPostgresAuthorityWriteAndDeadline(t *testing.T) {
	dsn := os.Getenv("WB2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("WB2API_TEST_POSTGRES_DSN is required")
	}
	ctx := context.Background()
	s, err := OpenPostgres(ctx, dsn, fmt.Sprintf("coord-tx/%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, err := s.Begin(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = tx.Write(json.RawMessage(`{"first":true}`)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Write(json.RawMessage(`{"second":true}`)); err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err = s.Begin(blocked, "k"); err == nil {
		t.Fatal("row lock ignored caller deadline")
	}
	if err = tx.CommitCurrent(); err != nil {
		t.Fatal(err)
	}
	raw, err := s.View(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]bool
	if err = json.Unmarshal(raw, &got); err != nil || !got["second"] || got["first"] {
		t.Fatalf("write did not retain final state: %s %v", raw, err)
	}
}
