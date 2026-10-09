// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package viewer

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"time"

	"github.com/alibaba/open-code-review/internal/model"
	"github.com/alibaba/open-code-review/internal/session"
)

func handleSessionProgress(w http.ResponseWriter, r *http.Request, root, repo, sessionID string) {
	setNoStore(w)
	path := filepath.Join(root, repo, sessionID+".jsonl")
	summary, err := peekSession(path)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	total := summary.SelectedCount
	if total == 0 {
		total = summary.FileCount
	}
	duration := summary.DurationSec
	if summary.Aborted && !summary.Timestamp.IsZero() {
		duration = time.Since(summary.Timestamp).Seconds()
	}
	state := summary.TerminalState
	label := statusLabel(state)
	if summary.Aborted {
		state = "active"
		label = "审核中" // allow-non-english: localized viewer status label
	} else if summary.Legacy || state == "" {
		state = "legacy"
		label = statusLabel(state)
	}
	payload := map[string]any{
		"percent":    summary.ProgressPercent(),
		"total":      total,
		"completed":  summary.CompletedCount,
		"processing": summary.ActiveCount,
		"reused":     summary.ReusedCount,
		"failed":     summary.FailedCount,
		"active":     summary.Aborted,
		"findings":   summary.CommentCount,
		"duration":   formatDuration(duration),
		"state":      state,
		"label":      label,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(payload)
}

func handleCompareExport(w http.ResponseWriter, r *http.Request, root, repo string) {
	compareOutput(w, r, root, repo, "html")
	return
}

func handleCompareMarkdownExport(w http.ResponseWriter, r *http.Request, root, repo string) {
	compareOutput(w, r, root, repo, "markdown")
}

func compareOutput(w http.ResponseWriter, r *http.Request, root, repo, format string) {
	rr := httptest.NewRecorder()
	handleCompareFormat(rr, r, root, repo, format == "html", format == "markdown")
	if rr.Code != http.StatusOK {
		http.Error(w, rr.Body.String(), rr.Code)
		return
	}
	if format == "markdown" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="compare-%s-%s.md"`, r.URL.Query().Get("before"), r.URL.Query().Get("after")))
	} else {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="compare-%s-%s.html"`, r.URL.Query().Get("before"), r.URL.Query().Get("after")))
	}
	_, _ = w.Write(rr.Body.Bytes())
}

func handleRepos(w http.ResponseWriter, r *http.Request, root string) {
	setNoStore(w)
	if r.URL.Path != "/" && r.URL.Path != "/repos" {
		http.NotFound(w, r)
		return
	}

	repos, err := DiscoverRepos(root)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	renderTemplate(w, "repos.html", map[string]any{"Repos": repos})
}

type sessionsData struct {
	EncodedRepo string
	RepoName    string
	Sessions    []SessionSummary
}

