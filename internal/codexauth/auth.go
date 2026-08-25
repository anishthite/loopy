// Package codexauth reads the local OAuth state created by Pi or Codex.
package codexauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	refreshURL = "https://auth.openai.com/oauth/token"
	clientID   = "app_EMoamEEZ73f0CkXaXp7hrann"
)

var ErrLoginRequired = errors.New("codex authentication not found; run pi /login openai-codex or codex login")

// Credentials are the non-persisted fields needed for one Codex request.
// RefreshToken deliberately stays private so callers cannot accidentally log it.
type Credentials struct {
	AccessToken string
	AccountID   string
}

// Source reads Pi auth first and falls back to Codex CLI auth. The exported
// transport fields make the package testable without touching a real login.
type Source struct {
	HomeDir  string
	HTTP     *http.Client
	TokenURL string

	mu  sync.Mutex
	now func() time.Time
}

// Available reports whether a usable local login exists. It does not refresh:
// startup remains local and a near-expiry token is refreshed when it is used.
func (s *Source) Available() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.load()
	return err
}

// Credentials returns current credentials, refreshing tokens that are expired
// or within five minutes of expiry. Refreshed values are persisted atomically
// because refresh tokens may rotate.
func (s *Source) Credentials(ctx context.Context) (Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.load()
	if err != nil {
		return Credentials{}, err
	}
	if c.access != "" && (c.expiresAt.IsZero() || c.expiresAt.After(s.clock().Add(5*time.Minute))) {
		return c.credentials(), nil
	}
	if c.refresh == "" {
		return Credentials{}, ErrLoginRequired
	}
	if err := s.refresh(ctx, c); err != nil {
		return Credentials{}, err
	}
	return c.credentials(), nil
}

type authKind uint8

const (
	piAuth authKind = iota
	codexAuth
)

type candidate struct {
	kind authKind
	path string
	root map[string]json.RawMessage

	access, refresh, idToken, accountID string
	expiresAt                           time.Time
	expiresMillis                       bool
}

type tokenClaims struct {
	Exp  int64 `json:"exp"`
	Auth struct {
		ChatGPTAccountID string `json:"chatgpt_account_id"`
	} `json:"https://api.openai.com/auth"`
}

func (c candidate) credentials() Credentials {
	return Credentials{AccessToken: c.access, AccountID: c.accountID}
}

func (s *Source) load() (*candidate, error) {
	home := s.HomeDir
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, ErrLoginRequired
		}
	}

	paths := []struct {
		kind authKind
		path string
	}{
		{kind: piAuth, path: filepath.Join(home, ".pi", "agent", "auth.json")},
		{kind: codexAuth, path: filepath.Join(home, ".codex", "auth.json")},
	}
	for _, p := range paths {
		c, err := loadFile(p.kind, p.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			continue // another source may contain the user's usable login
		}
		c.fillJWTClaims()
		if c.accountID != "" && (c.access != "" || c.refresh != "") {
			return c, nil
		}
	}
	return nil, ErrLoginRequired
}

func loadFile(kind authKind, path string) (*candidate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	root := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	c := &candidate{kind: kind, path: path, root: root}

	if kind == piAuth {
		entry, ok := root["openai-codex"]
		if !ok {
			return nil, ErrLoginRequired
		}
		fields := map[string]json.RawMessage{}
		if err := json.Unmarshal(entry, &fields); err != nil {
			return nil, err
		}
		c.access = stringField(fields, "access")
		c.refresh = stringField(fields, "refresh")
		c.accountID = stringField(fields, "accountId")
		c.expiresAt, c.expiresMillis = expiryField(fields["expires"])
		return c, nil
	}

	tokens, ok := root["tokens"]
	if !ok {
		return nil, ErrLoginRequired
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(tokens, &fields); err != nil {
		return nil, err
	}
	c.access = stringField(fields, "access_token")
	c.refresh = stringField(fields, "refresh_token")
	c.idToken = stringField(fields, "id_token")
	c.accountID = stringField(fields, "account_id")
	return c, nil
}

func stringField(fields map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(fields[key], &value)
	return value
}

func expiryField(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 {
		return time.Time{}, false
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		if n, err := strconv.ParseInt(number.String(), 10, 64); err == nil {
			if n > 1_000_000_000_000 {
				return time.UnixMilli(n), true
			}
			return time.Unix(n, 0), false
		}
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if t, err := time.Parse(time.RFC3339, text); err == nil {
			return t, false
		}
	}
	return time.Time{}, false
}

func (c *candidate) fillJWTClaims() {
	for _, token := range []string{c.access, c.idToken} {
		claims, ok := jwtClaims(token)
		if !ok {
			continue
		}
		if c.expiresAt.IsZero() && claims.Exp > 0 {
			c.expiresAt = time.Unix(claims.Exp, 0)
		}
		if c.accountID == "" {
			c.accountID = claims.Auth.ChatGPTAccountID
		}
	}
}

func jwtClaims(token string) (tokenClaims, bool) {
	var claims tokenClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return claims, false
		}
	}
	if json.Unmarshal(payload, &claims) != nil {
		return claims, false
	}
	return claims, true
}

