// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestDecodeSystemList(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		want []systemProperties
		err  error
	}{
		{name: "missing", err: errMissingSystemList},
		{name: "null", doc: "null", err: errMissingSystemList},
		{name: "empty array", doc: "[]", want: []systemProperties{}},
		{name: "populated", doc: `[{"Id":"fixture-system"}]`, want: []systemProperties{{ID: "fixture-system"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list, err := decodeSystemList(tc.doc)
			if !errors.Is(err, tc.err) || !reflect.DeepEqual(list, tc.want) {
				t.Fatalf("decodeSystemList(%q) = %#v, %v, want %#v, %v", tc.doc, list, err, tc.want, tc.err)
			}
		})
	}

	var typeErr *json.UnmarshalTypeError
	if list, err := decodeSystemList(`{}`); list != nil || !errors.As(err, &typeErr) {
		t.Fatalf("decodeSystemList({}) = %#v, %v, want a JSON type error", list, err)
	}
}
