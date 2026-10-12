// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package viewer provides a WebUI for browsing session records
// produced by open-code-review runs. It scans JSONL files under
// $HOME/.opencodereview/sessions/, parses them, and exposes structured data.
package viewer

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/open-code-review/internal/model"
	"github.com/alibaba/open-code-review/internal/session"
)

// SessionsRoot returns the root directory where session JSONL files are stored.
func SessionsRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".opencodereview", "sessions"), nil
}

// RepoInfo represents a discovered repository from the sessions directory.
type RepoInfo struct {
	EncodedPath  string // encoded directory name on disk
	SessionCount int
	LastModified time.Time
}

// DiscoverRepos walks the sessions root and returns one entry per subdirectory.
func DiscoverRepos(root string) ([]RepoInfo, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read sessions dir: %w", err)
	}

	var repos []RepoInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		repoDir := filepath.Join(root, e.Name())
		info := RepoInfo{EncodedPath: e.Name()}

		subEntries, err := os.ReadDir(repoDir)
		if err != nil {
			continue
		}
		for _, se := range subEntries {
			if strings.HasSuffix(se.Name(), ".jsonl") {
				info.SessionCount++
				if fi, err := se.Info(); err == nil {
					if fi.ModTime().After(info.LastModified) {
						info.LastModified = fi.ModTime()
					}
				}
			}
		}
		if info.SessionCount > 0 {
			repos = append(repos, info)
		}
	}

	sort.Slice(repos, func(i, j int) bool {
		return repos[i].LastModified.After(repos[j].LastModified)
	})
	return repos, nil
}

// SessionSummary is built from session metadata, live progress/checkpoint
// records, and the optional terminal session_end record.
type SessionSummary struct {
	SessionID     string
	Timestamp     time.Time
	CWD           string
	GitBranch     string
	Model         string
	ReviewMode    string
	DiffFrom      string
	DiffTo        string
	DiffCommit    string
	FilesReviewed []string
	DurationSec   float64
	FileCount     int
	// FilesReadCount is the number of file-level review records observed while
	// loading the session. It is kept separate from findings because a clean
	// file still contributes to review progress.
	FilesReadCount   int
	ActiveCount      int
	LLMFailures      int
	CommentCount     int
	Aborted          bool
	Legacy           bool
	TerminalState    string
	SelectedCount    int
	CompletedCount   int
	ReusedCount      int
	FailedCount      int
	WaivedCount      int
	RunManifest      *session.RunManifest
	hasProgressTotal bool
	SelectedPaths    []string
	selectedPathSet  map[string]struct{}
}

func (s SessionSummary) ProgressPercent() int {
	// A run manifest is authoritative for review sessions. Every selected item
	// ends in exactly one coverage bucket, including failed and waived items, so
	// those buckets describe progress without depending on which checkpoint
	// records happened to be persisted.
	if s.RunManifest != nil {
		total := s.SelectedCount
		if total == 0 {
			total = s.CompletedCount + s.ReusedCount + s.FailedCount + s.WaivedCount
		}
		if total == 0 {
			return 0
		}
		done := s.CompletedCount + s.ReusedCount + s.FailedCount + s.WaivedCount
		if done > total {
			done = total
		}
		return done * 100 / total
	}
	if s.hasProgressTotal && s.SelectedCount > 0 && s.Aborted {
		done := s.CompletedCount + s.ReusedCount + s.FailedCount + s.WaivedCount
		active := min(s.ActiveCount, max(s.SelectedCount-done, 0))
		if done >= s.SelectedCount {
			return 100
		}
		// An in-flight file has made meaningful progress, but is not yet a
		// completed review item. Count it as half a unit until its checkpoint lands.
		numerator := done*2 + active
		return min(numerator*100/(s.SelectedCount*2), 99)
	}
	if s.hasProgressTotal {
		if s.SelectedCount == 0 {
			return 0
		}
		return min(s.FilesReadCount, s.SelectedCount) * 100 / s.SelectedCount
	}

	// Legacy sessions have no frozen denominator. Once they have ended,
	// files_reviewed is the best available fallback; checkpoint records refine
	// it for sessions that stopped before all files were reviewed. Active legacy
	// sessions can use dispatched request groups as a best-effort estimate.
	total := s.FileCount
	if total == 0 {
		total = s.SelectedCount
	}
	if total == 0 {
		if s.Aborted {
			// An active session without a persisted denominator cannot yield a
			// meaningful percentage. Item records alone must not turn into 100%.
			return 0
		}
		total = s.CompletedCount + s.ReusedCount + s.FailedCount + s.WaivedCount
	}
	if total == 0 {
		return 0
	}
	done := s.FilesReadCount
	if done == 0 {
		done = len(s.FilesReviewed)
	}
	if done == 0 {
		done = s.CompletedCount + s.ReusedCount + s.FailedCount + s.WaivedCount
	}
	if done > total {
		done = total
	}
	if s.Aborted && s.ActiveCount > 0 {
		active := min(s.ActiveCount, max(total-done, 0))
		if done >= total {
			return 100
		}
		return min((done*2+active)*100/(total*2), 99)
	}
	return done * 100 / total
}

