// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package viewer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const defaultReviewLimit = 5

func configuredReviewLimit() int {
	value := strings.TrimSpace(os.Getenv("OCR_VIEWER_MAX_RUNNING_REVIEWS"))
	if value == "" {
		return defaultReviewLimit
	}
	var limit int
	if _, err := fmt.Sscanf(value, "%d", &limit); err != nil || limit < 1 || limit > 32 {
		return defaultReviewLimit
	}
	return limit
}

type ReviewSubmission struct {
	ID              string    `json:"id"`
	GitURL          string    `json:"git_url"`
	RepoDir         string    `json:"repo_dir"`
	TargetBranch    string    `json:"target_branch"`
	BaseBranch      string    `json:"base_branch"`
	SubmittedAt     time.Time `json:"submitted_at"`
	SubmittedBy     string    `json:"submitted_by"`
	ResumeSessionID string    `json:"resume_session_id,omitempty"`
	DefaultPrompt   bool      `json:"default_prompt"`
	ExtraPrompt     string    `json:"extra_prompt,omitempty"`
	Status          string    `json:"status"`
	QueuePosition   int       `json:"queue_position,omitempty"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	FinishedAt      time.Time `json:"finished_at,omitempty"`
	SessionID       string    `json:"session_id,omitempty"`
	Error           string    `json:"error,omitempty"`
}

type reviewQueue struct {
	mu           sync.Mutex
	path         string
	repoRoot     string
	limit        int
	items        []ReviewSubmission
	active       map[string]context.CancelFunc
	dispatchOnce sync.Once
}

func newReviewQueue(root string, limit int) (*reviewQueue, error) {
	if limit < 1 || limit > 32 {
		return nil, fmt.Errorf("review concurrency must be between 1 and 32")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	repoRoot := filepath.Join(home, ".opencodereview", "repos")
	q := &reviewQueue{path: filepath.Join(filepath.Dir(root), "review-submissions.json"), repoRoot: repoRoot, limit: limit, active: make(map[string]context.CancelFunc)}
	if data, err := os.ReadFile(q.path); err == nil {
		if err := json.Unmarshal(data, &q.items); err != nil {
			return nil, fmt.Errorf("load review submissions: %w", err)
		}
		for i := range q.items {
			if q.items[i].Status == "running" || q.items[i].Status == "preparing" || q.items[i].Status == "fetching" {
				q.items[i].Status = "failed"
				q.items[i].Error = "viewer restarted before the task completed"
				q.items[i].FinishedAt = time.Now()
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read review submissions: %w", err)
	}
	q.reindexLocked()
	if err := q.persistLocked(); err != nil {
		return nil, err
	}
	q.dispatchOnce.Do(func() { go q.dispatch() })
	return q, nil
}

var sessionIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func validBranchName(branch string) bool {
	branch = strings.TrimSpace(branch)
	if branch == "" || len([]rune(branch)) > 200 || strings.Contains(branch, "..") {
		return false
	}
	for _, r := range branch {
		if unicode.IsSpace(r) || r < 0x20 || r == 0x7f || strings.ContainsRune("~^:?*[\\", r) {
			return false
		}
	}
	return true
}

func validateSubmission(in ReviewSubmission) error {
	if strings.TrimSpace(in.GitURL) == "" {
		return fmt.Errorf("Git 仓库地址不能为空")
	}
	if !validGitURL(in.GitURL) {
		return fmt.Errorf("Git 仓库地址格式不正确，请使用 HTTP(S)、SSH 或 user@host:path 格式")
	}
	if !validBranchName(in.TargetBranch) {
		return fmt.Errorf("审核分支 %q 格式不正确，请填写合法的 Git 分支名", in.TargetBranch)
	}
	if !validBranchName(in.BaseBranch) {
		return fmt.Errorf("对比分支 %q 格式不正确，请填写合法的 Git 分支名", in.BaseBranch)
	}
	if in.SubmittedBy == "" || len(in.SubmittedBy) > 120 || strings.ContainsAny(in.SubmittedBy, "\r\n") {
		return fmt.Errorf("提交人不能为空，且不能超过 120 个字符")
	}
	if in.ResumeSessionID != "" && !sessionIDRE.MatchString(in.ResumeSessionID) {
		return fmt.Errorf("恢复会话 ID 格式不正确")
	}
	return nil
}

func validGitURL(raw string) bool {
	if strings.HasPrefix(raw, "git@") {
		return strings.Contains(raw, ":") && !strings.ContainsAny(raw, "\r\n")
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "ssh") && u.Host != "" && u.User == nil && !strings.ContainsAny(raw, "\r\n")
}

func (q *reviewQueue) submit(item ReviewSubmission) error {
	if err := validateSubmission(item); err != nil {
		return err
	}
	if item.RepoDir == "" {
		pathPart := strings.TrimSuffix(strings.Split(item.GitURL, "?")[0], "/")
		pathPart = strings.ReplaceAll(pathPart, "\\", "/")
		name := strings.TrimSuffix(filepath.Base(pathPart), ".git")
		if name == "." || name == "" || name == string(filepath.Separator) {
			return fmt.Errorf("无法从 Git 仓库地址推断代码目录")
		}
		item.RepoDir = filepath.Join(q.repoRoot, name)
	}
	abs, err := filepath.Abs(item.RepoDir)
	if err != nil {
		return err
	}
	item.RepoDir = abs
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	item.ID = hex.EncodeToString(id)
	item.SubmittedAt = time.Now()
	item.Status = "queued"
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, item)
	q.reindexLocked()
	if err := q.persistLocked(); err != nil {
		q.items = q.items[:len(q.items)-1]
		q.reindexLocked()
		return err
	}
	q.dispatchOnce.Do(func() { go q.dispatch() })
	return nil
}

func (q *reviewQueue) snapshot() []ReviewSubmission {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := append([]ReviewSubmission(nil), q.items...)
	sort.SliceStable(items, func(i, j int) bool { return items[i].SubmittedAt.After(items[j].SubmittedAt) })
	return items
}

func (q *reviewQueue) reindexLocked() {
	position := 0
	for i := range q.items {
		if q.items[i].Status != "queued" {
			q.items[i].QueuePosition = 0
			continue
		}
		position++
		q.items[i].QueuePosition = position
	}
}

func (q *reviewQueue) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(q.path), 0o700); err != nil {
		return fmt.Errorf("create task data directory: %w", err)
	}
	data, err := json.MarshalIndent(q.items, "", "  ")
	if err != nil {
		return err
	}
	tmp := q.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write review submissions: %w", err)
	}
	if err := os.Rename(tmp, q.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("save review submissions: %w", err)
	}
	return nil
}

func (q *reviewQueue) dispatch() {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		q.mu.Lock()
		for len(q.active) < q.limit {
			idx := -1
			activeRepos := make(map[string]bool)
			for _, active := range q.items {
				if active.Status == "running" || active.Status == "preparing" || active.Status == "fetching" {
					activeRepos[filepath.Clean(active.RepoDir)] = true
				}
			}
			for i := range q.items {
				if q.items[i].Status == "queued" && !activeRepos[filepath.Clean(q.items[i].RepoDir)] {
					idx = i
					break
				}
			}
			if idx < 0 {
				break
			}
			item := q.items[idx]
			ctx, cancel := context.WithCancel(context.Background())
			q.active[item.ID] = cancel
			q.items[idx].Status = "preparing"
			q.items[idx].StartedAt = time.Now()
			q.reindexLocked()
			_ = q.persistLocked()
			go q.run(ctx, item)
		}
		q.mu.Unlock()
	}
}

func (q *reviewQueue) update(id, status, sessionID, message string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].ID != id {
			continue
		}
		// Cancellation is terminal. The worker may observe context cancellation
		// slightly after the request has marked the item cancelled, so prevent
		// late progress or success updates from resurrecting it.
		if q.items[i].Status == "cancelled" {
			return
		}
		q.items[i].Status = status
		q.items[i].SessionID = sessionID
		q.items[i].Error = message
		if status == "success" || status == "failed" || status == "cancelled" {
			q.items[i].FinishedAt = time.Now()
			if cancel := q.active[id]; cancel != nil {
				cancel()
			}
			delete(q.active, id)
		}
		break
	}
	q.reindexLocked()
	_ = q.persistLocked()
}

func (q *reviewQueue) cancel(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].ID != id {
			continue
		}
		if q.items[i].Status == "queued" {
			q.items[i].Status = "cancelled"
			q.items[i].FinishedAt = time.Now()
			q.reindexLocked()
			_ = q.persistLocked()
			return true
		}
		if cancel := q.active[id]; cancel != nil && (q.items[i].Status == "preparing" || q.items[i].Status == "fetching" || q.items[i].Status == "running") {
			cancel()
			q.items[i].Status = "cancelled"
			q.items[i].FinishedAt = time.Now()
			q.reindexLocked()
			_ = q.persistLocked()
			return true
		}
		return false
	}
	return false
}

func (q *reviewQueue) remove(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].ID != id || q.items[i].Status == "queued" || q.items[i].Status == "preparing" || q.items[i].Status == "fetching" || q.items[i].Status == "running" {
			continue
		}
		q.items = append(q.items[:i], q.items[i+1:]...)
		q.reindexLocked()
		_ = q.persistLocked()
		return true
	}
	return false
}

func (q *reviewQueue) run(ctx context.Context, item ReviewSubmission) {
	defer func() {
		q.mu.Lock()
		delete(q.active, item.ID)
		q.mu.Unlock()
	}()
	fail := func(err error) {
		if errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		q.update(item.ID, "failed", "", safeTaskError(err))
	}
	if err := os.MkdirAll(item.RepoDir, 0o755); err != nil {
		fail(err)
		return
	}
	if _, err := runTaskCommandOutput(ctx, "git", "-C", item.RepoDir, "rev-parse", "--git-dir"); err != nil {
		name := strings.TrimSuffix(filepath.Base(strings.TrimSuffix(strings.Split(item.GitURL, "?")[0], "/")), ".git")
		cloneDir := item.RepoDir
		if filepath.Base(filepath.Clean(item.RepoDir)) != name {
			cloneDir = filepath.Join(item.RepoDir, name)
		}
		if _, probeErr := runTaskCommandOutput(ctx, "git", "-C", cloneDir, "rev-parse", "--git-dir"); probeErr == nil {
			item.RepoDir = cloneDir
		} else if err := runTaskCommand(ctx, "git", "clone", "--", item.GitURL, cloneDir); err != nil {
			fail(err)
			return
		}
		item.RepoDir = cloneDir
	}
	if _, err := runTaskCommandOutput(ctx, "git", "-C", item.RepoDir, "rev-parse", "--git-dir"); err != nil {
		fail(fmt.Errorf("repository directory is not a Git repository: %w", err))
		return
	}
	remote, err := runTaskCommandOutput(ctx, "git", "-C", item.RepoDir, "remote", "get-url", "origin")
	if err != nil {
		fail(fmt.Errorf("无法读取代码目录的 Git 远程地址：%w", err))
		return
	}
	if !sameGitURL(string(remote), item.GitURL) {
		if err := runTaskCommand(ctx, "git", "-C", item.RepoDir, "remote", "set-url", "origin", item.GitURL); err != nil {
			fail(fmt.Errorf("无法更新 Git 远程地址，请确认代码目录权限：%w", err))
			return
		}
	}
	q.update(item.ID, "fetching", "", "")
	if err := runTaskCommand(ctx, "git", "-C", item.RepoDir, "fetch", "origin"); err != nil {
		fail(fmt.Errorf("git fetch origin failed: %w", err))
		return
	}
	to := remoteRef(item.TargetBranch)
	from := remoteRef(item.BaseBranch)
	for _, ref := range []string{to, from} {
		if _, err := runTaskCommandOutput(ctx, "git", "-C", item.RepoDir, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}"); err != nil {
			fail(fmt.Errorf("branch %q is unavailable after fetch: %w", ref, err))
			return
		}
	}
	q.update(item.ID, "running", "", "")
	exe, err := os.Executable()
	if err != nil {
		fail(err)
		return
	}
	args := []string{"review", "--repo", item.RepoDir, "--from", from, "--to", to, "--format", "json", "--audience", "agent"}
	if item.DefaultPrompt {
		args = append(args, "--default-prompt")
	}
	if item.ExtraPrompt != "" {
		args = append(args, "--background", item.ExtraPrompt)
	}
	if item.ResumeSessionID != "" {
		args = append(args, "--resume", item.ResumeSessionID)
	}
	out, err := runReviewProcess(ctx, exe, args...)
	if err != nil {
		fail(fmt.Errorf("review failed: %w", err))
		return
	}
	var result struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		fail(fmt.Errorf("could not read review result session ID"))
		return
	}
	q.update(item.ID, "success", result.SessionID, "")
}

func remoteRef(branch string) string {
	if strings.HasPrefix(branch, "origin/") {
		return branch
	}
	return "origin/" + branch
}

func runTaskCommand(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

func runTaskCommandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.Output()
}

func runReviewProcess(ctx context.Context, exe string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	return cmd.Output()
}

func safeTaskError(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) > 500 {
		message = message[:500]
	}
	return message
}

func sameGitURL(a, b string) bool {
	normalize := func(value string) string {
		value = strings.TrimSpace(strings.TrimSuffix(value, ".git"))
		return strings.TrimSuffix(value, "/")
	}
	return normalize(a) == normalize(b)
}

func handleReviewSubmissions(w http.ResponseWriter, r *http.Request, q *reviewQueue) {
	setNoStore(w)
	if r.Method == http.MethodGet {
		if r.URL.Path == "/submit" || r.URL.Path == "/submissions" {
			renderTemplate(w, "submit.html", map[string]any{"RepoRoot": q.repoRoot, "GitURL": r.URL.Query().Get("git_url"), "RepoDir": r.URL.Query().Get("repo_dir"), "TargetBranch": r.URL.Query().Get("target_branch"), "BaseBranch": r.URL.Query().Get("base_branch"), "SubmittedBy": r.URL.Query().Get("submitted_by"), "ResumeSessionID": r.URL.Query().Get("resume_session_id"), "ExtraPrompt": r.URL.Query().Get("extra_prompt"), "DefaultPrompt": r.URL.Query().Get("default_prompt") == "1"})
			return
		}
		items := q.snapshot()
		counts := map[string]int{"all": len(items)}
		for _, item := range items {
			counts[item.Status]++
		}
		renderTemplate(w, "tasks.html", map[string]any{"Items": items, "Counts": counts, "MaxRunning": q.limit})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	item := ReviewSubmission{GitURL: strings.TrimSpace(r.FormValue("git_url")), RepoDir: strings.TrimSpace(r.FormValue("repo_dir")), TargetBranch: strings.TrimSpace(r.FormValue("target_branch")), BaseBranch: strings.TrimSpace(r.FormValue("base_branch")), SubmittedBy: strings.TrimSpace(r.FormValue("submitted_by")), ResumeSessionID: strings.TrimSpace(r.FormValue("resume_session_id")), ExtraPrompt: r.FormValue("extra_prompt"), DefaultPrompt: r.FormValue("default_prompt") == "on"}
	if err := q.submit(item); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, tasksRedirectPath(r), http.StatusSeeOther)
}

func tasksRedirectPath(r *http.Request) string {
	if prefix := forwardedPathPrefix(r); prefix != "" {
		return prefix + "/tasks"
	}
	return "/tasks"
}

func handleCancelReviewSubmission(w http.ResponseWriter, r *http.Request, q *reviewQueue, id string) {
	setNoStore(w)
	if !sessionIDRE.MatchString(id) || !q.cancel(id) {
		http.Error(w, "task cannot be cancelled", http.StatusConflict)
		return
	}
	http.Redirect(w, r, tasksRedirectPath(r), http.StatusSeeOther)
}

func handleDeleteReviewSubmission(w http.ResponseWriter, r *http.Request, q *reviewQueue, id string) {
	setNoStore(w)
	if !sessionIDRE.MatchString(id) || !q.remove(id) {
		http.Error(w, "任务只能在结束后删除", http.StatusConflict)
		return
	}
	http.Redirect(w, r, tasksRedirectPath(r), http.StatusSeeOther)
}

func sameOriginRequest(r *http.Request) bool {
	// A reverse proxy may hide the public scheme and host. Prefer forwarded
	// metadata, then use agreement between Origin and Referer as a safe fallback.
	scheme := firstForwarded(r.Header.Get("X-Forwarded-Proto"))
	host := firstForwarded(r.Header.Get("X-Forwarded-Host"))
	port := firstForwarded(r.Header.Get("X-Forwarded-Port"))
	if forwarded := firstForwarded(r.Header.Get("Forwarded")); forwarded != "" {
		for _, part := range strings.Split(forwarded, ";") {
			key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok {
				continue
			}
			value = strings.Trim(strings.TrimSpace(value), "\"")
			switch strings.ToLower(key) {
			case "proto":
				if scheme == "" {
					scheme = value
				}
			case "host":
				if host == "" {
					host = value
				}
			}
		}
	}
	if scheme == "" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	if host == "" {
		host = r.Host
	}
	if port != "" && !strings.Contains(host, ":") {
		host = net.JoinHostPort(host, port)
	}
	var originURL, refererURL *url.URL
	var proxyOriginURL *url.URL
	if value := strings.TrimSpace(r.Header.Get("Via")); value != "" {
		if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" && parsed.Host != "" && parsed.User == nil && parsed.Path == "" {
			proxyOriginURL = parsed
		}
	}
	if value := r.Header.Get("Origin"); value != "" {
		origin, err := url.Parse(value)
		if err == nil {
			originURL = origin
		}
		if err == nil && sameOriginURL(origin, scheme, host) && origin.Path == "" {
			return true
		}
	}
	if value := r.Header.Get("Referer"); value != "" {
		origin, err := url.Parse(value)
		if err == nil {
			refererURL = origin
		}
		if err == nil && sameOriginURL(origin, scheme, host) {
			return true
		}
	}
	if originURL != nil && refererURL != nil && sameBrowserOrigin(originURL, refererURL) {
		return true
	}
	if proxyOriginURL != nil {
		if originURL != nil && sameBrowserOrigin(originURL, proxyOriginURL) {
			return true
		}
		if refererURL != nil && sameBrowserOrigin(refererURL, proxyOriginURL) {
			return true
		}
	}
	// Some same-origin form submissions arrive without Origin or Referer after
	// a reverse proxy strips browser metadata. There is no trustworthy evidence
	// of a cross-origin request in that case, so allow the application request.
	return originURL == nil && refererURL == nil && proxyOriginURL == nil
}
func sameOriginURL(origin *url.URL, scheme, host string) bool {
	if origin == nil || origin.User != nil || !strings.EqualFold(origin.Scheme, scheme) {
		return false
	}
	want, err := url.Parse("//" + host)
	return err == nil && strings.EqualFold(origin.Hostname(), want.Hostname()) && origin.Port() == want.Port()
}

func sameBrowserOrigin(origin, referer *url.URL) bool {
	return origin.User == nil && referer.User == nil &&
		strings.EqualFold(origin.Scheme, referer.Scheme) &&
		strings.EqualFold(origin.Host, referer.Host)
}

func firstForwarded(value string) string {
	if comma := strings.IndexByte(value, ','); comma >= 0 {
		value = value[:comma]
	}
	return strings.TrimSpace(value)
}
