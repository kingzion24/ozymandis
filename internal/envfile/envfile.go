// Package envfile reads the KEY=value block that a .env file is.
//
// It exists so that pasting a file into the dashboard and piping the same file
// into `oz secrets set --stdin` cannot disagree. Two parsers would drift, and
// the way they would drift is silent: a line one accepts and the other skips
// leaves an app missing a variable that the person who set it watched go in.
package envfile

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Parse reads KEY=value lines and returns them in the order-independent form
// the variable service wants.
//
// Blank lines and # comments are skipped. Each line is trimmed before it is
// split, which is what makes a paste from a browser work: a textarea submits
// CRLF per the HTML spec, and the CR would otherwise become the last character
// of every value.
//
// Values are trimmed but not unquoted. Trimming, because the surrounding
// whitespace of a pasted credential is never part of the credential and is
// invisible in the box you pasted it into — a stray trailing space is the same
// class of defect as the trailing newline that once made a stored CI secret
// fail to authenticate, and it is far easier to prevent here than to diagnose
// from a 403. Not unquoted, because a .env is not a shell: "abc" is a
// three-character value in some readers and a five-character one in others,
// and guessing which somebody meant is worse than passing through what they
// wrote.
func Parse(r io.Reader) (map[string]string, error) {
	out := map[string]string{}

	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// `export KEY=value` is how a file meant to be sourced is written, and
		// somebody pasting one has not made a mistake worth an error.
		line = strings.TrimPrefix(line, "export ")

		k, v, ok := strings.Cut(line, "=")
		if !ok {
			// Numbered, because the whole point of pasting a block is not
			// reading it line by line, and "line 7" is the difference between
			// a fix and a hunt.
			return nil, fmt.Errorf("line %d is not KEY=value: %q", n, line)
		}
		k = strings.TrimSpace(k)
		if k == "" {
			return nil, fmt.Errorf("line %d has no key before the =", n)
		}
		out[k] = strings.TrimSpace(v)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("envfile: read: %w", err)
	}
	return out, nil
}
