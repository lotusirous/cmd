package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: fp [-c] [path ...]")
	flag.PrintDefaults()
	os.Exit(2)
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("fp: ")
	c := flag.Bool("c", false, "copy the paths to the clipboard as well")
	flag.Usage = usage
	flag.Parse()

	out, err := paths(flag.Args(), "")
	if err != nil {
		log.Fatal(err)
	}
	s := strings.Join(out, "\n")
	fmt.Println(s)
	if *c {
		if err := clip(s); err != nil {
			log.Fatal(err)
		}
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

// clip puts s on the system clipboard.
func clip(s string) error {
	argv := []string{"xclip", "-selection", "clipboard"}
	switch {
	case runtime.GOOS == "darwin":
		argv = []string{"pbcopy"}
	case os.Getenv("WAYLAND_DISPLAY") != "":
		argv = []string{"wl-copy"}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(s)
	// xclip and wl-copy fork; a pipe here would hang Run.
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clipboard: %w", err)
	}
	return nil
}