// ListSessions returns lightweight summaries for all sessions in a repo subdir.
func ListSessions(root, encodedRepo string) ([]SessionSummary, error) {
	repoDir := filepath.Join(root, encodedRepo)
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return nil, fmt.Errorf("read repo dir: %w", err)
	}

	type sessionResult struct {
		index   int
		summary SessionSummary
		err     error
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		paths = append(paths, filepath.Join(repoDir, e.Name()))
	}
	// A session file can contain the full review trace and be very large. Read
	// independent sessions concurrently so one large history does not block the
	// whole repository page behind a serial disk scan.
	results := make(chan sessionResult, len(paths))
	workers := min(len(paths), 8)
	var wg sync.WaitGroup
	jobs := make(chan int)
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				summary, parseErr := peekSession(paths[index])
				results <- sessionResult{index: index, summary: summary, err: parseErr}
			}
		}()
	}
	go func() {
		for index := range paths {
			jobs <- index
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	parsed := make([]sessionResult, 0, len(paths))
	for result := range results {
		if result.err == nil {
			result.summary.SessionID = strings.TrimSuffix(filepath.Base(paths[result.index]), ".jsonl")
			parsed = append(parsed, result)
		}
	}
	summaries := make([]SessionSummary, 0, len(parsed))
	for _, result := range parsed {
		summaries = append(summaries, result.summary)
	}

	// Timestamp alone is not a total order: two sessions can share one
	// (same second, or both zero for an aborted run before its first
	// timestamped record). sessions.html pairs row i with row i+1 as
	// "compare", so an order that reshuffles on ties makes that pairing
	// non-deterministic across renders. SessionID breaks the tie so the
	// order is deterministic across calls, not just stable within one sort.
	sort.Slice(summaries, func(i, j int) bool {
		if !summaries[i].Timestamp.Equal(summaries[j].Timestamp) {
			return summaries[i].Timestamp.After(summaries[j].Timestamp)
		}
		return summaries[i].SessionID > summaries[j].SessionID
	})
	return summaries, nil
}

// peekSession reads the first record, all review_item records (for comment
// count), and the last record of a JSONL file.
func peekSession(path string) (SessionSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return SessionSummary{}, err
	}
	defer f.Close()

	summary := SessionSummary{Aborted: true}
	var lastLine []byte
	readFiles := make(map[string]struct{})
	requestedPaths := make(map[string]struct{})
	activeItems := make(map[string]struct{})
	startedItemsSeen := false
	metadataRead := false
	readErr := readJSONLLines(f, func(line []byte) {
		lastLine = append([]byte(nil), line...)

		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			return
		}
		typ, _ := rec["type"].(string)
		if !metadataRead && typ == "session_start" {
			if ts, ok := rec["timestamp"].(string); ok {
				summary.Timestamp, _ = time.Parse(time.RFC3339Nano, ts)
			}
			if cwd, ok := rec["cwd"].(string); ok {
				summary.CWD = cwd
			}
			if branch, ok := rec["gitBranch"].(string); ok {
				summary.GitBranch = branch
			}
			if model, ok := rec["model"].(string); ok {
				summary.Model = model
			}
			if rm, ok := rec["reviewMode"].(string); ok {
				summary.ReviewMode = rm
			}
			if v, ok := rec["diffFrom"].(string); ok {
				summary.DiffFrom = v
			}
			if v, ok := rec["diffTo"].(string); ok {
				summary.DiffTo = v
			}
			if v, ok := rec["diffCommit"].(string); ok {
				summary.DiffCommit = v
			}
			metadataRead = true
		}
		if typ == "review_progress" {
			applyReviewProgress(&summary, rec)
		}
		if typ == "review_item_started" {
			startedItemsSeen = true
			applyStartedItems(&summary, activeItems, rec)
		}
		if typ == "llm_request" {
			path, _ := rec["filePath"].(string)
			taskType, _ := rec["taskType"].(string)
			if path != "__grouping__" && path != "" {
				addActiveRequest(requestedPaths, path, taskType)
			}
		}

		// Count only actual terminal item records. Searching the raw JSON for a
		// type name would mistake an LLM response containing that text for a
		// completed file and inflate the percentage.
		if isReviewItemRecord(typ) {
			finishActiveItem(&summary, activeItems, rec)
			if addReviewedFile(readFiles, rec) {
				countReviewItem(&summary, typ)
			}
			if comments, ok := rec["comments"].([]any); ok {
				summary.CommentCount += len(comments)
			}
		}
	})

	if len(lastLine) > 0 {
		var rec map[string]any
		if err := json.Unmarshal(lastLine, &rec); err == nil {
			if typ, _ := rec["type"].(string); typ == "session_end" {
				applySessionEnd(&summary, rec)
			}
		}
	}
	if startedItemsSeen {
		summary.ActiveCount = len(activeItems)
	} else {
		summary.ActiveCount = activeRequestedItemCount(requestedPaths, readFiles)
	}
	// A running session has no stable denominator. Only repair a completed
	// legacy session whose end record omitted (or under-counted) files_reviewed.
	if !summary.Aborted && summary.RunManifest == nil && !summary.hasProgressTotal && summary.FileCount < summary.FilesReadCount {
		summary.FileCount = summary.FilesReadCount
	}
	if summary.Aborted && summary.RunManifest == nil && !summary.hasProgressTotal && summary.FileCount == 0 {
		// Older sessions did not persist review_progress. Request records still
		// provide a stable denominator while the review is running.
		summary.FileCount = len(summary.SelectedPaths)
		if summary.FileCount == 0 {
			summary.FileCount = requestedItemCount(requestedPaths)
		}
		if summary.FileCount == 0 && summary.FilesReadCount > 0 {
			// Very old sessions may contain only terminal item records. Showing
			// those observed files is more useful than a permanent zero percent.
			summary.FileCount = summary.FilesReadCount
		}
	}
	return summary, readErr
}

