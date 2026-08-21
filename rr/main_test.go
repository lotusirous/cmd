package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testClient returns srv's client with the same redirect policy rr uses.
func testClient(srv *httptest.Server) *http.Client {
	c := srv.Client()
	c.CheckRedirect = noRedirect
	return c
}

// writeFile writes an rr file and returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunGET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test") != "yes" {
			http.Error(w, "bad header", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "hello")
	}))
	defer srv.Close()

	req := "GET " + srv.URL + "/ HTTP/1.1\r\nHost: example.com\r\nX-Test: yes\r\n\r\n"
	path := writeFile(t, "get", req)

	var out bytes.Buffer
	if err := run(t.Context(), path, testClient(srv), &out); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "200") || !strings.Contains(out.String(), "hello") {
		t.Fatalf("stdout = %q", out.String())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte(req)) {
		t.Fatalf("request half changed:\n%s", data)
	}
	if !bytes.Contains(data, []byte("\n"+delim+"\n")) {
		t.Fatalf("missing delimiter:\n%s", data)
	}
	if !bytes.Contains(data, []byte("hello")) {
		t.Fatalf("missing response body:\n%s", data)
	}
}

func TestRunPOSTJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"n":1}`)
	}))
	defer srv.Close()

	req := "POST " + srv.URL + "/ HTTP/1.1\r\nHost: example.com\r\nContent-Length: 4\r\n\r\nbody"
	path := writeFile(t, "post", req)

	var out bytes.Buffer
	if err := run(t.Context(), path, testClient(srv), &out); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "{\n  \"ok\": true") {
		t.Fatalf("stdout not indented json:\n%s", out.String())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, respPart, ok := strings.Cut(string(data), delim+"\n")
	if !ok {
		t.Fatalf("missing delimiter:\n%s", data)
	}
	if !strings.Contains(respPart, "{\n  \"ok\": true") {
		t.Fatalf("stored response not indented:\n%s", respPart)
	}
	if !strings.Contains(respPart, "Content-Length:") {
		t.Fatalf("missing Content-Length:\n%s", respPart)
	}
}

func TestRunPOSTBodySent(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
	}))
	defer srv.Close()

	path := writeFile(t, "post", "POST "+srv.URL+"/ HTTP/1.1\r\nHost: example.com\r\nContent-Length: 4\r\n\r\nbody")
	if err := run(t.Context(), path, testClient(srv), io.Discard); err != nil {
		t.Fatal(err)
	}
	if b := <-got; b != "body" {
		t.Fatalf("server read %q, want %q", b, "body")
	}
}

// A chunked response has no length to preserve; it must be stored with the
// Content-Length of the body actually written, and no chunk framing.
func TestRunChunkedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "chun")
		w.(http.Flusher).Flush()
		io.WriteString(w, "ked")
	}))
	defer srv.Close()

	path := writeFile(t, "chunked", "GET "+srv.URL+"/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
	if err := run(t.Context(), path, testClient(srv), io.Discard); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, resp, _ := strings.Cut(string(data), delim+"\n")
	if strings.Contains(resp, "Transfer-Encoding") {
		t.Fatalf("chunked framing kept:\n%s", resp)
	}
	if !strings.Contains(resp, "Content-Length: 7") || !strings.HasSuffix(resp, "chunked") {
		t.Fatalf("body not stored plainly:\n%s", resp)
	}
}

func TestRunReplaceResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "v2")
	}))
	defer srv.Close()

	req := "GET " + srv.URL + "/ HTTP/1.1\r\nHost: example.com\r\n\r\n"
	path := writeFile(t, "req", req+delim+"\nHTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nv1")

	if err := run(t.Context(), path, testClient(srv), io.Discard); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte(req)) {
		t.Fatalf("request changed:\n%s", data)
	}
	if bytes.Count(data, []byte(delim)) != 1 {
		t.Fatalf("expected one delimiter:\n%s", data)
	}
	if !bytes.Contains(data, []byte("v2")) || bytes.Contains(data, []byte("v1")) {
		t.Fatalf("response not replaced:\n%s", data)
	}
}

func TestRunKeepsMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	path := writeFile(t, "mode", "GET "+srv.URL+"/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), path, testClient(srv), io.Discard); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %v, want %v", got, os.FileMode(0o640))
	}
}

func TestRunEnvExpand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	t.Setenv("TOKEN", "secret")

	path := writeFile(t, "auth", "GET "+srv.URL+"/ HTTP/1.1\r\nHost: example.com\r\nAuthorization: Bearer ${TOKEN}\r\n\r\n")
	if err := run(t.Context(), path, testClient(srv), io.Discard); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "${TOKEN}") {
		t.Fatalf("token expanded on disk:\n%s", data)
	}
}

func TestRunEnvUnset(t *testing.T) {
	path := writeFile(t, "unset", "GET https://example.com/$ALSO HTTP/1.1\r\nHost: example.com\r\nAuthorization: Bearer ${MISSING}\r\n\r\n")

	before, _ := os.ReadFile(path)
	err := run(t.Context(), path, http.DefaultClient, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "MISSING") || !strings.Contains(err.Error(), "ALSO") {
		t.Fatalf("err = %v, want both unset names", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("file changed on env error")
	}
}

func TestRunOriginForm(t *testing.T) {
	path := writeFile(t, "origin", "GET /path HTTP/1.1\r\nHost: example.com\r\n\r\n")

	err := run(t.Context(), path, http.DefaultClient, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("err = %v, want absolute-form error", err)
	}
}

func TestRunDirectory(t *testing.T) {
	dir := t.TempDir()
	err := run(t.Context(), dir, http.DefaultClient, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("err = %v, want directory error", err)
	}
}

func TestRunTransportError(t *testing.T) {
	req := "GET http://127.0.0.1:1/nope HTTP/1.1\r\nHost: 127.0.0.1:1\r\n\r\n"
	old := req + delim + "\nHTTP/1.1 500 Old\r\nContent-Length: 3\r\n\r\nold"
	path := writeFile(t, "fail", old)

	client := &http.Client{CheckRedirect: noRedirect}
	err := run(t.Context(), path, client, io.Discard)
	if err == nil {
		t.Fatal("expected transport error")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal([]byte(old), after) {
		t.Fatalf("file changed on transport error:\n%s", after)
	}
}

func TestSplitFile(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"request only", "GET https://x/\n\n", "GET https://x/\n\n"},
		{"with response", "GET https://x/\n\n" + delim + "\nHTTP/1.1 200\n\n", "GET https://x/\n\n"},
		{"cr delimiter", "GET https://x/\r\n" + delim + "\r\nHTTP/1.1 200\r\n\r\n", "GET https://x/\r\n"},
		{"delimiter first", delim + "\nHTTP/1.1 200\n", ""},
		{"delimiter unterminated", "GET https://x/\n" + delim, "GET https://x/\n"},
		{"delimiter mid-line", "GET https://x/----\n", "GET https://x/----\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := splitFile([]byte(tt.in)); string(got) != tt.want {
				t.Fatalf("req = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("NAME", "value")
	t.Setenv("DOLLAR", "a$NAME b${NAME}")

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{"none", "no variables here", "no variables here", ""},
		{"brace", "x ${NAME} y", "x value y", ""},
		{"plain", "x $NAME y", "x value y", ""},
		{"adjacent", "$NAME${NAME}", "valuevalue", ""},
		// A substituted value is data, not a template: it must not be rescanned.
		{"no reexpansion", "${DOLLAR}", "a$NAME b${NAME}", ""},
		{"digit is not a name", `{"price":"$5"}`, `{"price":"$5"}`, ""},
		{"bare dollars", "$$ $ ${}", "$$ $ ${}", ""},
		{"unset", "${A} $B", "", "unset: A, B"},
		{"partly unset", "${NAME} ${GONE}", "", "unset: GONE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expandEnv([]byte(tt.in))
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
