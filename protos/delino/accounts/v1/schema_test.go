// SPDX-License-Identifier: Apache-2.0

package accountsv1_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	// Registers the descriptors of the package.
	_ "github.com/delinoio/mono/protos/delino/accounts/v1"
)

const packageName = "delino.accounts.v1"

// Every enum of the package has a zero value named <ENUM>_UNSPECIFIED.
func TestEnumsHaveUnspecifiedZeroValue(t *testing.T) {
	count := 0
	protoregistry.GlobalTypes.RangeEnums(func(enum protoreflect.EnumType) bool {
		descriptor := enum.Descriptor()
		if descriptor.ParentFile().Package() != packageName {
			return true
		}
		count++
		zero := descriptor.Values().ByNumber(0)
		if zero == nil || !strings.HasSuffix(string(zero.Name()), "_UNSPECIFIED") {
			t.Errorf("enum %s has no _UNSPECIFIED zero value", descriptor.FullName())
		}
		return true
	})
	if count == 0 {
		t.Fatal("no enums registered for the package")
	}
}

// Money fields follow the billing contract: a signed 64-bit micro-USD value
// whose name ends with `_usd_micros`. A field whose name mentions `usd` in any
// other form is a mistake.
func TestMoneyFieldsAreInt64UsdMicros(t *testing.T) {
	count := 0
	protoregistry.GlobalTypes.RangeMessages(func(message protoreflect.MessageType) bool {
		descriptor := message.Descriptor()
		if descriptor.ParentFile().Package() != packageName {
			return true
		}
		count++
		fields := descriptor.Fields()
		for i := 0; i < fields.Len(); i++ {
			field := fields.Get(i)
			name := string(field.Name())
			if !strings.Contains(name, "usd") {
				continue
			}
			if !strings.HasSuffix(name, "_usd_micros") {
				t.Errorf("field %s mentions usd but does not end with _usd_micros", field.FullName())
			}
			if field.Kind() != protoreflect.Int64Kind || field.IsList() || field.IsMap() {
				t.Errorf("money field %s is not a singular int64", field.FullName())
			}
		}
		return true
	})
	if count == 0 {
		t.Fatal("no messages registered for the package")
	}
}
