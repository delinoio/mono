// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"encoding/json"
	"errors"
	"fmt"
)

var errMissingSystemList = errors.New("hcsrt: compute system list: missing result document")

// systemProperties is the part of the compute system properties that the
// runtime reads.
type systemProperties struct {
	ID        string `json:"Id"`
	Owner     string `json:"Owner"`
	RuntimeID string `json:"RuntimeId"`
	State     string `json:"State"`
	Stopped   bool   `json:"Stopped"`
}

func decodeSystemList(doc string) ([]systemProperties, error) {
	if doc == "" {
		return nil, errMissingSystemList
	}
	var list []systemProperties
	if err := json.Unmarshal([]byte(doc), &list); err != nil {
		return nil, fmt.Errorf("hcsrt: compute system list: %w", err)
	}
	if list == nil {
		return nil, errMissingSystemList
	}
	return list, nil
}
