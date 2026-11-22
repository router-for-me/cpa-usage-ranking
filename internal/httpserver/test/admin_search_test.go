package test

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAdminUnifiedSearchHTTP(t *testing.T) {
	router, service, db := adminDatabaseRouter(t)
	if _, err := db.Writer.Exec(`INSERT INTO clients(id,github_id,cpa_uuid,display_name,avatar_id,created_at_ms) VALUES(1,1,'550e8400-e29b-41d4-a716-446655440000','100',1,0),(100,100,'550e8400-e29b-41d4-a716-446655440000','target',1,0),(9007199254740993,9007199254740993,'550e8400-e29b-41d4-a716-446655440000','large',1,0)`); err != nil {
		t.Fatal(err)
	}
	cookie, _ := adminLoginCookie(t, service, "owner", "isolated-test-password")
	for _, scenario := range []struct {
		query string
		count int
	}{{"q=100", 2}, {"q=9007199254740993", 1}, {"q=target", 1}, {"q=%27+OR+1%3D1", 0}, {"q=none", 0}} {
		response := adminCall(router, "GET", "/v1/admin/users?"+scenario.query, "", cookie, "", "", true)
		var data struct {
			Users []struct {
				ID string `json:"id"`
			}
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &data) != nil || len(data.Users) != scenario.count {
			t.Fatal("search response", response.Code, response.Body.String())
		}
		if scenario.query == "q=9007199254740993" && data.Users[0].ID != "9007199254740993" {
			t.Fatal("ID precision lost")
		}
	}
	for _, query := range []string{"q=x&q=y", "q=x&id=1", "q=x&name=y", "q=" + strings.Repeat("x", 81)} {
		if got := adminCall(router, "GET", "/v1/admin/users?"+query, "", cookie, "", "", true).Code; got != 400 {
			t.Fatal("invalid query accepted", got)
		}
	}
	if got := adminCall(router, "GET", "/v1/admin/users?q=100", "", "", "", "", true).Code; got != 401 {
		t.Fatal("search bypassed authentication", got)
	}
}
