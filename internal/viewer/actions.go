// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package viewer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Session mutations require an explicit same-origin browser request. The custom
// header prevents cross-origin forms from deleting local review records.
func handleDeleteSession(w http.ResponseWriter, r *http.Request, root, repo, id string) {
	origin, err := url.Parse(r.Header.Get("Origin"))
	scheme, host := requestOriginTarget(r)
	if err != nil || origin.Scheme != scheme || origin.Host != host || origin.User != nil || origin.Path != "" || r.Header.Get("X-OCR-Confirm") != "delete" {
		http.Error(w, "same-origin deletion confirmation required", http.StatusForbidden)
		return
	}
	// os.Root also prevents symlink/junction escapes from the sessions root.
	dir, err := os.OpenRoot(root)
	if err != nil {
		http.Error(w, "sessions unavailable", http.StatusInternalServerError)
		return
	}
	defer dir.Close()
	if err := dir.Remove(filepath.Join(repo, id+".jsonl")); err != nil {
		status := http.StatusInternalServerError
		if os.IsNotExist(err) {
			status = http.StatusNotFound
		}
		http.Error(w, "could not delete session", status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleDeleteRepository(w http.ResponseWriter, r *http.Request, root, repo string) {
	if !sameOriginDelete(r) {
		http.Error(w, "same-origin deletion confirmation required", http.StatusForbidden)
		return
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		http.Error(w, "sessions unavailable", http.StatusInternalServerError)
		return
	}
	defer dir.Close()
	if err := dir.RemoveAll(repo); err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		http.Error(w, "could not delete repository", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func sameOriginDelete(r *http.Request) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	scheme, host := requestOriginTarget(r)
	return err == nil && origin.Scheme == scheme && origin.Host == host && origin.User == nil && origin.Path == "" && r.Header.Get("X-OCR-Confirm") == "delete"
}

func requestOriginTarget(r *http.Request) (string, string) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); forwarded == "http" || forwarded == "https" {
		scheme = forwarded
	}
	host := r.Host
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); forwarded != "" {
		host = forwarded
	}
	return scheme, host
}

func handleMarkdown(w http.ResponseWriter, r *http.Request, root, repo, id string) {
	handleSessionExport(w, r, root, repo, id, "md")
}

func handleSessionExport(w http.ResponseWriter, r *http.Request, root, repo, id, format string) {
	vs, err := LoadSession(root, repo, id)
	if err != nil {
		http.Error(w, "could not export session", http.StatusNotFound)
		return
	}
	var out bytes.Buffer
	language := exportLanguageFromRequest(r.URL.Query().Get("lang"), r.Header.Get("Accept-Language"))
	contentType, extension := "text/html; charset=utf-8", "html"
	switch format {
	case "html":
		if err := ExportSessionWithLanguage(&out, root, repo, id, language); err != nil {
			http.Error(w, "could not export session", http.StatusNotFound)
			return
		}
	case "md":
		if err := ExportSessionMarkdownWithLanguage(&out, root, repo, id, language); err != nil {
			http.Error(w, "could not export session", http.StatusNotFound)
			return
		}
		contentType, extension = "text/markdown; charset=utf-8", "md"
	case "json":
		payload := map[string]any{"summary": vs.Summary, "findings": vs.Comments}
		if err := json.NewEncoder(&out).Encode(payload); err != nil {
			http.Error(w, "could not export session", http.StatusInternalServerError)
			return
		}
		contentType, extension = "application/json; charset=utf-8", "json"
	case "sarif":
		results := make([]map[string]any, 0, len(vs.Comments))
		for _, c := range vs.Comments {
			results = append(results, map[string]any{"ruleId": c.ID, "level": c.Severity, "message": map[string]string{"text": c.Content}, "locations": []any{map[string]any{"physicalLocation": map[string]any{"artifactLocation": map[string]string{"uri": c.FilePath}, "region": map[string]int{"startLine": c.StartLine, "endLine": c.EndLine}}}}})
		}
		payload := map[string]any{"version": "2.1.0", "$schema": "https://json.schemastore.org/sarif-2.1.0.json", "runs": []any{map[string]any{"tool": map[string]any{"driver": map[string]string{"name": "open-code-review"}}, "results": results}}}
		if err := json.NewEncoder(&out).Encode(payload); err != nil {
			http.Error(w, "could not export session", http.StatusInternalServerError)
			return
		}
		contentType, extension = "application/sarif+json; charset=utf-8", "sarif"
	default:
		http.Error(w, "unsupported export format", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "review-" + id + "." + extension}))
	w.Header().Set("Content-Length", fmt.Sprint(out.Len()))
	_, _ = w.Write(out.Bytes())
}

func handleMarkdownLegacy(w http.ResponseWriter, r *http.Request, root, repo, id string) {
	var out bytes.Buffer
	if err := ExportSessionMarkdown(&out, root, repo, id); err != nil {
		http.Error(w, "could not export session", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "review-" + id + ".md"}))
	w.Header().Set("Content-Length", fmt.Sprint(out.Len()))
	w.Write(out.Bytes())
}
