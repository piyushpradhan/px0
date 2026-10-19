package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// forgeconnect.go is the server side of the Connect dialog: what the
// workspace's forge needs from the user (forgeMeta), and taking a token
// they pasted (handleForgeConnect) -- checked against the forge, stored in
// settings, and applied to a running PR review at once, so connecting never
// needs a restart.

// forgeTarget is the forge this workspace talks to: the PR's in a review
// session, otherwise the one the branch's remote points at. A plain
// workspace's target has only Provider and Host.
func (s *Server) forgeTarget() (GitProvider, PRTarget, bool) {
	if s.pr != nil {
		s.pr.mu.Lock()
		defer s.pr.mu.Unlock()
		return s.pr.provider, s.pr.target, s.pr.provider != nil
	}
	root := s.ix.Root()
	if !gitAvailable(root) {
		return nil, PRTarget{}, false
	}
	remote := gitRemoteURL(root, gitCurrentBranch(root))
	p := ForgeForRemote(remote)
	if p == nil {
		return nil, PRTarget{}, false
	}
	return p, PRTarget{Provider: p.Name(), Host: remoteHost(remote)}, true
}

// forgeMeta names the forge, whether px0 has a token for it (and from
// where), and how to get one. nil when the workspace has no forge px0 knows.
func (s *Server) forgeMeta() map[string]any {
	p, target, ok := s.forgeTarget()
	if !ok {
		return nil
	}
	source := ""
	if s.pr != nil {
		s.pr.mu.Lock()
		if s.pr.token != "" {
			source = s.pr.tokenSource
			if source == "" {
				source = "session"
			}
		}
		s.pr.mu.Unlock()
	} else {
		_, source = p.ResolveToken(readSettings(), target)
	}
	return map[string]any{
		"info":     p.Info(),
		"host":     target.Host,
		"hasToken": source != "",
		"source":   source, // settings, env, gh, glab, session
		"setup":    p.TokenSetup(target.Host),
	}
}

// handleForgeConnect takes a token from the Connect dialog. In a PR review it
// is checked by reading the PR with it; one the forge rejects is never kept.
// It is saved to settings unless that would replace a token saved for
// another host of the same forge, in which case it is used for this session
// only. Either way a running review starts using it immediately.
func (s *Server) handleForgeConnect(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) {
		return
	}
	var body struct {
		Token string `json:"token"`
		Extra string `json:"extra"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&body); err != nil || strings.TrimSpace(body.Token) == "" {
		fail(w, http.StatusBadRequest, "paste a token first")
		return
	}
	token, extra := strings.TrimSpace(body.Token), strings.TrimSpace(body.Extra)
	provider, target, ok := s.forgeTarget()
	if !ok {
		fail(w, http.StatusBadRequest, "this workspace's remote isn't on a forge px0 knows")
		return
	}
	info, setup := provider.Info(), provider.TokenSetup(target.Host)

	updates := map[string]any{info.TokenKey: token}
	if setup.Extra != nil {
		updates[setup.Extra.Key] = nilIfEmpty(extra)
	}
	persist := true
	if setup.HostKey != "" {
		// A token is already saved: keep it unless it is this host's own.
		if saved, _ := readSettingsRawMap()[info.TokenKey].(string); strings.TrimSpace(saved) != "" {
			if _, src := provider.ResolveToken(readSettings(), target); src != "settings" {
				persist = false
			}
		}
		updates[setup.HostKey] = target.Host
	}

	var prev map[string]any
	if persist {
		raw := readSettingsRawMap()
		prev = map[string]any{}
		for k := range updates {
			prev[k] = raw[k] // nil deletes on restore
		}
		if err := updateSettingsMap(updates); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	validated, writeAccess := false, false
	if s.pr != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if _, err := provider.FetchPR(ctx, target, token); err != nil {
			if persist {
				updateSettingsMap(prev)
			}
			fail(w, http.StatusBadRequest, info.Label+" didn't accept this token: "+redactToken(err.Error(), token))
			return
		}
		validated = true
		writeAccess = provider.CheckPushAccess(ctx, target, token)
		s.pr.mu.Lock()
		s.pr.token, s.pr.tokenSource, s.pr.writeAccess = token, "settings", writeAccess
		if !persist {
			s.pr.tokenSource = "session"
		}
		s.pr.mu.Unlock()
	}
	writeJSON(w, map[string]any{"ok": true, "saved": persist, "validated": validated, "writeAccess": writeAccess})
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