func isReviewItemRecord(typ string) bool {
	switch typ {
	case "review_item_done", "review_item_reused", "review_item_failed":
		return true
	default:
		return false
	}
}

func addReviewedFile(seen map[string]struct{}, rec map[string]any) bool {
	path, _ := rec["filePath"].(string)
	if path == "" {
		path, _ = rec["newPath"].(string)
	}
	if path == "" {
		return false
	}
	if _, exists := seen[path]; exists {
		return false
	}
	seen[path] = struct{}{}
	return true
}

func applyReviewProgress(summary *SessionSummary, rec map[string]any) {
	v, ok := rec["selected_count"].(float64)
	if !ok || v < 0 || int(v) < 0 || v != float64(int(v)) || summary.hasProgressTotal || summary.RunManifest != nil {
		return
	}
	// The denominator is frozen before dispatch and must not change as more
	// conversations (including virtual grouping/summary tasks) are recorded.
	summary.SelectedCount = int(v)
	summary.FileCount = int(v)
	summary.hasProgressTotal = true
	if paths, ok := rec["selected_paths"].([]any); ok {
		for _, raw := range paths {
			if path, ok := raw.(string); ok && path != "" {
				addSelectedPath(summary, path)
			}
		}
	}
	if summary.SelectedCount == 0 && len(summary.SelectedPaths) > 0 {
		summary.SelectedCount = len(summary.SelectedPaths)
		summary.FileCount = summary.SelectedCount
	}
}

func addSelectedPath(summary *SessionSummary, path string) {
	if path == "" {
		return
	}
	if summary.selectedPathSet == nil {
		summary.selectedPathSet = make(map[string]struct{}, len(summary.SelectedPaths)+1)
		for _, selected := range summary.SelectedPaths {
			summary.selectedPathSet[selected] = struct{}{}
		}
	}
	if _, exists := summary.selectedPathSet[path]; exists {
		return
	}
	summary.selectedPathSet[path] = struct{}{}
	summary.SelectedPaths = append(summary.SelectedPaths, path)
}

func addActiveRequest(requested map[string]struct{}, path, taskType string) {
	if path == "" {
		return
	}
	if taskType != string(session.PlanTask) && taskType != string(session.MainTask) {
		return
	}
	requested[path] = struct{}{}
}

func requestedItemPaths(requested map[string]struct{}) map[string]struct{} {
	items := make(map[string]struct{})
	for group := range requested {
		for _, path := range strings.Split(group, ",") {
			path = strings.TrimSpace(path)
			if path != "" {
				items[path] = struct{}{}
			}
		}
	}
	return items
}

func requestedItemCount(requested map[string]struct{}) int {
	return len(requestedItemPaths(requested))
}

func activeRequestedItemCount(requested, completed map[string]struct{}) int {
	items := requestedItemPaths(requested)
	for path := range completed {
		delete(items, path)
	}
	return len(items)
}

func applyStartedItems(summary *SessionSummary, active map[string]struct{}, rec map[string]any) {
	paths, ok := rec["file_paths"].([]any)
	if !ok {
		return
	}
	for _, raw := range paths {
		path, ok := raw.(string)
		if ok && path != "" {
			active[path] = struct{}{}
			addSelectedPath(summary, path)
		}
	}
}

func finishActiveItem(summary *SessionSummary, active map[string]struct{}, rec map[string]any) {
	path, _ := rec["filePath"].(string)
	if path == "" {
		path, _ = rec["newPath"].(string)
	}
	if path != "" {
		delete(active, path)
	}
}