func (s *Source) refresh(ctx context.Context, c *candidate) error {
	form := url.Values{
		"client_id":     {clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.refresh},
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL(), bytes.NewBufferString(form.Encode()))
	if err != nil {
		return fmt.Errorf("refresh codex login: %w", err)
	}
	hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.httpClient().Do(hr)
	if err != nil {
		return errors.New("could not refresh codex login; run pi /login openai-codex or codex login")
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return errors.New("could not refresh codex login; run pi /login openai-codex or codex login")
	}
	var out struct {
		AccessToken  string          `json:"access_token"`
		RefreshToken string          `json:"refresh_token"`
		IDToken      string          `json:"id_token"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil || out.AccessToken == "" {
		return errors.New("could not refresh codex login; run pi /login openai-codex or codex login")
	}
	c.access = out.AccessToken
	if out.RefreshToken != "" {
		c.refresh = out.RefreshToken
	}
	if out.IDToken != "" {
		c.idToken = out.IDToken
	}
	c.expiresAt = expiryFromDuration(out.ExpiresIn, s.clock())
	c.fillJWTClaims()
	if c.accountID == "" {
		return errors.New("could not determine codex account; run pi /login openai-codex or codex login")
	}
	if c.expiresAt.IsZero() {
		return errors.New("could not determine codex token expiry; run pi /login openai-codex or codex login")
	}
	if err := c.save(s.clock()); err != nil {
		return fmt.Errorf("save refreshed codex login: %w", err)
	}
	return nil
}

func expiryFromDuration(raw json.RawMessage, now time.Time) time.Time {
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		if seconds, err := strconv.ParseInt(n.String(), 10, 64); err == nil && seconds > 0 {
			return now.Add(time.Duration(seconds) * time.Second)
		}
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if seconds, err := strconv.ParseInt(text, 10, 64); err == nil && seconds > 0 {
			return now.Add(time.Duration(seconds) * time.Second)
		}
	}
	return time.Time{}
}

func (c *candidate) save(now time.Time) error {
	if c.kind == piAuth {
		fields := map[string]json.RawMessage{}
		if err := json.Unmarshal(c.root["openai-codex"], &fields); err != nil {
			return err
		}
		fields["access"] = marshalRaw(c.access)
		fields["refresh"] = marshalRaw(c.refresh)
		fields["accountId"] = marshalRaw(c.accountID)
		if c.expiresMillis {
			fields["expires"] = marshalRaw(c.expiresAt.UnixMilli())
		} else {
			fields["expires"] = marshalRaw(c.expiresAt.Unix())
		}
		c.root["openai-codex"] = marshalRaw(fields)
	} else {
		fields := map[string]json.RawMessage{}
		if err := json.Unmarshal(c.root["tokens"], &fields); err != nil {
			return err
		}
		fields["access_token"] = marshalRaw(c.access)
		fields["refresh_token"] = marshalRaw(c.refresh)
		if c.idToken != "" {
			fields["id_token"] = marshalRaw(c.idToken)
		}
		fields["account_id"] = marshalRaw(c.accountID)
		c.root["tokens"] = marshalRaw(fields)
		c.root["last_refresh"] = marshalRaw(now.UTC().Format(time.RFC3339))
	}
	data, err := json.MarshalIndent(c.root, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(c.path, append(data, '\n'))
}

func marshalRaw(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".loopy-codex-auth-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func (s *Source) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Source) httpClient() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (s *Source) tokenURL() string {
	if s.TokenURL != "" {
		return s.TokenURL
	}
	return refreshURL
}
