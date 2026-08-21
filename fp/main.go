package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	out, err := paths(os.Args[1:], "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fp: %v\n", err)
		os.Exit(1)
	}
	for _, p := range out {
		fmt.Println(p)
	}
}

// paths resolves each argument to a clean, absolute path with symlinks evaluated.
// An empty args slice defaults to ".". When wd is empty, the current directory is used.
func paths(args []string, wd string) ([]string, error) {
	if wd == "" {
		var err error
		wd, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("getwd: %w", err)
		}
	}
	if len(args) == 0 {
		args = []string{"."}
	}

	out := make([]string, len(args))
	for i, arg := range args {
		p, err := resolvePath(wd, arg)
		if err != nil {
			return nil, err
		}
		out[i] = p
	}
	return out, nil
}

func resolvePath(wd, arg string) (string, error) {
	p := arg
	if !filepath.IsAbs(p) {
		p = filepath.Join(wd, p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("%q: %w", arg, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("%q: %w", arg, err)
	}
	return resolved, nil
}
