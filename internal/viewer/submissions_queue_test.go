// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package viewer

import (
	"context"
	"path/filepath"
	"testing"
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

func TestDefaultReviewLimitAllowsFiveConcurrentTasks(t *testing.T) {
	if defaultReviewLimit != 5 {
		t.Fatalf("defaultReviewLimit = %d, want 5", defaultReviewLimit)
	}
}
