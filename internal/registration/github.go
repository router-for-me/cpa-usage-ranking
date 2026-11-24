package registration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cpa-usage-ranking/internal/profiles"
)

const (
	githubAuthorize = "https://github.com/login/oauth/authorize"
	githubToken     = "https://github.com/login/oauth/access_token"
	githubUserURL   = "https://api.github.com/user"
	authorizePath   = "/auth/github/authorize"
	callbackPath    = "/auth/github/callback"
)

type GitHubOptions struct {
	PublicOrigin string
	ClientID     string
	ClientSecret string
}

func (o GitHubOptions) Validate() error {
	u, ok := httpURL(o.PublicOrigin)
	if !ok || u.Scheme != "https" || (u.Path != "" && u.Path != "/") || strings.ContainsAny(o.PublicOrigin, "?#") {
		return errors.New("RANKING_PUBLIC_ORIGIN must be an HTTPS origin without path, query or fragment")
	}
	if o.ClientID == "" || o.ClientSecret == "" || strings.TrimSpace(o.ClientID) != o.ClientID || strings.TrimSpace(o.ClientSecret) != o.ClientSecret {
		return errors.New("RANKING_GITHUB_CLIENT_ID and RANKING_GITHUB_CLIENT_SECRET must be non-empty")
	}
	return nil
}

type GitHub struct {
	options GitHubOptions
	http    http.Client
}

// NewGitHub 固定生产端点；测试只能注入传输，不从环境变量重定向 OAuth 请求。
func NewGitHub(options GitHubOptions, transport http.RoundTripper) (*GitHub, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	options.PublicOrigin = strings.TrimSuffix(options.PublicOrigin, "/")
	return &GitHub{options: options, http: http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}, nil
}

func (g *GitHub) BrowserURL(requestID string) string {
	return g.options.PublicOrigin + authorizePath + "?" + url.Values{"request_id": {requestID}}.Encode()
}

func (g *GitHub) AuthorizeURL(auth Authorization) string {
	return githubAuthorize + "?" + url.Values{
		"client_id":             {g.options.ClientID},
		"redirect_uri":          {g.options.PublicOrigin + callbackPath},
		"state":                 {auth.State},
		"code_challenge":        {auth.Challenge},
		"code_challenge_method": {"S256"},
	}.Encode()
}

type githubUser struct {
	id          int64
	displayName string
}

func (g *GitHub) exchange(ctx context.Context, code, verifier string) (githubUser, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	form := url.Values{
		"client_id":     {g.options.ClientID},
		"client_secret": {g.options.ClientSecret},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {g.options.PublicOrigin + callbackPath},
	}
	tokenRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, githubToken, strings.NewReader(form.Encode()))
	if err != nil {
		return githubUser{}, ErrGitHub
	}
	tokenRequest.Header.Set("Accept", "application/json")
	tokenRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var token struct {
		Value string `json:"access_token"`
	}
	if err := g.readJSON(tokenRequest, &token); err != nil || token.Value == "" {
		return githubUser{}, ErrGitHub
	}
	userRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, githubUserURL, nil)
	if err != nil {
		return githubUser{}, ErrGitHub
	}
	userRequest.Header.Set("Accept", "application/vnd.github+json")
	userRequest.Header.Set("Authorization", "Bearer "+token.Value)
	var user struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Login string `json:"login"`
	}
	if err := g.readJSON(userRequest, &user); err != nil || user.ID <= 0 {
		return githubUser{}, ErrGitHub
	}
	name, err := profiles.NormalizeDisplayName(user.Name)
	if err != nil {
		name, err = profiles.NormalizeDisplayName(user.Login)
		if err != nil {
			return githubUser{}, ErrGitHub
		}
	}
	return githubUser{id: user.ID, displayName: name}, nil
}

// 请求失败一律返回无敏感内容的错误，不转发上游正文、URL、授权码或 token。
func (g *GitHub) readJSON(req *http.Request, value any) error {
	res, err := g.http.Do(req)
	if err != nil {
		return ErrGitHub
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return ErrGitHub
	}
	const maxResponse = 64 << 10
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil || len(body) > maxResponse || json.Unmarshal(body, value) != nil {
		return ErrGitHub
	}
	return nil
}
