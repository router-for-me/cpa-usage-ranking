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
	_, err := db.Writer.Exec(`INSERT INTO clients(id,github_id,server_id,display_name,avatar_id,ranking_hidden,withdrawn,created_at_ms) VALUES
	(7,7001,'550e8400-e29b-41d4-a816-446655440000','target',1,0,0,0),(11,1101,'550e8400-e29b-41d4-a816-446655440001','7',1,1,0,0),(12,1201,'550e8400-e29b-41d4-a816-446655440002','77',1,1,1,0),
	(13,1301,'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee','literal %_ quote''',1,0,0,0),(14,1401,'550e8400-e29b-41d4-a816-446655440004','9007199254740993',1,0,0,0),
	(9007199254740993,9007199254740994,'550e8400-e29b-41d4-a816-446655440005','large-id',1,0,0,0)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		query string
		count int
	}{
		{"7", 4},
		{"  target  ", 1},
		{"7001", 1},
		{"9007199254740994", 1},
		{"9007199254740993", 2},
		{"BBBB-CCCC", 1},
		{"446655440002", 1},
		{"%_", 1},
		{"quote'", 1},
		{"' OR 1=1", 0},
		{"missing", 0},
	} {
		page, err := repository.List(context.Background(), clients.ListQuery{Search: test.query})
		if err != nil || len(page.Clients) != test.count {
			t.Fatalf("query %q: count=%d err=%v", test.query, len(page.Clients), err)
		}
	}
	withdrawn := true
	wPage, err := repository.List(context.Background(), clients.ListQuery{Withdrawn: &withdrawn})
	if err != nil || len(wPage.Clients) != 1 || wPage.Clients[0].ID != 12 || !wPage.Clients[0].Withdrawn || wPage.Clients[0].GitHubID != 1201 || wPage.Clients[0].ServerID != "550e8400-e29b-41d4-a816-446655440002" {
		t.Fatalf("withdrawn filter or metadata: %+v err=%v", wPage, err)
	}
	notWithdrawn := false
	nwPage, err := repository.List(context.Background(), clients.ListQuery{Withdrawn: &notWithdrawn})
	if err != nil || len(nwPage.Clients) != 5 {
		t.Fatalf("not-withdrawn filter: %d err=%v", len(nwPage.Clients), err)
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
