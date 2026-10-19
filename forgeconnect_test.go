package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// connectProvider is a GitLab provider whose API answers are canned: one
// token is accepted and has push access, everything else is rejected.
type connectProvider struct {
	GitLabProvider
	good string
}

func (c *connectProvider) FetchPR(ctx context.Context, t PRTarget, token string) (PRMeta, error) {
	if token != c.good {
		return PRMeta{}, errors.New("gitlab: 401 Unauthorized (token " + token + ")")
	}
	return PRMeta{Number: t.Number}, nil
}

func (c *connectProvider) CheckPushAccess(ctx context.Context, t PRTarget, token string) bool {
	return token == c.good
}

func connectServer(t *testing.T, host string) *Server {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GITLAB_HOST", "")
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("PATH", t.TempDir()) // no glab
	s, _ := newTestServer(t)
	s.pr = &prSession{
		provider: &connectProvider{good: "good-token"},
		target:   PRTarget{Provider: "gitlab", Host: host, Owner: "g", Repo: "r", Number: 4},
		meta:     PRMeta{Number: 4},
	}
	return s
}

func TestForgeConnectAppliesAndSaves(t *testing.T) {
	s := connectServer(t, "gitlab.example.com")
	w := postJSON(t, s, "/api/forge/connect", `{"token":"good-token"}`)
	if w.Code != 200 {
		t.Fatalf("connect: %d %s", w.Code, w.Body)
	}
	var res map[string]any
	json.Unmarshal(w.Body.Bytes(), &res)
	if res["saved"] != true || res["validated"] != true || res["writeAccess"] != true {
		t.Errorf("result = %v", res)
	}
	// Live: the running review uses it now, with no restart.
	if s.pr.token != "good-token" || !s.pr.writeAccess {
		t.Errorf("session token=%q writeAccess=%v", s.pr.token, s.pr.writeAccess)
	}
	// Saved, along with the host it belongs to, so the next session finds it.
	cfg := readSettings()
	if cfg.GitLabToken == nil || *cfg.GitLabToken != "good-token" || cfg.GitLabHost == nil || *cfg.GitLabHost != "gitlab.example.com" {
		t.Errorf("settings = token %v host %v", cfg.GitLabToken, cfg.GitLabHost)
	}
	if tok, src := (&GitLabProvider{}).ResolveToken(cfg, PRTarget{Host: "gitlab.example.com"}); tok != "good-token" || src != "settings" {
		t.Errorf("resolve after connect = %q/%q", tok, src)
	}
	meta := s.forgeMeta()
	if meta["hasToken"] != true || meta["source"] != "settings" {
		t.Errorf("forgeMeta = %v", meta)
	}
}

func TestForgeConnectRejectsBadToken(t *testing.T) {
	s := connectServer(t, "gitlab.com")
	w := postJSON(t, s, "/api/forge/connect", `{"token":"bad-token"}`)
	if w.Code != 400 {
		t.Fatalf("bad token accepted: %d %s", w.Code, w.Body)
	}
	if body := w.Body.String(); strings.Contains(body, "bad-token") {
		t.Errorf("error echoes the token: %s", body)
	}
	if cfg := readSettings(); cfg.GitLabToken != nil || cfg.GitLabHost != nil {
		t.Errorf("rejected token was kept: %v %v", cfg.GitLabToken, cfg.GitLabHost)
	}
	if s.pr.token != "" {
		t.Errorf("session took a rejected token")
	}
	if w := postJSON(t, s, "/api/forge/connect", `{"token":"  "}`); w.Code != 400 {
		t.Errorf("empty token: %d", w.Code)
	}
}

// A token already saved for one GitLab host is not replaced by connecting to
// another: the new one serves this session only.
func TestForgeConnectKeepsOtherHostsToken(t *testing.T) {
	s := connectServer(t, "gitlab.other.org")
	if err := updateSettingsMap(map[string]any{"gitlab.token": "corp-token", "gitlab.host": "gitlab.corp.io"}); err != nil {
		t.Fatal(err)
	}
	w := postJSON(t, s, "/api/forge/connect", `{"token":"good-token"}`)
	var res map[string]any
	json.Unmarshal(w.Body.Bytes(), &res)
	if w.Code != 200 || res["saved"] != false {
		t.Fatalf("connect: %d %v", w.Code, res)
	}
	cfg := readSettings()
	if *cfg.GitLabToken != "corp-token" || *cfg.GitLabHost != "gitlab.corp.io" {
		t.Errorf("other host's token replaced: %v %v", *cfg.GitLabToken, *cfg.GitLabHost)
	}
	if s.pr.token != "good-token" || s.forgeMeta()["source"] != "session" {
		t.Errorf("session token = %q, source %v", s.pr.token, s.forgeMeta()["source"])
	}
}

func TestTokenSetupPerForge(t *testing.T) {
	cases := []struct {
		p       GitProvider
		host    string
		url     string
		hostKey string
	}{
		{&GitHubProvider{}, "github.com", "https://github.com/settings/personal-access-tokens/new", ""},
		{&GitLabProvider{}, "gitlab.corp.io", "https://gitlab.corp.io/-/user_settings/personal_access_tokens", "gitlab.host"},
		{&GiteaProvider{}, "git.example.org", "https://git.example.org/user/settings/applications", "gitea.host"},
		{&BitbucketProvider{}, "bitbucket.org", "https://id.atlassian.com/manage-profile/security/api-tokens", ""},
	}
	for _, c := range cases {
		st := c.p.TokenSetup(c.host)
		if st.URL != c.url || st.HostKey != c.hostKey || len(st.Steps) == 0 || st.Env == "" || st.Readonly == "" {
			t.Errorf("%s: %+v", c.p.Name(), st)
		}
		// Every setting the dialog writes must exist in the Settings schema.
		for _, key := range []string{c.p.Info().TokenKey, st.HostKey} {
			if key != "" && !schemaHas(key) {
				t.Errorf("%s: setting %q missing from the schema", c.p.Name(), key)
			}
		}
		if st.Extra != nil && !schemaHas(st.Extra.Key) {
			t.Errorf("%s: setting %q missing from the schema", c.p.Name(), st.Extra.Key)
		}
	}
}

func schemaHas(key string) bool {
	for _, it := range settingsSchema {
		if it.Key == key {
			return true
		}
	}
	return false
}
