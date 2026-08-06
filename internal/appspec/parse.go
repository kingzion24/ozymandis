package appspec

import (
	"fmt"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Parse decodes a spec and refuses anything it does not recognise.
//
// Unknown keys are an error rather than being ignored, which is the same rule
// the JSON API applies with DisallowUnknownFields and for the same reason: a
// misspelled key is somebody believing they configured something they did not.
// Ignoring it means the deploy goes out with the old value and nothing
// anywhere says so — the failure is silent, and the person spends the next
// hour looking at the wrong thing.
//
// This is what makes the [secrets] rejection work as more than a special case.
// A file cannot smuggle credentials under any table name, because no
// unrecognised table is accepted at all; the named error simply says something
// more useful about the one people will actually try.
func Parse(data []byte) (Spec, error) {
	var spec Spec
	md, err := toml.Decode(string(data), &spec)
	if err != nil {
		return Spec{}, fmt.Errorf("appspec: %s is not valid TOML: %w", FileName, err)
	}

	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		sort.Strings(keys)

		// Checked against the whole set rather than only the first, so a file
		// with both a typo and a [secrets] table gets the message that matters.
		for _, k := range keys {
			if root := strings.SplitN(k, ".", 2)[0]; root == "secrets" || root == "secret" {
				return Spec{}, ErrSecretsInFile
			}
		}
		return Spec{}, fmt.Errorf(
			"appspec: %s sets things this does not recognise: %s\n"+
				"A misspelled key is a setting you believe you made and did not, "+
				"so it is refused rather than ignored.",
			FileName, strings.Join(keys, ", "))
	}

	if err := spec.Validate(); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

// Encode writes a spec back out as TOML.
//
// Used by `oz init` to produce a starting file, and by tests to prove a spec
// survives a round trip. Absent fields stay absent: a pointer that is nil is
// not written, so re-encoding a partial file does not turn its silences into
// explicit zeros.
func Encode(s Spec) ([]byte, error) {
	var b strings.Builder
	if err := toml.NewEncoder(&b).Encode(s); err != nil {
		return nil, fmt.Errorf("appspec: encode: %w", err)
	}
	return []byte(b.String()), nil
}
