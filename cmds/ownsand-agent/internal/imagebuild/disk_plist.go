// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// diskPlistDict reads just the plist value types used to identify attachments.
// Dictionaries and arrays retain values read before the end of a truncated
// document. A string is available only after its closing tag.
type diskPlistDict map[string]*diskPlistValue

type diskPlistValue struct {
	dict diskPlistDict
	list []*diskPlistValue
	text string
}

func (dict diskPlistDict) array(key string) []*diskPlistValue {
	if value := dict[key]; value != nil {
		return value.list
	}
	return nil
}

func (dict diskPlistDict) text(key string) string {
	if value := dict[key]; value != nil {
		return value.text
	}
	return ""
}

func readDiskImagePlist(out []byte) (diskPlistDict, error) {
	d := xml.NewDecoder(bytes.NewReader(out))
	root, err := nextPlistElement(d)
	if err != nil || root.Name.Local != "plist" {
		return nil, errors.New("expected a disk image plist")
	}
	root, err = nextPlistElement(d)
	if err != nil || root.Name.Local != "dict" {
		return nil, errors.New("expected a disk image plist dictionary")
	}
	var value diskPlistValue
	if err := value.decode(d, root); err != nil {
		return value.dict, err
	}
	if _, err := nextPlistElement(d); err != io.EOF {
		if err == nil {
			err = errors.New("unexpected element after the plist dictionary")
		}
		return value.dict, err
	}
	if _, err := nextPlistElement(d); err != io.EOF {
		return nil, errors.New("unexpected data after the plist")
	}
	return value.dict, nil
}

func (v *diskPlistValue) decode(d *xml.Decoder, start xml.StartElement) error {
	switch start.Name.Local {
	case "dict":
		v.dict = make(diskPlistDict)
		for {
			child, err := nextPlistElement(d)
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if child.Name.Local != "key" {
				return errors.New("expected a plist dictionary key")
			}
			var key string
			if err := d.DecodeElement(&key, &child); err != nil {
				return err
			}
			if _, exists := v.dict[key]; exists {
				return fmt.Errorf("duplicate plist dictionary key %q", key)
			}
			child, err = nextPlistElement(d)
			if err == io.EOF {
				return errors.New("missing plist dictionary value")
			}
			if err != nil {
				return err
			}
			value := new(diskPlistValue)
			v.dict[key] = value
			if err := value.decode(d, child); err != nil {
				return err
			}
		}
	case "array":
		for {
			child, err := nextPlistElement(d)
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			value := new(diskPlistValue)
			v.list = append(v.list, value)
			if err := value.decode(d, child); err != nil {
				return err
			}
		}
	case "string":
		return d.DecodeElement(&v.text, &start)
	default:
		return d.Skip()
	}
}

// nextPlistElement returns io.EOF at the end of the current element or document.
// The XML decoder returns a syntax error if the document ends inside an element.
func nextPlistElement(d *xml.Decoder) (xml.StartElement, error) {
	for {
		token, err := d.Token()
		if err != nil {
			return xml.StartElement{}, err
		}
		switch token := token.(type) {
		case xml.StartElement:
			return token, nil
		case xml.EndElement:
			return xml.StartElement{}, io.EOF
		case xml.CharData:
			if strings.TrimSpace(string(token)) != "" {
				return xml.StartElement{}, errors.New("unexpected text in a plist container")
			}
		}
	}
}