func countReviewItem(summary *SessionSummary, typ string) {
	summary.FilesReadCount++
	switch typ {
	case "review_item_done":
		summary.CompletedCount++
	case "review_item_reused":
		summary.ReusedCount++
	case "review_item_failed":
		summary.FailedCount++
	}
}

// readJSONLLines visits each physical JSONL record without bufio.Scanner's
// fixed token ceiling. session_end embeds the complete run manifest and can
// legitimately exceed the former 10 MiB scanner limit on very large reviews.
func readJSONLLines(r io.Reader, visit func([]byte)) error {
	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		switch err {
		case nil:
			visit(line)
			continue
		case io.EOF:
			if len(line) > 0 {
				visit(line)
			}
			return nil
		default:
			// ReadBytes may return partial data together with a non-EOF error.
			// Discard it rather than presenting a corrupt fragment as a JSONL
			// record to callers.
			return err
		}
	}
}

// ReviewComment represents a single code review finding from a session.
type ReviewComment struct {
	FilePath            string
	Content             string
	SuggestionCode      string
	PendingConfirmation string
	ExistingCode        string
	StartLine           int
	EndLine             int
	Category            string // bug, security, performance, maintainability, test, style, documentation, other
	Severity            string // critical, high, medium, low
	CodeAttribution     *model.CodeAttribution
	MarkID              string `json:"-"`
	ID                  string `json:"-"`
	Reused              bool   `json:"-"`
}

// ViewSession holds fully parsed records for one session.
type ViewSession struct {
	Summary      SessionSummary
	TokenUsage   TokenUsageSummary
	Files        []*FileGroup     // ordered by file path
	SessionTasks []*FileGroup     // session-level tasks (grouping, etc.) separated from file-level
	Comments     []*ReviewComment // review findings from review_item_done/reused records
}

// TokenUsageSummary aggregates token counts across the session.
type TokenUsageSummary struct {
	TotalPromptTokens     int
	TotalCompletionTokens int
	TotalCacheReadTokens  int
	TotalCacheWriteTokens int
	RequestCount          int
	FileTokenBreakdown    []FileTokenUsage
}

// FileTokenUsage tracks token totals for a single file within a session.
type FileTokenUsage struct {
	FilePath         string
	PromptTokens     int
	CompletionTokens int
	CacheReadTokens  int
	CacheWriteTokens int
}

// FileGroup aggregates records for a single file.
type FileGroup struct {
	FilePath string
	Tasks    map[TaskType][]*TaskCard
}

// TaskType mirrors session.TaskType.
type TaskType string

const (
	PlanTask              TaskType = "plan_task"
	MainTask              TaskType = "main_task"
	MemoryCompressionTask TaskType = "memory_compression_task"
	ReLocationTask        TaskType = "re_location_task"
	GroupingTask          TaskType = "grouping_task"
)

var sessionLevelPaths = map[string]bool{
	"__grouping__": true,
}

func isSessionLevelPath(fp string) bool {
	return sessionLevelPaths[fp]
}

// TaskCard links an LLM request with its response and tool calls.
type TaskCard struct {
	RequestMessages  any // preserved for display
	RequestNo        int
	ResponseContent  string
	ReasoningContent string // the model's reasoning/thinking text for this turn, if the provider exposed any
	ToolCalls        []ToolCallInfo
	DurationMs       int64
	Error            string
	Model            string
	PromptTokens     int
	CompletionTokens int
	CacheReadTokens  int
	CacheWriteTokens int
}

// ToolCallInfo summarizes a single tool call.
type ToolCallInfo struct {
	Name       string
	Arguments  string
	Result     string
	Ok         bool
	DurationMs int64
}

// GroupingFileRef is one file inside a grouping group, resolved from the integer
// index the model returned back to its path. Resolved is false when the index
// named no file in the request's list — rendered as "#<idx>" so an auditor sees
// the anomaly rather than a silently dropped entry.
type GroupingFileRef struct {
	Index    int
	Path     string
	Resolved bool
}

// GroupingGroupView is one semantic group as shown in the viewer: the model's
// label plus its files resolved back to paths. It represents the grouping LLM
// call's *proposed* partition — the record this card holds. The final partition
// the reviewer actually used can differ, because enforceMaxFilesPerGroup and
// enforceGroupTokenBudget may re-split it afterwards; that is not part of this
// record (it surfaces only in the CLI `--format json` "groups" field).
type GroupingGroupView struct {
	Label string
	Files []GroupingFileRef
}

// groupingFileListRe matches one line of the numbered file list buildFileList
// emits into the grouping request, e.g. "[0] MODIFIED   internal/x.go (+12/-3)".
// It couples the viewer to buildFileList's "[%d] " prefix + formatDiffEntry's
// "STATUS   path (+N/-M)" shape (internal/agent/grouping.go, agent/agent.go);
// TestBuildGroupingIndex guards that coupling. The trailing "(+N/-M)" anchors
// the path capture so a path with spaces still resolves.
var groupingFileListRe = regexp.MustCompile(`(?m)^\[(\d+)\]\s+\S+\s+(.+?)\s+\(\+\d+/-\d+\)\s*$`)

