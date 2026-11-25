// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package vzrt

import "testing"

func TestMacOSBaseImageGenerationWithNativeCopies(t *testing.T) {
	testMacOSBaseImageGeneration(t, copyFile)
}
