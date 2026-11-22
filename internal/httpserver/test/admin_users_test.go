package test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAdminUserListHTTP(t *testing.T) {
	router, service, db := adminDatabaseRouter(t)
	if _, err := db.Writer.Exec(`INSERT INTO clients(id,github_id,cpa_uuid,display_name,avatar_id,disabled,created_at_ms) VALUES(1,1,'550e8400-e29b-41d4-a716-446655440000','no reports',1,0,123),(9007199254740993,9007199254740993,'550e8400-e29b-41d4-a716-446655440000','literal %_ <img>',1,1,456)`); err != nil {
		t.Fatal(err)
	}
	ownerCookie, owner := adminLoginCookie(t, service, "owner", "isolated-test-password")
	if _, err := service.CreateAccount(context.Background(), owner.Identity, "staff", "ordinary-test-password"); err != nil {
		t.Fatal(err)
	}
	staffCookie, _ := adminLoginCookie(t, service, "staff", "ordinary-test-password")
	for _, cookie := range []string{ownerCookie, staffCookie} {
		response := adminCall(router, "GET", "/v1/admin/users?limit=1", "", cookie, "", "", true)
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
		var page struct {
			Users []struct {
				ID       string `json:"id"`
				Disabled bool   `json:"disabled"`
			}
			Next string `json:"next_cursor"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Users) != 1 || page.Users[0].ID != "1" || page.Next != "1" {
			t.Fatal("bad first page")
		}
		next := adminCall(router, "GET", "/v1/admin/users?after=1", "", cookie, "", "", true)
		if err := json.Unmarshal(next.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Users) != 1 || page.Users[0].ID != "9007199254740993" || !page.Users[0].Disabled || page.Next != "" {
			t.Fatal("ID precision or banned user missing")
		}
		for _, secret := range []string{"password", "hash", "token", "revision", "staff"} {
			if strings.Contains(next.Body.String(), secret) {
				t.Fatal("private data leaked")
			}
		}
		for _, query := range []string{"disabled=true", "name=%25_", "id=9007199254740993"} {
			if got := adminCall(router, "GET", "/v1/admin/users?"+query, "", cookie, "", "", true).Code; got != 200 {
				t.Fatal(got)
			}
		}
	}
	for _, query := range []string{"limit=101", "limit=0", "limit=-1", "id=0", "id=90071992547409930000", "after=+1", "disabled=1", "disabled=", "limit=1&limit=2", "unknown=x", "id=1&name=one"} {
		if got := adminCall(router, "GET", "/v1/admin/users?"+query, "", ownerCookie, "", "", true).Code; got != 400 {
			t.Fatalf("accepted %q: %d", query, got)
		}
	}
	for _, cookie := range []string{"", "ranking_admin=forged"} {
		if got := adminCall(router, "GET", "/v1/admin/users", "", cookie, "", "", true).Code; got != 401 {
			t.Fatal("anonymous list exposed", got)
		}
	}
}