// buildGroupingIndex scans the grouping request messages for the numbered file
// list and returns an index→path map. reqMessages is the raw JSONL value
// (a []any of map[string]any with a string "content"). Returns nil if nothing
// parses, which makes groupingView fall back to the raw response text.
func buildGroupingIndex(reqMessages any) map[int]string {
	msgs, ok := reqMessages.([]any)
	if !ok {
		return nil
	}
	index := make(map[int]string)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		// Only the user message carries the actual file list. Scanning the system
		// prompt too would let its worked example (grouping_task_system.md's
		// "[0] MODIFIED path ...") — which users can reword onto its own line —
		// seed a bogus index→path entry that silently mislabels an out-of-range
		// index instead of showing it as "#idx".
		if role, _ := mm["role"].(string); role != "user" {
			continue
		}
		content, ok := mm["content"].(string)
		if !ok {
			continue
		}
		for _, match := range groupingFileListRe.FindAllStringSubmatch(content, -1) {
			idx, err := strconv.Atoi(match[1])
			if err != nil {
				continue
			}
			index[idx] = match[2]
		}
	}
	if len(index) == 0 {
		return nil
	}
	return index
}

// groupingResponseView mirrors one element of the grouping LLM response (label
// plus integer file indices) as the viewer consumes it.
type groupingResponseView struct {
	Label string `json:"label"`
	Files []int  `json:"files"`
}

// parseGroupingGroups parses the grouping LLM response into label + integer
// indices. It mirrors parseGroupingResponse's one-shot Unmarshal and
// markdown-fence stripping. ok is false when the content is not the index-shaped
// JSON — a parse failure, or (notably) a session recorded before the index
// switch whose "files" held path strings; the caller then keeps showing the raw
// response, which for those older sessions is already the readable path form.
func parseGroupingGroups(content string) (groups []groupingResponseView, ok bool) {
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "```") {
		lines := strings.Split(content, "\n")
		if len(lines) >= 2 {
			lines = lines[1:]
		}
		if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "```") {
			lines = lines[:len(lines)-1]
		}
		content = strings.Join(lines, "\n")
	}
	if err := json.Unmarshal([]byte(content), &groups); err != nil {
		return nil, false
	}
	return groups, true
}

// groupingView resolves a grouping task card into a path-labelled view of the
// model's proposed groups, or nil when either side is missing/unparseable (the
// template then falls back to the raw response text).
func groupingView(card *TaskCard) []GroupingGroupView {
	if card == nil {
		return nil
	}
	groups, ok := parseGroupingGroups(card.ResponseContent)
	if !ok {
		return nil
	}
	index := buildGroupingIndex(card.RequestMessages)
	if index == nil {
		return nil
	}
	views := make([]GroupingGroupView, 0, len(groups))
	anyResolved := false
	for _, g := range groups {
		gv := GroupingGroupView{Label: g.Label, Files: make([]GroupingFileRef, 0, len(g.Files))}
		for _, idx := range g.Files {
			path, resolved := index[idx]
			anyResolved = anyResolved || resolved
			gv.Files = append(gv.Files, GroupingFileRef{Index: idx, Path: path, Resolved: resolved})
		}
		views = append(views, gv)
	}
	// The request list matched the regex (index != nil) yet not one referenced
	// index resolved: the list format has drifted from what the response indexes
	// into. Rendering every file as "#idx" would be more misleading than the raw
	// response, so fall back to it.
	if !anyResolved {
		return nil
	}
	return views
}

