// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package viewer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/alibaba/open-code-review/internal/session"
)

func TestCancelPreventsLateWorkerUpdate(t *testing.T) {
	q := &reviewQueue{
		path:   filepath.Join(t.TempDir(), "submissions.json"),
		limit:  defaultReviewLimit,
		active: make(map[string]context.CancelFunc),
		items:  []ReviewSubmission{{ID: "task-1", Status: "running"}},
	}
	_, cancel := context.WithCancel(context.Background())
	q.active["task-1"] = cancel

	if !q.cancel("task-1") {
		t.Fatal("cancel() = false, want true")
	}
	q.update("task-1", "success", "session-1", "")

	if got := q.items[0].Status; got != "cancelled" {
		t.Fatalf("status after late update = %q, want cancelled", got)
	}
	if _, ok := q.active["task-1"]; !ok {
		t.Fatal("cancelled worker should remain active until its goroutine exits")
	}
}

func TestTerminalTaskRejectsLateWorkerUpdate(t *testing.T) {
	q := &reviewQueue{
		path:   filepath.Join(t.TempDir(), "submissions.json"),
		limit:  defaultReviewLimit,
		active: make(map[string]context.CancelFunc),
		items:  []ReviewSubmission{{ID: "task-1", Status: "running"}},
	}
	q.update("task-1", "failed", "session-1", "review session ended with failures")
	q.update("task-1", "success", "session-1", "")
	if got := q.items[0].Status; got != "failed" {
		t.Fatalf("status after late success = %q, want failed", got)
	}
}

func TestDefaultReviewLimitAllowsFiveConcurrentTasks(t *testing.T) {
	if defaultReviewLimit != 5 {
		t.Fatalf("defaultReviewLimit = %d, want 5", defaultReviewLimit)
	}
}

func TestRemoveSessionCancelsTaskBeforeSessionIDIsKnown(t *testing.T) {
	q := &reviewQueue{
		path:   filepath.Join(t.TempDir(), "submissions.json"),
		limit:  defaultReviewLimit,
		active: make(map[string]context.CancelFunc),
		items:  []ReviewSubmission{{ID: "task-1", RepoDir: filepath.Join("/tmp", "crm"), Status: "running"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	q.active["task-1"] = cancel
	q.removeSession("crm", "")
	select {
	case <-ctx.Done():
	default:
		t.Fatal("session deletion did not cancel the active task")
	}
	if len(q.items) != 0 {
		t.Fatalf("items after session deletion = %d, want 0", len(q.items))
	}
}

func TestWaitForReviewSessionRequiresSessionEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	repoDir := filepath.Join(t.TempDir(), "repo")
	path, err := session.SessionFilePath(repoDir, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	start := fmt.Sprintf(`{"type":"session_start","sessionId":"session-1","cwd":%q}`+"\n", repoDir)
	if err := os.WriteFile(path, []byte(start), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ended, _ := waitForReviewSession(ctx, repoDir, "session-1"); ended {
		t.Fatal("unfinished session was reported as terminal")
	}
	if err := os.WriteFile(path, []byte(start+`{"type":"session_end","files_reviewed":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ended, success := waitForReviewSession(context.Background(), repoDir, "session-1"); !ended || !success {
		t.Fatalf("completed session = ended:%v success:%v, want true,true", ended, success)
	}
	partial := fmt.Sprintf(`{"type":"session_start","sessionId":"session-1","cwd":%q}`+"\n", repoDir)
	partial += fmt.Sprintf(`{"type":"session_end","run_manifest":{"schema_version":%q,"terminal_state":"partial"}}`, session.ManifestSchemaVersion)
	if err := os.WriteFile(path, []byte(partial), 0o600); err != nil {
		t.Fatal(err)
	}
	if ended, success := waitForReviewSession(context.Background(), repoDir, "session-1"); !ended || !success {
		t.Fatalf("partial session = ended:%v success:%v, want true,true", ended, success)
	}
}
