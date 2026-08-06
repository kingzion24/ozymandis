package appspec

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The load-bearing case for the whole package.
//
// `[scale] replicas = 0` and no [scale] at all are opposite instructions —
// "scale to nothing" and "say nothing about scaling" — and a struct of plain
// values reads both as 0. This is the exact shape of the defect that a
// confident assertion would have hidden: `if spec.Scale.Replicas != nil` passes
// whether or not the decoder distinguishes the cases, when the fixtures happen
// to be built wrong.
//
// So the three states are enumerated explicitly and each is pinned to a
// distinct expected shape, rather than checking one and trusting the rest.
func TestReplicasZeroPresentIsNotReplicasAbsent(t *testing.T) {
	cases := []struct {
		name string
		doc  string

		wantScaleNil    bool
		wantReplicasNil bool
		wantReplicas    int32
	}{
		{
			// The one that matters: an explicit scale-to-nothing.
			name:         "replicas = 0, explicitly",
			doc:          "name = \"web\"\n[scale]\nreplicas = 0\n",
			wantScaleNil: false, wantReplicasNil: false, wantReplicas: 0,
		},
		{
			// No opinion at all. Must not be read as scale-to-nothing.
			name:         "no [scale] table",
			doc:          "name = \"web\"\n",
			wantScaleNil: true,
		},
		{
			// A third state the probe turned up: the table exists, the key does
			// not. Also "no opinion", and distinguishable from both the others.
			name:         "[scale] present, replicas absent",
			doc:          "name = \"web\"\n[scale]\n",
			wantScaleNil: false, wantReplicasNil: true,
		},
		{
			name:         "a real count",
			doc:          "name = \"web\"\n[scale]\nreplicas = 3\n",
			wantScaleNil: false, wantReplicasNil: false, wantReplicas: 3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := Parse([]byte(tc.doc))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			if (spec.Scale == nil) != tc.wantScaleNil {
				t.Fatalf("Scale nil = %v, want %v", spec.Scale == nil, tc.wantScaleNil)
			}
			if tc.wantScaleNil {
				return
			}
			if (spec.Scale.Replicas == nil) != tc.wantReplicasNil {
				t.Fatalf("Replicas nil = %v, want %v",
					spec.Scale.Replicas == nil, tc.wantReplicasNil)
			}
			if tc.wantReplicasNil {
				return
			}
			if *spec.Scale.Replicas != tc.wantReplicas {
				t.Errorf("Replicas = %d, want %d", *spec.Scale.Replicas, tc.wantReplicas)
			}
		})
	}
}

