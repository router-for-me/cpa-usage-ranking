package test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"cpa-usage-ranking/internal/clients"
)

func TestBoundedUserListAndLiteralSearch(t *testing.T) {
	ctx := context.Background()
	db, repository := openRepository(t, filepath.Join(t.TempDir(), "list.db"))
	for i := 0; i < 105; i++ {
		client := create(t, db, fmt.Sprintf("list-%d", i))
		if i == 0 {
			if err := repository.SetDisabled(ctx, client.ID, true); err != nil {
				t.Fatal(err)
			}
			if err := repository.UpdateProfile(ctx, client.ID, `literal %_' OR 1=1 <img>`, 1); err != nil {
				t.Fatal(err)
			}
		}
	}
	first, err := repository.List(ctx, clients.ListQuery{})
	if err != nil || len(first.Clients) != 50 || first.NextID != 50 {
		t.Fatal("default page", err)
	}
	seen := map[int64]bool{}
	cursor := int64(0)
	for {
		page, err := repository.List(ctx, clients.ListQuery{After: cursor, Limit: 17})
		if err != nil {
			t.Fatal(err)
		}
		for _, client := range page.Clients {
			if seen[client.ID] || client.ID <= cursor {
				t.Fatal("unstable pagination")
			}
			seen[client.ID] = true
		}
		if page.NextID == 0 {
			break
		}
		cursor = page.NextID
	}
	if len(seen) != 105 {
		t.Fatal("missing users")
	}
	banned := true
	page, err := repository.List(ctx, clients.ListQuery{Disabled: &banned})
	if err != nil || len(page.Clients) != 1 || !page.Clients[0].Disabled {
		t.Fatal("banned user not discoverable", err)
	}
	page, err = repository.List(ctx, clients.ListQuery{Name: `%_' OR 1=1`})
	if err != nil || len(page.Clients) != 1 || page.Clients[0].ID != 1 {
		t.Fatal("search interpreted SQL/pattern", err)
	}
	page, err = repository.List(ctx, clients.ListQuery{ID: 105})
	if err != nil || len(page.Clients) != 1 || page.Clients[0].ID != 105 {
		t.Fatal("exact id", err)
	}
	page, err = repository.List(ctx, clients.ListQuery{Name: "no match"})
	if err != nil || len(page.Clients) != 0 {
		t.Fatal("empty result", err)
	}
	for _, query := range []clients.ListQuery{{Limit: 101}, {Limit: -1}, {ID: -1}, {After: -1}, {ID: 1, Name: "ambiguous"}} {
		if _, err := repository.List(ctx, query); !errors.Is(err, clients.ErrQuery) {
			t.Fatal("invalid query accepted")
		}
	}
}
