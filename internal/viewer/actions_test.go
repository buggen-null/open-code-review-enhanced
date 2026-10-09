// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package viewer

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/model"
)

func TestDeleteSession(t *testing.T) {
	for _, tc := range []struct {
		name, origin, confirm, method string
		want                          int
	}{
		{"confirmed", "http://example.com", "delete", "DELETE", 204},
		{"missing origin", "", "delete", "DELETE", 403},
		{"foreign origin", "http://evil.example", "delete", "DELETE", 403},
		{"wrong port", "http://example.com:9", "delete", "DELETE", 403},
		{"wrong scheme", "https://example.com", "delete", "DELETE", 403},
		{"no confirmation", "http://example.com", "", "DELETE", 403},
		{"get cannot delete", "http://example.com", "delete", "GET", 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := compareFixture(t)
			mux := newMux(root)
			req := httptest.NewRequest(tc.method, "/r/myrepo/s1/delete", nil)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("X-OCR-Confirm", tc.confirm)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
			}
			_, err := os.Stat(filepath.Join(root, "myrepo", "s1.jsonl"))
			if tc.want == 204 && !os.IsNotExist(err) {
				t.Fatal("session was not deleted")
			}
			if tc.want != 204 && err != nil {
				t.Fatal("unconfirmed request removed record")
			}
			if _, err := os.Stat(filepath.Join(root, "myrepo", "s2.jsonl")); err != nil {
				t.Fatal("other session changed", err)
			}
			if tc.want == 204 {
				rr = httptest.NewRecorder()
				mux.ServeHTTP(rr, req)
				if rr.Code != 404 {
					t.Fatalf("repeated delete = %d", rr.Code)
				}
			}
		})
	}
}

func TestDeleteSessionUsesForwardedOrigin(t *testing.T) {
	root := compareFixture(t)
	req := httptest.NewRequest(http.MethodDelete, "/r/myrepo/s1/delete", nil)
	req.Host = "127.0.0.1:8200"
	req.Header.Set("Origin", "https://review.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "review.example.com")
	req.Header.Set("X-OCR-Confirm", "delete")
	rr := httptest.NewRecorder()
	newMux(root).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestSessionActionTraversal(t *testing.T) {
	mux := newMux(compareFixture(t))
	for _, path := range []string{"/r/myrepo/..%5Coutside/delete", "/r/myrepo/C%3Aoutside/delete", "/r/..%5Coutside/s1/export.md"} {
		method := http.MethodDelete
		if strings.HasSuffix(path, "export.md") {
			method = http.MethodGet
		}
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(method, path, nil))
		if rr.Code != 400 {
			t.Fatalf("%s = %d", path, rr.Code)
		}
	}
}

type failedMarkdownWriter struct{}

func (failedMarkdownWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestMarkdownExport(t *testing.T) {
	root := compareFixture(t)
	var out bytes.Buffer
	if err := ExportSessionMarkdown(&out, root, "myrepo", "s1"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Code Review Report", "Session: s1", "still broken", "was broken", "x := 1", "x := 2", "Findings: 2"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q", want)
		}
	}
	if err := ExportSessionMarkdown(failedMarkdownWriter{}, root, "myrepo", "s1"); err == nil {
		t.Fatal("writer failure ignored")
	}
	if err := ExportSessionMarkdown(&out, root, "myrepo", "missing"); err == nil {
		t.Fatal("missing session accepted")
	}
	out.Reset()
	if err := ExportSessionMarkdownWithLanguage(&out, root, "myrepo", "s1", "zh-CN"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# 代码审核报告", "## 摘要", "## 问题", "路径："} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("Chinese export missing %q", want)
		}
	}
	block := markdownBlock("```go\n<script>alert(1)</script>\n```")
	if !strings.HasPrefix(block, "````\n") || !strings.HasSuffix(block, "\n````\n\n") {
		t.Fatal(block)
	}
	mux := newMux(root)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/r/myrepo/s1/export.md", nil))
	if rr.Code != 200 || !strings.Contains(rr.Header().Get("Content-Disposition"), "review-s1.md") || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/markdown") {
		t.Fatal(rr)
	}
	r := httptest.NewRequest("GET", "/r/myrepo/s1/export?format=md&lang=zh-CN", nil)
	r.Header.Set("Accept-Language", "en-US")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, r)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "# 代码审核报告") {
		t.Fatalf("Chinese HTTP export = %d: %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/r/myrepo/missing/export.md", nil))
	if rr.Code != 404 {
		t.Fatal(rr.Code)
	}
}

func TestFixPriority(t *testing.T) {
	for _, tc := range []struct{ severity, category, want string }{
		{" HIGH ", "style", "Fix first"}, {"critical", "bug", "Fix first"},
		{"low", "security", "Fix first"}, {"medium", "style", "Recommended fix"},
		{"low", "bug", "Recommended fix"}, {"", "performance", "Recommended fix"},
		{"low", "documentation", "Review and schedule"}, {"unknown", "other", "Review and schedule"},
	} {
		if got := fixPriority(model.LlmComment{Severity: tc.severity, Category: tc.category}); got != tc.want {
			t.Errorf("%+v: %s", tc, got)
		}
	}
}

func TestExportLanguage(t *testing.T) {
	for _, tt := range []struct {
		input, want string
	}{
		{"en", "en"}, {"en-US", "en"}, {"zh-CN", "zh-CN"}, {"", "zh-CN"}, {"fr", "zh-CN"},
	} {
		if got := exportLanguageFromRequest(tt.input, "zh-CN"); got != tt.want {
			t.Errorf("exportLanguageFromRequest(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestEnhancedCompareAndSessionPages(t *testing.T) {
	mux := newMux(compareFixture(t))
	for _, tc := range []struct {
		path  string
		wants []string
	}{
		{"/r/myrepo/compare?before=s1&after=s2", []string{"处理建议", "Fix first", "未修复问题", "疑似已修复", "data-copy-path=", "审核后覆盖"}},
		{"/r/myrepo/compare?before=s1&after=legacy", []string{"Legacy session has no coverage manifest"}},
		{"/r/myrepo", []string{"name=\"before\"", "name=\"after\"", "data-delete-session=", "/export.md"}},
		{"/r/myrepo/s1", []string{"导出 Markdown", "删除会话", "data-copy-path=", "title=\"a.go\""}},
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("GET", tc.path, nil))
		if rr.Code != 200 {
			t.Fatal(rr.Code, rr.Body.String())
		}
		for _, want := range tc.wants {
			if !strings.Contains(rr.Body.String(), want) {
				t.Errorf("%s missing %q", tc.path, want)
			}
		}
	}
}