// The distinction has to survive the wire, which is the entire reason this
// package uses pointers rather than toml.MetaData.IsDefined. IsDefined would
// pass the test above and fail this one, because it describes a decode and
// does not travel.
func TestAbsentVersusZeroSurvivesJSON(t *testing.T) {
	zero, err := Parse([]byte("name = \"web\"\n[scale]\nreplicas = 0\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	absent, err := Parse([]byte("name = \"web\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	zeroJSON, err := json.Marshal(zero)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	absentJSON, err := json.Marshal(absent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if string(zeroJSON) == string(absentJSON) {
		t.Fatalf("the two encode identically (%s) — the server cannot tell "+
			"scale-to-nothing from say-nothing", zeroJSON)
	}
	if !strings.Contains(string(zeroJSON), `"replicas":0`) {
		t.Errorf("explicit zero did not survive marshalling: %s", zeroJSON)
	}
	if strings.Contains(string(absentJSON), "replicas") {
		t.Errorf("absent became present on the wire: %s", absentJSON)
	}

	// And back again, because the server decodes what the CLI sent.
	var back Spec
	if err := json.Unmarshal(zeroJSON, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Scale == nil || back.Scale.Replicas == nil || *back.Scale.Replicas != 0 {
		t.Errorf("explicit zero did not survive the round trip: %+v", back.Scale)
	}

	var backAbsent Spec
	if err := json.Unmarshal(absentJSON, &backAbsent); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if backAbsent.Scale != nil {
		t.Errorf("absent became present after the round trip: %+v", backAbsent.Scale)
	}
}

// The same distinction for every other optional scalar, not just replicas.
// Each of these has a zero value somebody could legitimately mean.
func TestEveryOptionalScalarDistinguishesAbsentFromZero(t *testing.T) {
	cases := []struct {
		name    string
		zeroDoc string
		present func(Spec) bool
		isZero  func(Spec) bool
	}{
		{
			// "" clears a release command. Absent leaves it alone. These must
			// not be the same request.
			name:    "release_command empty",
			zeroDoc: "name = \"web\"\n[deploy]\nrelease_command = \"\"\n",
			present: func(s Spec) bool { return s.Deploy != nil && s.Deploy.ReleaseCommand != nil },
			isZero:  func(s Spec) bool { return *s.Deploy.ReleaseCommand == "" },
		},
		{
			name:    "internal false",
			zeroDoc: "name = \"web\"\n[service]\ninternal = false\n",
			present: func(s Spec) bool { return s.Service != nil && s.Service.Internal != nil },
			isZero:  func(s Spec) bool { return !*s.Service.Internal },
		},
		{
			name:    "liveness false",
			zeroDoc: "name = \"web\"\n[health]\nliveness = false\n",
			present: func(s Spec) bool { return s.Health != nil && s.Health.Liveness != nil },
			isZero:  func(s Spec) bool { return !*s.Health.Liveness },
		},
		{
			// "" removes the probe. Absent leaves whatever is configured.
			name:    "health path empty",
			zeroDoc: "name = \"web\"\n[health]\npath = \"\"\n",
			present: func(s Spec) bool { return s.Health != nil && s.Health.Path != nil },
			isZero:  func(s Spec) bool { return *s.Health.Path == "" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withZero, err := Parse([]byte(tc.zeroDoc))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !tc.present(withZero) {
				t.Fatal("an explicitly-set zero value reads as absent")
			}
			if !tc.isZero(withZero) {
				t.Error("the value did not survive as its zero")
			}

			bare, err := Parse([]byte("name = \"web\"\n"))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if tc.present(bare) {
				t.Error("an absent value reads as present")
			}
		})
	}
}

// [env] distinguishes absent from empty the way the pointers do, because an
// empty table is a real instruction: remove every plaintext variable. A nil map
// and an empty map are different, and this pins that the decoder preserves it.
func TestEnvDistinguishesAbsentFromEmpty(t *testing.T) {
	absent, err := Parse([]byte("name = \"web\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if absent.Env != nil {
		t.Errorf("no [env] table gave a non-nil map: %#v", absent.Env)
	}

	empty, err := Parse([]byte("name = \"web\"\n[env]\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if empty.Env == nil {
		t.Fatal("an empty [env] table gave a nil map — " +
			"\"remove them all\" is indistinguishable from \"say nothing\"")
	}
	if len(empty.Env) != 0 {
		t.Errorf("empty [env] = %#v", empty.Env)
	}
}

// The structural guard the package doc promises.
//
// Mechanical rather than by review, because it only has to be forgotten once: a
// plain `Replicas int32` added to Scale next year would compile, pass every
// other test, and silently reintroduce exactly the collapse this package exists
// to prevent. Reflection over the type is the only check that catches a field
// nobody wrote a test for.
func TestEveryOptionalFieldCanBeAbsent(t *testing.T) {
	// Fields that are required rather than optional, and so are allowed to be
	// plain values. Each one must be named here deliberately.
	required := map[string]bool{
		"Spec.Name":   true,
		"Volume.Name": true,
		"Volume.Path": true,
		"Domain.Host": true,
	}

	var check func(t *testing.T, typ reflect.Type, seen map[reflect.Type]bool)
	check = func(t *testing.T, typ reflect.Type, seen map[reflect.Type]bool) {
		if seen[typ] {
			return
		}
		seen[typ] = true

		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			qualified := typ.Name() + "." + f.Name

			switch f.Type.Kind() {
			case reflect.Ptr, reflect.Slice, reflect.Map:
				// Fine: all three carry a distinct absent value.
			default:
				if !required[qualified] {
					t.Errorf("%s is a plain %s. Every optional field in this "+
						"package must be a pointer, slice, or map so that absent "+
						"and zero stay distinguishable — see the package doc. If "+
						"it is genuinely required, add it to the `required` set "+
						"in this test.", qualified, f.Type.Kind())
				}
			}

			// Recurse into the section structs.
			ft := f.Type
			for ft.Kind() == reflect.Ptr || ft.Kind() == reflect.Slice {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				check(t, ft, seen)
			}
		}
	}

	check(t, reflect.TypeOf(Spec{}), map[reflect.Type]bool{})
}
