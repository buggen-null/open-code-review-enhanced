// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package viewer

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForwardedPrefixHTML(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<link href="/static/style.css"><a href="/r/repo">repo</a>`))
	})
	req := httptest.NewRequest(http.MethodGet, "http://viewer/", nil)
	req.Header.Set("X-Forwarded-Prefix", "/code-audit")
	rec := httptest.NewRecorder()

	forwardedPrefixHTML(next).ServeHTTP(rec, req)

	want := `<link href="/code-audit/static/style.css"><a href="/code-audit/r/repo">repo</a>`
	if rec.Body.String() != want {
		t.Fatalf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestForwardedPrefixHTMLLeavesUnprefixedResponsesUntouched(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<script src="/static/app.js"></script>`))
	})
	req := httptest.NewRequest(http.MethodGet, "http://viewer/", nil)
	rec := httptest.NewRecorder()

	forwardedPrefixHTML(next).ServeHTTP(rec, req)

	if got := rec.Body.String(); got != `<script src="/static/app.js"></script>` {
		t.Fatalf("body = %q, want root-relative asset URL", got)
	}
}

func TestPrefixRootRelativeURLsDoesNotDuplicatePrefix(t *testing.T) {
	got := prefixRootRelativeURLs(`<a href="/code-audit/r/repo"><img src="/static/app.js">`, "/code-audit")
	want := `<a href="/code-audit/r/repo"><img src="/code-audit/static/app.js">`
	if got != want {
		t.Fatalf("prefixRootRelativeURLs() = %q, want %q", got, want)
	}
}

func TestForwardedPrefixHTMLPrefixesRenderedRepositoryLinksOnce(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "session.jsonl"), []byte(`{"type":"session_start"}`), 0600); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://viewer/", nil)
	req.Header.Set("X-Forwarded-Prefix", "/code-audit")
	rec := httptest.NewRecorder()
	forwardedPrefixHTML(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleRepos(w, r, root)
	})).ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `href="/code-audit/r/repo"`) {
		t.Fatalf("repository link was not prefixed: %s", body)
	}
	if strings.Contains(body, "/code-audit/code-audit/") {
		t.Fatalf("repository link contains a duplicated prefix: %s", body)
	}
}
