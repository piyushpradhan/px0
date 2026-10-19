package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// forgehttp.go is the JSON-over-HTTPS plumbing the Gitea and Bitbucket
// providers share: one request, a 2xx decoded into the caller's shape,
// anything else an error carrying the forge's own message.

var forgeHTTPClient = &http.Client{Timeout: 15 * time.Second}

// forgeAPI is one forge's API root, auth header and error-body reader.
type forgeAPI struct {
	name    string // for error text: "gitea", "bitbucket"
	base    string // e.g. "https://codeberg.org/api/v1"
	auth    string // full Authorization header value, "" for anonymous
	message func(body []byte) string
}

// do issues method on path (relative to base, or an absolute URL) and
// decodes a 2xx reply into out when it is non-nil. The status is returned
// on error too, so callers can tell "not mergeable" from "head moved".
func (a forgeAPI) do(ctx context.Context, method, path string, body, out any) (int, error) {
	u := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		u = a.base + path
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if a.auth != "" {
		req.Header.Set("Authorization", a.auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := forgeHTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(b))
		if a.message != nil {
			if m := a.message(b); m != "" {
				msg = m
			}
		}
		return resp.StatusCode, fmt.Errorf("%s: %s %s: %s: %s", a.name, method, path, resp.Status, msg)
	}
	if out != nil && len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}
