package test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"cpa-usage-ranking/internal/auth"
)

func TestRegistrationHTTPResponsesAndLogsRedactSecrets(t *testing.T) {
	f := newRegistrationFixture(t, defaultRegistrationSettings())
	secret := testSecret(1)
	otherSecret := testSecret(2)
	seeded := f.seed(t, secret)
	hash, err := auth.HashCredential(secret)
	if err != nil {
		t.Fatal(err)
	}
	var responses strings.Builder
	for _, tc := range []struct{ body, credential string }{
		{`{"display_name":"Name"}`, secret + "x"},
		{fmt.Sprintf(`{"secret":%q}`, secret), otherSecret},
		{fmt.Sprintf(`{"display_name":%q}`, secret), otherSecret},
	} {
		r := f.send(t, tc.body, tc.credential, nil)
		if r.status < 400 {
			t.Fatal("malicious input succeeded")
		}
		responses.Write(r.body)
	}
	if err := f.repo.SetDisabled(context.Background(), seeded.ID, true); err != nil {
		t.Fatal(err)
	}
	id := f.start(t, secret, cpaUUID, returnURL)
	f.approve(t, id, 1, "Name", "name", returnURL)
	r := f.complete(t, secret, id)
	requireHTTPError(t, r, 403, "client_disabled")
	responses.Write(r.body)
	otherID := f.start(t, otherSecret, cpaUUID, returnURL)
	f.approve(t, otherID, 2, "Name", "name", returnURL)
	if _, err := f.db.Writer.Exec(fmt.Sprintf(`CREATE TRIGGER sensitive_failure BEFORE INSERT ON client_writers BEGIN SELECT RAISE(ABORT, '%s'); END`, otherSecret)); err != nil {
		t.Fatal(err)
	}
	r = f.complete(t, otherSecret, otherID)
	requireHTTPError(t, r, 500, "internal_error")
	responses.Write(r.body)
	var seats int
	if err := f.db.Reader.QueryRow("SELECT count(*) FROM clients").Scan(&seats); err != nil || seats != 1 {
		t.Fatalf("failed writer insert leaked a seat: %d %v", seats, err)
	}
	// 重复 Authorization 头必须拒绝，不任选其一。
	request, err := http.NewRequest("POST", f.server.URL+startPath, strings.NewReader(validStartBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Add("Authorization", "Bearer "+secret)
	request.Header.Add("Authorization", "Bearer "+otherSecret)
	request.Header.Set("Content-Type", "application/json")
	response, err := f.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 401 {
		t.Fatal("ambiguous Authorization accepted")
	}
	responses.Write(body)
	// 仅测试异常路由：复用生产恢复器，不能打印 panic 值、URL、请求头或正文。
	request, err = http.NewRequest("POST", f.server.URL+"/test/secret-panic?credential="+secret, strings.NewReader(otherSecret))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+secret)
	response, err = f.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 500 {
		t.Fatal("panic recovery failed")
	}
	responses.Write(body)
	output := responses.String() + f.logs.String()
	for _, sensitive := range []string{secret, otherSecret, fmt.Sprintf("%x", hash), "sensitive_failure", "Authorization", "credential="} {
		if strings.Contains(output, sensitive) {
			t.Fatal("sensitive request/database data leaked in response or logs")
		}
	}
	if !strings.Contains(f.logs.String(), "http handler panicked") {
		t.Fatal("panic log safety path was not exercised")
	}
}