func handleSessions(w http.ResponseWriter, r *http.Request, root, repo string) {
	setNoStore(w)
	summaries, err := ListSessions(root, repo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Derive a display name from the first session's CWD
	name := repo
	for _, s := range summaries {
		if s.CWD != "" {
			name = filepath.Base(s.CWD)
			break
		}
	}

	renderTemplate(w, "sessions.html", sessionsData{
		EncodedRepo: repo,
		RepoName:    name,
		Sessions:    summaries,
	})
}

type sessionPageData struct {
	EncodedRepo string
	RepoName    string
	Session     *ViewSession

	// Static marks a render destined for a standalone file rather than the
	// live server. It inlines the two /static/ assets below and turns the
	// site-absolute breadcrumb links, which are dead over file://, into plain
	// text. The zero value is what the HTTP handler renders, so the served
	// page is byte-identical to what it was before export existed.
	Static    bool
	InlineCSS template.CSS
	InlineJS  template.JS
	Language  string
}

func handleSession(w http.ResponseWriter, r *http.Request, root, repo, sessionID string) {
	vs, err := LoadSession(root, repo, sessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load session: %v", err), http.StatusNotFound)
		return
	}

	// Derive a display name
	name := filepath.Base(vs.Summary.CWD)
	if name == "." || name == "" {
		name = repo
	}

	renderTemplate(w, "session.html", sessionPageData{
		EncodedRepo: repo,
		RepoName:    name,
		Session:     vs,
	})
}

type compareBucket struct {
	Title       string
	Findings    []model.LlmComment
	Description string
}

type comparePageData struct {
	EncodedRepo string
	RepoName    string
	Before      SessionSummary
	After       SessionSummary
	// Warning explains mode, chronology, or coverage limitations.
	Warning string
	// Buckets holds session.Compare's four buckets in the order the CLI
	// prints them. Unlike the CLI, an empty bucket still renders (as "none"):
	// a section vanishing from a web page is indistinguishable from a broken
	// page, while "Resolved (0)" is itself the answer the reader came for.
	Buckets     []compareBucket
	Recommended int
	Static      bool
	InlineCSS   template.CSS
	InlineJS    template.JS
}

// toLlmComments adapts the viewer's parsed findings to the model type
// session.Compare consumes. Path, Category and ExistingCode are load-bearing:
// they are the three inputs to Compare's finding key, so dropping any of them
// would collapse findings onto the Content fallback and mis-bucket the diff.
func toLlmComments(comments []*ReviewComment) []model.LlmComment {
	out := make([]model.LlmComment, 0, len(comments))
	for _, c := range comments {
		if c == nil {
			continue
		}
		out = append(out, model.LlmComment{
			Path:                c.FilePath,
			Content:             c.Content,
			SuggestionCode:      c.SuggestionCode,
			PendingConfirmation: c.PendingConfirmation,
			ExistingCode:        c.ExistingCode,
			StartLine:           c.StartLine,
			EndLine:             c.EndLine,
			Category:            c.Category,
			Severity:            c.Severity,
			CodeAttribution:     c.CodeAttribution,
		})
	}
	return out
}

// modeWarning mirrors the only comparability warning `ocr session compare`
// emits (cmd/opencodereview/session_cmd.go, runSessionCompare): a differing
// review mode warns, and the comparison still renders. The dash for an unset
// mode matches displayMode there and the Mode column in sessions.html.
func modeWarning(before, after SessionSummary) string {
	if before.ReviewMode == after.ReviewMode {
		return ""
	}
	dash := func(mode string) string {
		if mode == "" {
			return "-"
		}
		return mode
	}
	return modeDifferenceWarning(dash(before.ReviewMode), dash(after.ReviewMode))
}

// handleCompare renders the `ocr session compare` result for two sessions of
// the same repo. repo is the on-disk directory name, passed through from the
// URL exactly as handleSessions/handleSession pass it.
func handleCompare(w http.ResponseWriter, r *http.Request, root, repo string) {
	handleCompareFormat(w, r, root, repo, false, false)
}

func handleCompareFormat(w http.ResponseWriter, r *http.Request, root, repo string, standalone, markdown bool) {
	before := r.URL.Query().Get("before")
	after := r.URL.Query().Get("after")
	if before == "" || after == "" {
		http.Error(w, "query parameters 'before' and 'after' are required", http.StatusBadRequest)
		return
	}
	// ServeMux never inspects query values, so these ids need the same
	// rejection newMux gives the path segments: both end up in filepath.Join
	// inside LoadSession.
	for _, id := range []string{before, after} {
		if unsafeSegment(id) {
			http.Error(w, "invalid session id", http.StatusBadRequest)
			return
		}
	}

	bv, err := LoadSession(root, repo, before)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load session: %v", err), http.StatusNotFound)
		return
	}
	av, err := LoadSession(root, repo, after)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load session: %v", err), http.StatusNotFound)
		return
	}

	// encodeRepoPath maps both separators to "-", so two distinct working
	// directories ("/home/a/b" and "/home/a-b") share one viewer directory and
	// this route can be handed a cross-repo pair. Comparing findings across
	// repositories is meaningless, so it is an error here exactly as it is in
	// the CLI (cmd/opencodereview/session_cmd.go, runSessionCompare) - unlike a
	// differing review mode, which only warns.
	if bv.Summary.CWD != av.Summary.CWD {
		http.Error(w, "sessions belong to different repositories", http.StatusBadRequest)
		return
	}

	// Same call shape as the CLI: the after manifest supplies reviewed paths and
	// old-to-new mappings for renamed files.
	result := session.Compare(
		toLlmComments(bv.Comments),
		toLlmComments(av.Comments),
		av.Summary.RunManifest,
	)

	name := filepath.Base(av.Summary.CWD)
	if name == "." || name == "" {
		name = repo
	}

	warning := modeWarning(bv.Summary, av.Summary)
	if av.Summary.RunManifest == nil {
		warning += legacyCoverageWarning()
	}
	if av.Summary.Aborted || av.Summary.FailedCount > 0 || av.Summary.TerminalState == "partial" || av.Summary.TerminalState == "failed" {
		warning += incompleteReviewWarning()
	}
	if bv.Summary.Timestamp.After(av.Summary.Timestamp) {
		warning += reversedChronologyWarning()
	}
	recommended := 0
	for _, c := range result.Persisting {
		if fixPriority(c) != "Review and schedule" {
			recommended++
		}
	}
	data := comparePageData{
		EncodedRepo: repo,
		RepoName:    name,
		Before:      bv.Summary,
		After:       av.Summary,
		Warning:     warning,
		Recommended: recommended,
		Buckets: []compareBucket{
			{Title: "New", Findings: result.New, Description: "Newly detected findings in the after review."},
			{Title: "Persisting", Findings: result.Persisting, Description: "Not fixed: findings detected in both reviews. Priorities are recommendations based on severity and category."},
			{Title: "Resolved", Findings: result.Resolved, Description: "Apparently fixed: no longer detected in reviewed files. This is a comparison result, not proof of correctness."},
			{Title: "Not reviewed", Findings: result.NotReviewed, Description: "Fix status unknown: these files were not successfully re-reviewed."},
		},
	}
	if markdown {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(compareMarkdown(data)))
		return
	}
	if standalone {
		css, err := assets.ReadFile("static/style.css")
		if err != nil {
			http.Error(w, "failed to load stylesheet", http.StatusInternalServerError)
			return
		}
		actions, err := assets.ReadFile("static/actions.js")
		if err != nil {
			http.Error(w, "failed to load comparison script", http.StatusInternalServerError)
			return
		}
		compareJS, err := assets.ReadFile("static/compare.js")
		if err != nil {
			http.Error(w, "failed to load comparison script", http.StatusInternalServerError)
			return
		}
		data.Static, data.InlineCSS, data.InlineJS = true, template.CSS(css), template.JS(string(actions)+"\n"+string(compareJS))
	}
	renderTemplate(w, "compare.html", data)
}

