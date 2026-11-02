package mattermost

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestUploadFileThenAttachIt(t *testing.T) {
	var post map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("%s: no bearer token", r.URL.Path)
		}
		switch r.URL.Path {
		case "/api/v4/files":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatalf("not multipart: %v", err)
			}
			if got := r.FormValue("channel_id"); got != "ch1" {
				t.Errorf("channel_id = %q", got)
			}
			fhs := r.MultipartForm.File["files"]
			if len(fhs) != 1 {
				t.Fatalf("files = %d, want 1", len(fhs))
			}
			fh := fhs[0]
			if fh.Filename != "report.pdf" || fh.Header.Get("Content-Type") != "application/pdf" {
				t.Errorf("file = %q %q", fh.Filename, fh.Header.Get("Content-Type"))
			}
			f, _ := fh.Open()
			data, _ := io.ReadAll(f)
			if string(data) != "%PDF-1.4 x" {
				t.Errorf("data = %q", data)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"file_infos":[{"id":"f1"}],"client_ids":[]}`))
		case "/api/v4/posts":
			if err := json.NewDecoder(r.Body).Decode(&post); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{"id":"p1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/", "tok")
	ctx := context.Background()
	id, err := c.UploadFile(ctx, "ch1", "report.pdf", "application/pdf", []byte("%PDF-1.4 x"))
	if err != nil || id != "f1" {
		t.Fatalf("UploadFile = %q, %v", id, err)
	}
	pid, err := c.CreatePostFiles(ctx, "ch1", "here", "root1", []string{id})
	if err != nil || pid != "p1" {
		t.Fatalf("CreatePostFiles = %q, %v", pid, err)
	}
	ids, _ := post["file_ids"].([]any)
	if post["channel_id"] != "ch1" || post["root_id"] != "root1" || !slices.Equal(ids, []any{"f1"}) {
		t.Errorf("post = %v", post)
	}
}

func TestUploadFileReportsTheServersRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"File attachments have been disabled on this server."}`, http.StatusNotImplemented)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "tok").UploadFile(context.Background(), "ch1", "r.pdf", "application/pdf", []byte("%PDF-"))
	if err == nil {
		t.Fatal("want an error")
	}
	if want := "File attachments have been disabled"; !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to carry %q", err, want)
	}
}
