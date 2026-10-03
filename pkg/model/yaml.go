package model

import (
	"fmt"

	"gopkg.in/yaml.v2"
)

// DecodeKeysYAML decodes a YAML list of keys, such as a hand-written seed
// file, and validates each one.
func DecodeKeysYAML(b []byte) ([]Key, error) {
	var keys []Key
	if err := yaml.Unmarshal(b, &keys); err != nil {
		return nil, fmt.Errorf("model: decode keys: %w", err)
	}
	for _, k := range keys {
		if _, _, _, err := k.Parse(); err != nil {
			return nil, fmt.Errorf("model: decode keys: %w", err)
		}
	}
	return keys, nil
}