// safeSegment validates that s is safe to use as a single path component and
// returns it unchanged. Every caller must join the returned value, not its
// own copy of s: a validate-then-use-the-original-variable shape leaves the
// join sourced from the untrusted argument, which static path-injection
// analysis (this codebase's CodeQL scan included) cannot tell apart from a
// join with no check at all. Returning the checked value is what makes the
// join provably downstream of the check.
func safeSegment(s string) (string, error) {
	if s == "" || strings.Contains(s, "..") || strings.ContainsAny(s, `/\`) {
		return "", fmt.Errorf("invalid path segment: %q", s)
	}
	return s, nil
}

// LoadSession fully parses a JSONL file into a ViewSession.
func LoadSession(root, encodedRepo, sessionID string) (*ViewSession, error) {
	encodedRepo, err := safeSegment(encodedRepo)
	if err != nil {
		return nil, err
	}
	sessionID, err = safeSegment(sessionID)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, encodedRepo, sessionID+".jsonl")
	f, err := os.Open(path) //nolint:gosec // encodedRepo and sessionID are validated by safeSegment above
	if err != nil {
		return nil, fmt.Errorf("open session file: %w", err)
	}
	defer f.Close()

	vs := &ViewSession{Files: make([]*FileGroup, 0)}
	vs.Summary.Aborted = true
	fileIndex := make(map[string]*FileGroup)
	markOccurrences := make(map[string]int)
	readFiles := make(map[string]struct{})
	requestedPaths := make(map[string]struct{})
	activeItems := make(map[string]struct{})
	startedItemsSeen := false

	readErr := readJSONLLines(f, func(line []byte) {
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			return // skip malformed lines
		}
		typ, _ := rec["type"].(string)

		switch typ {
		case "session_start":
			if ts, ok := rec["timestamp"].(string); ok {
				vs.Summary.Timestamp, _ = time.Parse(time.RFC3339, ts)
			}
			if cwd, ok := rec["cwd"].(string); ok {
				vs.Summary.CWD = cwd
			}
			if branch, ok := rec["gitBranch"].(string); ok {
				vs.Summary.GitBranch = branch
			}
			if model, ok := rec["model"].(string); ok {
				vs.Summary.Model = model
			}
			if rm, ok := rec["reviewMode"].(string); ok {
				vs.Summary.ReviewMode = rm
			}
			if v, ok := rec["diffFrom"].(string); ok {
				vs.Summary.DiffFrom = v
			}
			if v, ok := rec["diffTo"].(string); ok {
				vs.Summary.DiffTo = v
			}
			if v, ok := rec["diffCommit"].(string); ok {
				vs.Summary.DiffCommit = v
			}

		case "review_progress":
			applyReviewProgress(&vs.Summary, rec)

		case "review_item_started":
			startedItemsSeen = true
			applyStartedItems(&vs.Summary, activeItems, rec)

		case "llm_request":
			fp, _ := rec["filePath"].(string)
			tt, _ := rec["taskType"].(string)
			if fp != "__grouping__" && fp != "" {
				addActiveRequest(requestedPaths, fp, tt)
			}
			reqNo := 0
			if n, ok := rec["request_no"].(float64); ok {
				reqNo = int(n)
			}
			msgs := rec["messages"]

			tc := &TaskCard{RequestMessages: msgs, RequestNo: reqNo}

			fg := fileIndex[fp]
			if fg == nil {
				fg = &FileGroup{FilePath: fp, Tasks: make(map[TaskType][]*TaskCard)}
				fileIndex[fp] = fg
				vs.Files = append(vs.Files, fg)
			}
			fg.Tasks[TaskType(tt)] = append(fg.Tasks[TaskType(tt)], tc)

		case "llm_response":
			fp, _ := rec["filePath"].(string)
			content, _ := rec["content"].(string)
			reasoning, _ := rec["reasoning_content"].(string)
			durationMs := int64(0)
			if d, ok := rec["duration_ms"].(float64); ok {
				durationMs = int64(d)
			}
			model, _ := rec["model"].(string)
			errStr, _ := rec["error"].(string)

			promptTok := 0
			completionTok := 0
			cacheReadTok := 0
			cacheWriteTok := 0
			if usage, ok := rec["usage"].(map[string]any); ok {
				if v, ok := usage["prompt_tokens"].(float64); ok {
					promptTok = int(v)
				}
				if v, ok := usage["completion_tokens"].(float64); ok {
					completionTok = int(v)
				}
				if v, ok := usage["cache_read_tokens"].(float64); ok {
					cacheReadTok = int(v)
				}
				if v, ok := usage["cache_write_tokens"].(float64); ok {
					cacheWriteTok = int(v)
				}
			}

			tt, _ := rec["taskType"].(string)
			fg := fileIndex[fp]
			if fg != nil {
				cards := fg.Tasks[TaskType(tt)]
				if len(cards) > 0 && cards[len(cards)-1].ResponseContent == "" {
					card := cards[len(cards)-1]
					card.ResponseContent = content
					card.ReasoningContent = reasoning
					card.DurationMs = durationMs
					card.Model = model
					card.Error = errStr
					card.PromptTokens = promptTok
					card.CompletionTokens = completionTok
					card.CacheReadTokens = cacheReadTok
					card.CacheWriteTokens = cacheWriteTok
				}
			}

			// Also attach tool_calls to the same card
			if tcs, ok := rec["tool_calls"].([]any); ok && fg != nil {
				tt, _ := rec["taskType"].(string)
				cards := fg.Tasks[TaskType(tt)]
				if len(cards) > 0 {
					card := cards[len(cards)-1]
					for _, tc := range tcs {
						if tm, ok := tc.(map[string]any); ok {
							name, _ := tm["name"].(string)
							args, _ := tm["arguments"].(string)
							info := ToolCallInfo{Name: name, Arguments: args}
							if name == "task_done" {
								info.Ok = taskDoneSucceeded(args)
							} else if name == "report_incorrect_comments" || name == "approve_all_comments" {
								info.Ok = true
							}
							card.ToolCalls = append(card.ToolCalls, info)
						}
					}
				}
			}

		case "llm_error":
			fp, _ := rec["filePath"].(string)
			tt, _ := rec["taskType"].(string)
			errStr, _ := rec["error"].(string)
			durationMs := int64(0)
			if d, ok := rec["duration_ms"].(float64); ok {
				durationMs = int64(d)
			}

			fg := fileIndex[fp]
			if fg != nil {
				cards := fg.Tasks[TaskType(tt)]
				if len(cards) > 0 && cards[len(cards)-1].Error == "" {
					card := cards[len(cards)-1]
					card.Error = errStr
					card.DurationMs = durationMs
				}
			}

		case "tool_call":
			toolName, _ := rec["tool_name"].(string)
			result, _ := rec["result"].(string)
			okVal := true
			if b, hasOk := rec["ok"].(bool); hasOk {
				okVal = b
			}
			fp, _ := rec["filePath"].(string)
			tt, _ := rec["taskType"].(string)
			durationMs := int64(0)
			if d, ok2 := rec["duration_ms"].(float64); ok2 {
				durationMs = int64(d)
			}

			fg := fileIndex[fp]
			if fg != nil {
				cards := fg.Tasks[TaskType(tt)]
				if len(cards) > 0 {
					card := cards[len(cards)-1]
					for ti := range card.ToolCalls {
						// Older session records omitted tool_name, so retain
						// positional matching only for those records.
						if (toolName == "" || card.ToolCalls[ti].Name == toolName) &&
							card.ToolCalls[ti].Result == "" && !card.ToolCalls[ti].Ok {
							card.ToolCalls[ti].Result = result
							card.ToolCalls[ti].Ok = okVal
							card.ToolCalls[ti].DurationMs = durationMs
							break
						}
					}
				}
			}

		case "review_item_done", "review_item_reused", "review_item_failed":
			finishActiveItem(&vs.Summary, activeItems, rec)
			if addReviewedFile(readFiles, rec) {
				countReviewItem(&vs.Summary, typ)
			}
			if typ == "review_item_failed" {
				break
			}
			fp, _ := rec["filePath"].(string)
			recUUID, _ := rec["uuid"].(string)
			if comments, ok := rec["comments"].([]any); ok {
				for ci, c := range comments {
					cm, ok := c.(map[string]any)
					if !ok {
						continue
					}
					rc := &ReviewComment{FilePath: fp, Reused: typ == "review_item_reused"}
					if v, ok := cm["path"].(string); ok && v != "" {
						rc.FilePath = v
					}
					if v, ok := cm["content"].(string); ok {
						rc.Content = v
					}
					if v, ok := cm["suggestion_code"].(string); ok {
						rc.SuggestionCode = v
					}
					if v, ok := cm["pending_confirmation"].(string); ok {
						rc.PendingConfirmation = v
					}
					if v, ok := cm["existing_code"].(string); ok {
						rc.ExistingCode = v
					}
					if v, ok := cm["start_line"].(float64); ok {
						rc.StartLine = int(v)
					}
					if v, ok := cm["end_line"].(float64); ok {
						rc.EndLine = int(v)
					}
					if v, ok := cm["category"].(string); ok {
						rc.Category = v
					}
					if v, ok := cm["severity"].(string); ok {
						rc.Severity = v
					}
					if raw, ok := cm["code_attribution"]; ok {
						if data, err := json.Marshal(raw); err == nil {
							_ = json.Unmarshal(data, &rc.CodeAttribution)
						}
					}
					rc.MarkID = commentMarkID(recUUID, ci, rc, markOccurrences)
					rc.ID = strconv.Itoa(len(vs.Comments) + 1)
					vs.Comments = append(vs.Comments, rc)
				}
			}

		case "session_end":
			applySessionEnd(&vs.Summary, rec)
		}
	})
	if startedItemsSeen {
		vs.Summary.ActiveCount = len(activeItems)
	} else {
		vs.Summary.ActiveCount = activeRequestedItemCount(requestedPaths, readFiles)
	}
	if !vs.Summary.Aborted && vs.Summary.RunManifest == nil && !vs.Summary.hasProgressTotal && vs.Summary.FileCount < vs.Summary.FilesReadCount {
		vs.Summary.FileCount = vs.Summary.FilesReadCount
	}
	if vs.Summary.Aborted && vs.Summary.RunManifest == nil && !vs.Summary.hasProgressTotal && vs.Summary.FileCount == 0 {
		vs.Summary.FileCount = len(vs.Summary.SelectedPaths)
		if vs.Summary.FileCount == 0 {
			vs.Summary.FileCount = requestedItemCount(requestedPaths)
		}
		if vs.Summary.FileCount == 0 && vs.Summary.FilesReadCount > 0 {
			vs.Summary.FileCount = vs.Summary.FilesReadCount
		}
	}

	// Aggregate token usage across all task cards
	fileBreakdown := make([]FileTokenUsage, 0, len(vs.Files))
	for _, fg := range vs.Files {
		ft := FileTokenUsage{FilePath: fg.FilePath}
		for _, cards := range fg.Tasks {
			for _, c := range cards {
				vs.TokenUsage.TotalPromptTokens += c.PromptTokens
				vs.TokenUsage.TotalCompletionTokens += c.CompletionTokens
				vs.TokenUsage.TotalCacheReadTokens += c.CacheReadTokens
				vs.TokenUsage.TotalCacheWriteTokens += c.CacheWriteTokens
				if c.ResponseContent != "" || c.PromptTokens > 0 {
					vs.TokenUsage.RequestCount++
				}
				ft.PromptTokens += c.PromptTokens
				ft.CompletionTokens += c.CompletionTokens
				ft.CacheReadTokens += c.CacheReadTokens
				ft.CacheWriteTokens += c.CacheWriteTokens
			}
		}
		fileBreakdown = append(fileBreakdown, ft)
	}
	sort.Slice(fileBreakdown, func(i, j int) bool {
		return fileBreakdown[i].PromptTokens+fileBreakdown[i].CompletionTokens > fileBreakdown[j].PromptTokens+fileBreakdown[j].CompletionTokens
	})
	vs.TokenUsage.FileTokenBreakdown = fileBreakdown

	// Separate session-level virtual paths from real file paths.
	realFiles := make([]*FileGroup, 0, len(vs.Files))
	for _, fg := range vs.Files {
		if isSessionLevelPath(fg.FilePath) {
			vs.SessionTasks = append(vs.SessionTasks, fg)
		} else {
			realFiles = append(realFiles, fg)
		}
	}
	vs.Files = realFiles

	sort.Slice(vs.Files, func(i, j int) bool {
		return vs.Files[i].FilePath < vs.Files[j].FilePath
	})

	vs.Summary.SessionID = sessionID
	vs.Summary.CommentCount = len(vs.Comments)
	return vs, readErr
}

// commentMarkID returns a stable identity for one comment within an immutable
// session file. Records carry a top-level uuid: uuid#commentIndex is exact,
// collision-free, and identical on every reload. Legacy records without a
// uuid fall back to a sha256 over every comment field plus an occurrence
// counter, so exact duplicate comments still get distinct identities.
func commentMarkID(recordUUID string, commentIndex int, rc *ReviewComment, occurrences map[string]int) string {
	if recordUUID != "" {
		return fmt.Sprintf("%s#%d", recordUUID, commentIndex)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%s\x00%s",
		rc.FilePath, rc.Content, rc.SuggestionCode, rc.ExistingCode,
		rc.StartLine, rc.EndLine, rc.Category, rc.Severity)))
	key := hex.EncodeToString(sum[:])
	n := occurrences[key]
	occurrences[key] = n + 1
	return fmt.Sprintf("%s#%d", key, n)
}

func applySessionEnd(summary *SessionSummary, rec map[string]any) {
	summary.Aborted = false
	if dur, ok := rec["duration_seconds"].(float64); ok {
		summary.DurationSec = dur
	}
	if files, ok := rec["files_reviewed"].([]any); ok {
		summary.FilesReviewed = make([]string, 0, len(files))
		for _, fv := range files {
			if s, ok := fv.(string); ok {
				summary.FilesReviewed = append(summary.FilesReviewed, s)
			}
		}
	}
	if f, ok := rec["llm_failures"].(float64); ok {
		summary.LLMFailures = int(f)
	}

	if raw, ok := rec["run_manifest"]; ok {
		data, err := json.Marshal(raw)
		if err == nil {
			var manifest session.RunManifest
			if err := json.Unmarshal(data, &manifest); err == nil && manifest.SchemaVersion == session.ManifestSchemaVersion {
				summary.RunManifest = &manifest
				summary.TerminalState = string(manifest.TerminalState)
				summary.FilesReviewed = filesReviewedFromSelected(manifest.Coverage.Selected)
				summary.SelectedCount = len(manifest.Coverage.Selected)
				summary.CompletedCount = len(manifest.Coverage.Completed)
				summary.ReusedCount = len(manifest.Coverage.Reused)
				summary.FailedCount = len(manifest.Coverage.Failed)
				summary.WaivedCount = len(manifest.Coverage.Waived)
				summary.FileCount = summary.SelectedCount
			}
		}
	}
	if summary.RunManifest == nil {
		summary.Legacy = true
		if !summary.hasProgressTotal {
			summary.FileCount = len(summary.FilesReviewed)
		}
	}
}

func filesReviewedFromSelected(selected []session.CoverageItem) []string {
	files := make([]string, 0, len(selected))
	for _, item := range selected {
		files = append(files, item.Path)
	}
	return files
}

func taskDoneSucceeded(arguments string) bool {
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return false
	}
	state, hasState := args["state"]
	if !hasState {
		return true
	}
	stateString, ok := state.(string)
	return ok && stateString == "DONE"
}
