// SPDX-License-Identifier: FSL-1.1-ALv2

package config

// Extra holds the configuration of the agent channel and data path layers.
type Extra struct{}

func loadExtra(Lookup) (Extra, error) { return Extra{}, nil }
