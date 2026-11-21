// SPDX-License-Identifier: Apache-2.0

package accountsv1_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
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

// The stable identifier of a meter is `<service>.<meter>` in lowercase.
// LedgerEntry.meter and accounts-api storage use these strings, so a renamed
// or removed Meter value fails here.
func TestMeterStableIdentifiers(t *testing.T) {
	services := map[string]bool{}
	serviceValues := accountsv1.ServiceId(0).Descriptor().Values()
	for i := 0; i < serviceValues.Len(); i++ {
		value := serviceValues.Get(i)
		if value.Number() == 0 {
			continue
		}
		services[strings.TrimPrefix(string(value.Name()), "SERVICE_ID_")] = true
	}
	got := map[string]bool{}
	meterValues := accountsv1.Meter(0).Descriptor().Values()
	for i := 0; i < meterValues.Len(); i++ {
		value := meterValues.Get(i)
		if value.Number() == 0 {
			continue
		}
		rest := strings.TrimPrefix(string(value.Name()), "METER_")
		id := ""
		for service := range services {
			if meter, ok := strings.CutPrefix(rest, service+"_"); ok && meter != "" {
				id = strings.ToLower(service) + "." + strings.ToLower(meter)
			}
		}
		if id == "" {
			t.Errorf("Meter %s does not start with a ServiceId name", value.Name())
			continue
		}
		got[id] = true
	}
	want := []string{
		"ownsand.relay_egress", "ownsand.r2_storage",
		"ownsand.class_a_rpc", "ownsand.class_b_rpc",
	}
	if len(got) != len(want) {
		t.Errorf("Meter has %d stable identifiers, want %d: %v", len(got), len(want), got)
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("no Meter value has the stable identifier %q", id)
		}
	}
}
