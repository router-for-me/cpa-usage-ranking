package test

import (
	"context"
	"sync/atomic"
	"testing"

	"cpa-usage-ranking/internal/httpserver"
)

func TestAdminBanHTTPAndRefreshNotification(t *testing.T) {
	var notifications atomic.Int64
	router, service, db := adminDatabaseRouter(t, httpserver.AdminHTTPOptions{RequestRefresh: func() { notifications.Add(1) }})
	if _, err := db.Writer.Exec(`INSERT INTO clients(github_id,server_id,display_name,avatar_id,created_at_ms) VALUES(1,'550e8400-e29b-41d4-a716-446655440000','one',1,123),(2,'550e8400-e29b-41d4-a716-446655440000','two',1,456)`); err != nil {
		t.Fatal(err)
	}
	ownerCookie, owner := adminLoginCookie(t, service, "owner", "isolated-test-password")
	if _, err := service.CreateAccount(context.Background(), owner.Identity, "staff", "ordinary-test-password"); err != nil {
		t.Fatal(err)
	}
	cookie, staff := adminLoginCookie(t, service, "staff", "ordinary-test-password")
	for _, actor := range []struct{ cookie, csrf string }{{ownerCookie, owner.CSRF}, {cookie, staff.CSRF}} {
		for _, body := range []string{`{"disabled":true}`, `{"disabled":true}`, `{"disabled":false}`, `{"disabled":false}`} {
			response := adminCall(router, "PUT", "/v1/admin/users/1/ban", body, actor.cookie, actor.csrf, "https://ranking.test", true)
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
		}
	}
	if notifications.Load() != 8 {
		t.Fatal("committed changes did not request refresh")
	}
	for _, scenario := range []struct {
		path, body, cookie, csrf, origin string
		want                             int
	}{
		{"1", `{"disabled":true}`, "", "", "https://ranking.test", 401},
		{"1", `{"disabled":true}`, ownerCookie, "", "https://ranking.test", 403},
		{"1", `{"disabled":true}`, ownerCookie, owner.CSRF, "https://evil.test", 403},
		{"999", `{"disabled":true}`, ownerCookie, owner.CSRF, "https://ranking.test", 404},
		{"0", `{"disabled":true}`, ownerCookie, owner.CSRF, "https://ranking.test", 400},
		{"1", `{}`, ownerCookie, owner.CSRF, "https://ranking.test", 400},
		{"1", `{"disabled":null}`, ownerCookie, owner.CSRF, "https://ranking.test", 400},
		{"1", `{"disabled":"true"}`, ownerCookie, owner.CSRF, "https://ranking.test", 400},
		{"1", `{"disabled":true}{}`, ownerCookie, owner.CSRF, "https://ranking.test", 400},
	} {
		if got := adminCall(router, "PUT", "/v1/admin/users/"+scenario.path+"/ban", scenario.body, scenario.cookie, scenario.csrf, scenario.origin, true).Code; got != scenario.want {
			t.Fatal("unexpected ban status", got, scenario.want)
		}
	}
	if notifications.Load() != 8 {
		t.Fatal("failed write requested refresh")
	}
}
