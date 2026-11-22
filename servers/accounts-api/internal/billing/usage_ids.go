// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"strings"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
)

// Stable identifiers of services and meters
// (docs/protos-delino-accounts-api-contract.md, Enums). The database, the
// ledger and the configuration store them; they never change.

// ServiceKey returns the stable identifier of a service (its enum name
// without the prefix, in lowercase), or false for UNSPECIFIED and unknown
// values.
func ServiceKey(s accountsv1.ServiceId) (string, bool) {
	if s == accountsv1.ServiceId_SERVICE_ID_UNSPECIFIED {
		return "", false
	}
	name, ok := accountsv1.ServiceId_name[int32(s)]
	if !ok {
		return "", false
	}
	return strings.ToLower(strings.TrimPrefix(name, "SERVICE_ID_")), true
}

// ServiceFromKey returns the service of a stable identifier.
func ServiceFromKey(key string) (accountsv1.ServiceId, bool) {
	// Reject anything outside a-z, 0-9 and "_" before the lookup:
	// strings.ToUpper is Unicode-aware and folds characters such as U+017F
	// (long s) to ASCII letters. config.serviceFromKey applies the same
	// rule; the packages keep separate copies because config cannot import
	// billing.
	if !stableKey(key) {
		return accountsv1.ServiceId_SERVICE_ID_UNSPECIFIED, false
	}
	v, ok := accountsv1.ServiceId_value["SERVICE_ID_"+strings.ToUpper(key)]
	if !ok || v == 0 {
		return accountsv1.ServiceId_SERVICE_ID_UNSPECIFIED, false
	}
	return accountsv1.ServiceId(v), true
}

// MeterKey returns the stable identifier of a meter (`<service>.<meter>` in
// lowercase) and its service, or false for UNSPECIFIED and unknown values.
func MeterKey(m accountsv1.Meter) (key string, service accountsv1.ServiceId, ok bool) {
	if m == accountsv1.Meter_METER_UNSPECIFIED {
		return "", 0, false
	}
	name, ok := accountsv1.Meter_name[int32(m)]
	if !ok {
		return "", 0, false
	}
	rest := strings.TrimPrefix(name, "METER_")
	// Each Meter value starts with the name of its ServiceId value. The
	// longest matching service name wins, so a service name that is a prefix
	// of another one cannot capture its meters.
	best := ""
	for v, serviceName := range accountsv1.ServiceId_name {
		if v == 0 {
			continue
		}
		prefix := strings.TrimPrefix(serviceName, "SERVICE_ID_") + "_"
		if strings.HasPrefix(rest, prefix) && len(rest) > len(prefix) && len(prefix) > len(best) {
			best, service = prefix, accountsv1.ServiceId(v)
		}
	}
	if best == "" {
		return "", 0, false
	}
	svc := strings.ToLower(strings.TrimSuffix(best, "_"))
	return svc + "." + strings.ToLower(strings.TrimPrefix(rest, best)), service, true
}

// stableKey reports whether key is non-empty and holds only the bytes a-z,
// 0-9 and "_" that stable identifiers use.
func stableKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// MeterFromKey returns the meter of a stable identifier.
func MeterFromKey(key string) (accountsv1.Meter, bool) {
	service, meter, ok := strings.Cut(key, ".")
	// Exactly one ".", and both parts hold only a-z, 0-9 and "_": the
	// Unicode-aware strings.ToUpper below would otherwise fold characters
	// such as U+017F (long s) to ASCII letters.
	if !ok || strings.Contains(meter, ".") || !stableKey(service) || !stableKey(meter) {
		return accountsv1.Meter_METER_UNSPECIFIED, false
	}
	v, ok := accountsv1.Meter_value["METER_"+strings.ToUpper(service)+"_"+strings.ToUpper(meter)]
	if !ok || v == 0 {
		return accountsv1.Meter_METER_UNSPECIFIED, false
	}
	return accountsv1.Meter(v), true
}

// ServiceMeters returns the meters of a service in Meter order.
func ServiceMeters(s accountsv1.ServiceId) []accountsv1.Meter {
	var out []accountsv1.Meter
	for v := int32(1); ; v++ {
		if _, ok := accountsv1.Meter_name[v]; !ok {
			// Meter values are dense from 1; the first gap ends the list.
			return out
		}
		if _, svc, ok := MeterKey(accountsv1.Meter(v)); ok && svc == s {
			out = append(out, accountsv1.Meter(v))
		}
	}
}

// ServiceDisplayName is the product name of a service in emails.
func ServiceDisplayName(service string) string {
	switch service {
	case "ownsand":
		return "OwnSand"
	default:
		return service
	}
}
