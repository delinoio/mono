// SPDX-License-Identifier: FSL-1.1-ALv2

package api

// DefinedEnum checks an enum value against its generated name map.
func DefinedEnum[T ~int32](field, enum string, value T, names map[int32]string) error {
	if _, ok := names[int32(value)]; !ok {
		return ErrInvalidf("invalid %s: must be a defined %s value", field, enum)
	}
	return nil
}
