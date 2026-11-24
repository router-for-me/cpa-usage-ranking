package test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"cpa-usage-ranking/internal/clients"
)

func TestUnifiedSearchIncludesNumericNamesAndExactIDs(t *testing.T) {
	db, repository := openRepository(t, filepath.Join(t.TempDir(), "search.db"))
	_, err := db.Writer.Exec(`INSERT INTO clients(id,github_id,server_id,display_name,avatar_id,ranking_hidden,created_at_ms) VALUES
	(7,7,'550e8400-e29b-41d4-a716-446655440000','target',1,0,0),(11,11,'550e8400-e29b-41d4-a716-446655440000','7',1,1,0),(12,12,'550e8400-e29b-41d4-a716-446655440000','77',1,1,0),
	(13,13,'550e8400-e29b-41d4-a716-446655440000','literal %_ quote''',1,0,0),(14,14,'550e8400-e29b-41d4-a716-446655440000','9007199254740993',1,0,0),
	(9007199254740993,9007199254740993,'550e8400-e29b-41d4-a716-446655440000','large-id',1,0,0)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		query string
		count int
	}{{"7", 4}, {"9007199254740993", 2}, {"%_", 1}, {"quote'", 1}, {"' OR 1=1", 0}, {"missing", 0}} {
		page, err := repository.List(context.Background(), clients.ListQuery{Search: test.query})
		if err != nil || len(page.Clients) != test.count {
			t.Fatalf("query %q: count=%d err=%v", test.query, len(page.Clients), err)
		}
	}
	hidden := true
	page, err := repository.List(context.Background(), clients.ListQuery{Search: "7", Hidden: &hidden, Limit: 1, After: 7})
	if err != nil || len(page.Clients) != 1 || page.Clients[0].ID != 11 || page.NextID != 11 {
		t.Fatal("hidden filter or bounded cursor lost", err)
	}
	page, err = repository.List(context.Background(), clients.ListQuery{Search: "7", Hidden: &hidden, Limit: 1, After: page.NextID})
	if err != nil || len(page.Clients) != 1 || page.Clients[0].ID != 12 || page.NextID != 0 {
		t.Fatal("unstable second page", err)
	}
	page, err = repository.List(context.Background(), clients.ListQuery{Search: "7", ID: 7})
	if err != nil || len(page.Clients) != 1 || page.Clients[0].ID != 7 {
		t.Fatal("combined search and id filter failed", err)
	}
	page, err = repository.List(context.Background(), clients.ListQuery{Search: "7", Name: "target"})
	if err != nil || len(page.Clients) != 1 || page.Clients[0].ID != 7 {
		t.Fatal("combined search and name filter failed", err)
	}
	for _, query := range []clients.ListQuery{{Search: strings.Repeat("a", 81)}, {Search: "\xff"}} {
		if _, err := repository.List(context.Background(), query); !errors.Is(err, clients.ErrQuery) {
			t.Fatal("invalid search accepted", err)
		}
	}
}
