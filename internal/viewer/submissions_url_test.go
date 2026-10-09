// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package viewer

import (
	"net/http/httptest"
	"testing"
)

func TestTasksRedirectPathKeepsProxyPrefix(t *testing.T) {
	r := httptest.NewRequest("POST", "http://viewer/code-audit/submissions/id/cancel", nil)
	r.Header.Set("X-Forwarded-Prefix", "/code-audit/")
	if got := tasksRedirectPath(r); got != "/code-audit/tasks" {
		t.Fatalf("tasksRedirectPath() = %q, want /code-audit/tasks", got)
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
