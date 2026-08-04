package app

import (
	"errors"
	"fmt"
	"strings"
)

// maxCommandArgs caps how many arguments a command line may carry.
//
// Not a Kubernetes limit — the pod template has room for far more. It exists
// so that a paste of something that is not a command line at all fails at the
// form with a sentence, rather than as a Deployment whose spec is a page of
// prose.
const maxCommandArgs = 64

// ParseCommand splits a command line into the argv a container runs.
//
// Shell-style quoting rather than strings.Fields: a command line is something
// people copy out of a Dockerfile, a compose file, or a fly.toml, and those
// carry arguments with spaces inside them. Splitting
// `--log-config '{"version": 1}'` on whitespace produces two arguments that
// each mean nothing, and the container starts and immediately exits with a
// message about the second one.
//
// What is deliberately absent is everything else a shell does: no variable
// expansion, no globbing, no pipelines, no redirection, no `&&`. The container
// runs this argv directly with no shell between, so `$PORT` would arrive at the
// process as five literal characters and `|` as an ordinary argument.
// Implementing half a shell would make the other half look like a bug; not
// implementing any of it makes the boundary somewhere a person can see it. A
// command that genuinely needs a shell can ask for one: `sh -c "..."` is two
// arguments and a string, and parses here exactly as written.
func ParseCommand(line string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		started bool // distinguishes a deliberate empty argument from no argument
		quote   rune // 0, '\'' or '"'
		escaped bool
	)

	flush := func() {
		if started {
			args = append(args, cur.String())
			cur.Reset()
			started = false
		}
	}

	for _, r := range line {
		switch {
		case escaped:
			cur.WriteRune(r)
			started = true
			escaped = false

		// Inside single quotes a backslash is an ordinary character, which is
		// what makes single quotes the reliable way to pass one through.
		case r == '\\' && quote != '\'':
			escaped = true
			started = true

		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
			started = true

		case r == '\'' || r == '"':
			quote = r
			started = true

		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()

		default:
			cur.WriteRune(r)
			started = true
		}
	}

	switch {
	case escaped:
		return nil, errors.New("command ends in a backslash with nothing to escape")
	case quote == '\'':
		return nil, errors.New("command has an unclosed single quote")
	case quote == '"':
		return nil, errors.New("command has an unclosed double quote")
	}
	flush()

	if len(args) > maxCommandArgs {
		return nil, fmt.Errorf(
			"command has %d arguments, which is more than the %d allowed",
			len(args), maxCommandArgs)
	}

	// An argv whose first element is empty names no program. Kubernetes accepts
	// it and the kubelet then fails the container with a message about an
	// executable called "", which reads as a platform fault rather than as the
	// empty quotes somebody typed.
	if len(args) > 0 && args[0] == "" {
		return nil, errors.New("command starts with an empty argument")
	}
	return args, nil
}
