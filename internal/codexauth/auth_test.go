package codexauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCredentialsPrefersPi(t *testing.T) {
	home := t.TempDir()
	writeAuth(t, filepath.Join(home, ".pi", "agent", "auth.json"), `{
		"openai-codex":{"type":"oauth","access":"pi-access","refresh":"pi-refresh","expires":4102444800,"accountId":"pi-account"}
	}`)
	writeAuth(t, filepath.Join(home, ".codex", "auth.json"), `{
		"tokens":{"access_token":"codex-access","refresh_token":"codex-refresh","account_id":"codex-account"}
	}`)

	source := &Source{HomeDir: home}
	got, err := source.Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "pi-access" || got.AccountID != "pi-account" {
		t.Fatalf("credentials = %+v, want Pi credentials", got)
	}
}

func TestCredentialsReadsCodexJWTClaims(t *testing.T) {
	home := t.TempDir()
	expires := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	writeAuth(t, filepath.Join(home, ".codex", "auth.json"), `{
		"auth_mode":"chatgpt",
		"tokens":{"access_token":"access","refresh_token":"refresh","id_token":"`+jwt(t, expires, "jwt-account")+`"}
	}`)

	source := &Source{HomeDir: home}
	got, err := source.Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "jwt-account" {
		t.Fatalf("credentials = %+v", got)
	}
	loaded, err := source.load()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.expiresAt.Equal(expires) {
		t.Fatalf("expires at = %v, want %v", loaded.expiresAt, expires)
	}
}

func TestCredentialsRefreshesAndPreservesPiFields(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`))
	}))
	defer srv.Close()

	path := filepath.Join(home, ".pi", "agent", "auth.json")
	writeAuth(t, path, `{
		"other":{"keep":true},
		"openai-codex":{"type":"oauth","access":"old-access","refresh":"old-refresh","expires":1,"accountId":"account","custom":"preserve"}
	}`)
	source := Source{HomeDir: home, HTTP: srv.Client(), TokenURL: srv.URL + "/oauth/token", now: func() time.Time { return now }}
	got, err := source.Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "new-access" || got.AccountID != "account" {
		t.Fatalf("credentials = %+v", got)
	}
	if form.Get("client_id") != clientID || form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "old-refresh" {
		t.Fatalf("refresh form = %v", form)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	var other map[string]bool
	if err := json.Unmarshal(stored["other"], &other); err != nil || !other["keep"] {
		t.Fatalf("unrelated fields were lost: %s", data)
	}
	var entry map[string]json.RawMessage
	if err := json.Unmarshal(stored["openai-codex"], &entry); err != nil {
		t.Fatal(err)
	}
	if string(entry["custom"]) != `"preserve"` || string(entry["access"]) != `"new-access"` || string(entry["refresh"]) != `"new-refresh"` {
		t.Fatalf("Pi fields = %s", stored["openai-codex"])
	}
	loaded, err := source.load()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.expiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("expires at = %v, want %v", loaded.expiresAt, now.Add(time.Hour))
	}
}

func TestCredentialsMissingDoesNotExposeTokens(t *testing.T) {
	home := t.TempDir()
	writeAuth(t, filepath.Join(home, ".pi", "agent", "auth.json"), `{
		"openai-codex":{"access":"secret-access","refresh":"secret-refresh"}
	}`)
	_, err := (&Source{HomeDir: home}).Credentials(context.Background())
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("error = %v, want login hint", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("token leaked in error: %v", err)
	}
}

func jwt(t *testing.T, expires time.Time, account string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, err := json.Marshal(map[string]any{
		"exp": expires.Unix(),
		"https://api.openai.com/auth": map[string]string{
			"chatgpt_account_id": account,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func writeAuth(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