func compareMarkdown(data comparePageData) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 会话对比\n\n- 仓库：%s\n- 审核前：%s（%s）\n- 审核后：%s（%s）\n", data.RepoName, data.Before.SessionID, formatTime(data.Before.Timestamp), data.After.SessionID, formatTime(data.After.Timestamp))
	if data.Warning != "" {
		fmt.Fprintf(&b, "\n> 注意：%s\n", data.Warning)
	}
	fmt.Fprintf(&b, "\n## 结果概览\n\n| 状态 | 数量 |\n| --- | ---: |\n")
	for _, bucket := range data.Buckets {
		fmt.Fprintf(&b, "| %s | %d |\n", bucket.Title, len(bucket.Findings))
	}
	for _, bucket := range data.Buckets {
		fmt.Fprintf(&b, "\n## %s（%d）\n", bucket.Title, len(bucket.Findings))
		if len(bucket.Findings) == 0 {
			b.WriteString("\n当前分类没有问题。\n")
			continue
		}
		for _, finding := range bucket.Findings {
			fmt.Fprintf(&b, "\n### %s:%d-%d\n\n- 类别：%s\n- 严重程度：%s\n", finding.Path, finding.StartLine, finding.EndLine, finding.Category, finding.Severity)
			if bucket.Title == "New" || bucket.Title == "Persisting" {
				fmt.Fprintf(&b, "- 处理建议：%s\n", fixPriority(finding))
			}
			fmt.Fprintf(&b, "\n%s\n", finding.Content)
			if finding.ExistingCode != "" {
				fmt.Fprintf(&b, "\n现有代码：\n\n```\n%s\n```\n", finding.ExistingCode)
			}
			if finding.SuggestionCode != "" {
				fmt.Fprintf(&b, "\n建议修改：\n\n```\n%s\n```\n", finding.SuggestionCode)
			}
		}
	}
	return b.String()
}

// fixPriority is deliberately deterministic and explainable; it does not claim
// another AI review or infer correctness from the availability of a code patch.
func fixPriority(c model.LlmComment) string {
	severity := normalizedCommentSeverity(c.Severity)
	category := normalizedCommentCategory(c.Category)
	if severity == "critical" || severity == "high" || category == "security" {
		return "Fix first"
	}
	if severity == "medium" || category == "bug" || category == "performance" {
		return "Recommended fix"
	}
	return "Review and schedule"
}
