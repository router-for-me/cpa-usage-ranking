package test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"cpa-usage-ranking/internal/httpserver"
)

func TestAdminRankingVisibilityPermissionsAndIndependentStates(t *testing.T) {
	var refreshed atomic.Int64
	router, service, db := adminDatabaseRouter(t, httpserver.AdminHTTPOptions{RequestRefresh: func() { refreshed.Add(1) }})
	if _, err := db.Writer.Exec(`INSERT INTO clients(github_id,server_id,display_name,avatar_id,created_at_ms) VALUES(1,'550e8400-e29b-41d4-a716-446655440000','one',1,123),(2,'550e8400-e29b-41d4-a716-446655440000','two',1,456)`); err != nil {
		t.Fatal(err)
	}
	ownerCookie, owner := adminLoginCookie(t, service, "owner", "isolated-test-password")
	account, err := service.CreateAccount(context.Background(), owner.Identity, "staff", "ordinary-test-password")
	if err != nil {
		t.Fatal(err)
	}
	cookie, staff := adminLoginCookie(t, service, "staff", "ordinary-test-password")
	for _, actor := range []struct{ cookie, csrf string }{{ownerCookie, owner.CSRF}, {cookie, staff.CSRF}} {
		for _, body := range []string{`{"hidden":true}`, `{"hidden":true}`, `{"hidden":false}`, `{"hidden":false}`} {
			if got := adminCall(router, "PUT", "/v1/admin/users/1/ranking-visibility", body, actor.cookie, actor.csrf, "https://ranking.test", true).Code; got != 200 {
				t.Fatal(got)
			}
		}
	}
	if refreshed.Load() != 8 {
		t.Fatal("no refresh for committed exclusion")
	}
	if got := adminCall(router, "PUT", "/v1/admin/users/1/ranking-visibility", `{"hidden":true}`, cookie, staff.CSRF, "https://ranking.test", true).Code; got != 200 {
		t.Fatal(got)
	}
	if got := adminCall(router, "PUT", "/v1/admin/users/1/ban", `{"disabled":true}`, cookie, staff.CSRF, "https://ranking.test", true).Code; got != 200 {
		t.Fatal(got)
	}
	// 封禁与下榜彼此独立，允许在封禁状态下任意切换 ranking_hidden。
	if got := adminCall(router, "PUT", "/v1/admin/users/1/ranking-visibility", `{"hidden":false}`, cookie, staff.CSRF, "https://ranking.test", true).Code; got != 200 {
		t.Fatal("independent hidden clear failed", got)
	}
	if got := adminCall(router, "PUT", "/v1/admin/users/1/ranking-visibility", `{"hidden":true}`, cookie, staff.CSRF, "https://ranking.test", true).Code; got != 200 {
		t.Fatal(got)
	}
	list := adminCall(router, "GET", "/v1/admin/users?hidden=true", "", cookie, "", "", true)
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"ranking_hidden":true`) || !strings.Contains(list.Body.String(), `"disabled":true`) {
		t.Fatal("hidden/banned list", list.Body.String())
	}
	if got := adminCall(router, "GET", "/v1/admin/users?hidden=", "", cookie, "", "", true).Code; got != 200 {
		t.Fatal("empty hidden query rejected", got)
	}
	for _, query := range []string{"hidden=1", "hidden=true&hidden=false"} {
		if got := adminCall(router, "GET", "/v1/admin/users?"+query, "", cookie, "", "", true).Code; got != 400 {
			t.Fatal("invalid hidden query accepted", got)
		}
	}
	for _, scenario := range []struct {
		path, body, cookie, csrf string
		want                     int
	}{
		{"1", `{"hidden":true}`, "", "", 401},
		{"1", `{"hidden":true}`, ownerCookie, "", 403},
		{"999", `{"hidden":true}`, ownerCookie, owner.CSRF, 404},
		{"0", `{"hidden":true}`, ownerCookie, owner.CSRF, 400},
		{"1", `{}`, ownerCookie, owner.CSRF, 400},
		{"1", `{"hidden":null}`, ownerCookie, owner.CSRF, 400},
		{"1", `{"hidden":"true"}`, ownerCookie, owner.CSRF, 400},
		{"1", `{"hidden":true,"disabled":false}`, ownerCookie, owner.CSRF, 400},
	} {
		if got := adminCall(router, "PUT", "/v1/admin/users/"+scenario.path+"/ranking-visibility", scenario.body, scenario.cookie, scenario.csrf, "https://ranking.test", true).Code; got != scenario.want {
			t.Fatal(got, scenario.want)
		}
	}
	// Deletion also invalidates subsequent exclusion writes (not only ban writes).
	if _, err := db.Writer.Exec("DELETE FROM admin_accounts WHERE id=?", account.ID); err != nil {
		t.Fatal(err)
	}
	if got := adminCall(router, "PUT", "/v1/admin/users/2/ranking-visibility", `{"hidden":true}`, cookie, staff.CSRF, "https://ranking.test", true).Code; got != 401 {
		t.Fatal("deleted admin changed visibility", got)
	}
}
