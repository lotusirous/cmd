package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: urlencode [-d]")
	flag.PrintDefaults()
	os.Exit(2)
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("urlencode: ")
	d := flag.Bool("d", false, "decode instead of encode")
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() != 0 {
		usage()
	}

	f := encode
	if *d {
		f = decode
	}
	if err := lines(os.Stdin, os.Stdout, f); err != nil {
		log.Fatal(err)
	}
}

// encode escapes all but the RFC 3986 unreserved characters,
// as encodeURIComponent does.
func encode(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// decode undoes encode and reads "+" as a space, passing a malformed escape through as browsers do.
func decode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '+':
			c = ' '
		case c == '%' && i+2 < len(s):
			if n, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				c = byte(n)
				i += 2
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

func lines(r io.Reader, w io.Writer, f func(string) string) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, math.MaxInt) // a data: URL can run far past the 64KB default
	for sc.Scan() {
		if _, err := fmt.Fprintln(w, f(sc.Text())); err != nil {
			return err
		}
	}
	return sc.Err()
}
