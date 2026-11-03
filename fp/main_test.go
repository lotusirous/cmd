package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPaths(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		links   map[string]string
		args    []string
		want    []string // relative; defaults to args or "."
		wantErr bool
	}{
		{name: "default"},
		{name: "single", files: map[string]string{"notes.txt": "x"}, args: []string{"notes.txt"}},
		{name: "nested", files: map[string]string{"a/b/c/.keep": ""}, args: []string{"a/b/c"}},
		{name: "multiple", files: map[string]string{"src/main.go": "p", "docs/readme.md": "#"}, args: []string{".", "src/main.go", "docs/readme.md"}},
		{name: "symlink", files: map[string]string{"real/target.txt": "x"}, links: map[string]string{"link": "real/target.txt"}, args: []string{"link"}, want: []string{"real/target.txt"}},
		{name: "symlink_dir", files: map[string]string{"real/nested.txt": "x"}, links: map[string]string{"alias": "real"}, args: []string{"alias/nested.txt"}, want: []string{"real/nested.txt"}},
		{name: "missing", args: []string{"nope.txt"}, wantErr: true},
		{name: "broken_symlink", links: map[string]string{"broken": "missing.txt"}, args: []string{"broken"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := setup(t, tt.files, tt.links)

			got, err := paths(tt.args, dir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			want := tt.want
			if want == nil {
				want = tt.args
				if len(want) == 0 {
					want = []string{"."}
				}
			}
			exp, err := paths(want, dir)
			if err != nil {
				t.Fatal(err)
			}
			same := func(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }
			if !slices.EqualFunc(got, exp, same) {
				t.Fatalf("got %v\nwant %v", got, exp)
			}
		})
	}
}

func TestClip(t *testing.T) {
	dir := t.TempDir()
	got := filepath.Join(dir, "got")
	script := "#!/bin/sh\ncat >" + got + "\n"
	for _, name := range []string{"pbcopy", "wl-copy", "xclip"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	want := "/a/b\n/c d"
	if err := clip(want); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != want {
		t.Fatalf("clipboard = %q, want %q", b, want)
	}

	t.Setenv("PATH", t.TempDir())
	if err := clip(want); err == nil {
		t.Fatal("clip with no clipboard command: err = nil")
	}
}

func setup(t testing.TB, files, links map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, c := range files {
		p = filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for from, to := range links {
		from = filepath.Join(dir, filepath.FromSlash(from))
		to = filepath.Join(dir, filepath.FromSlash(to))
		if err := os.MkdirAll(filepath.Dir(from), 0o755); err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(filepath.Dir(from), to)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(rel, from); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
