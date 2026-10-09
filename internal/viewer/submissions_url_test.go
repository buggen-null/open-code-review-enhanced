// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package viewer

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTasksRedirectPathKeepsProxyPrefix(t *testing.T) {
	r := httptest.NewRequest("POST", "http://viewer/code-audit/submissions/id/cancel", nil)
	r.Header.Set("X-Forwarded-Prefix", "/code-audit/")
	if got := tasksRedirectPath(r); got != "/code-audit/tasks" {
		t.Fatalf("tasksRedirectPath() = %q, want /code-audit/tasks", got)
	}
}

func TestSubmitFormDefaultsPromptToChecked(t *testing.T) {
	for _, tc := range []struct {
		name, query, want string
	}{
		{name: "new submission", want: `name="default_prompt" type="checkbox" checked`},
		{name: "retry preserves disabled value", query: "?default_prompt=0", want: `name="default_prompt" type="checkbox"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &reviewQueue{repoRoot: t.TempDir()}
			r := httptest.NewRequest("GET", "http://viewer/submit"+tc.query, nil)
			rr := httptest.NewRecorder()
			handleReviewSubmissions(rr, r, q)
			body := rr.Body.String()
			if tc.name == "new submission" {
				if !strings.Contains(body, tc.want) {
					t.Fatalf("default prompt checkbox was not checked")
				}
			} else if !strings.Contains(body, tc.want) || strings.Contains(body, tc.want+" checked") {
				t.Fatalf("disabled default prompt was not preserved")
			}
		})
	}
}

func TestValidGitURL(t *testing.T) {
	for _, raw := range []string{
		"http://git.example.test/team/project.git",
		"https://git.example.test/team/project.git",
		"ssh://git.example.test/team/project.git",
		"git@git.example.test:team/project.git",
	} {
		if !validGitURL(raw) {
			t.Errorf("validGitURL(%q) = false", raw)
		}
	}
	for _, raw := range []string{"/opt/project", "D:\\project", "ftp://git.example.test/project.git", "http://"} {
		if validGitURL(raw) {
			t.Errorf("validGitURL(%q) = true", raw)
		}
	}
}
