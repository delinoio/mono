// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"google.golang.org/protobuf/proto"
)

func checkVirtualizationEntitlementCapabilities(caps *ownsandv1.DeviceCapabilities, wantEntitlement bool) error {
	checks := caps.GetHypervisorChecks()
	check := func(id string) *ownsandv1.HypervisorCheck {
		for _, row := range checks {
			if row.GetCheckId() == id {
				return row
			}
		}
		return nil
	}
	entitlement := check("virtualization-entitlement")
	if entitlement == nil {
		return fmt.Errorf("missing virtualization-entitlement check")
	}
	if entitlement.GetPassed() != wantEntitlement {
		return fmt.Errorf("virtualization-entitlement passed = %t, want %t", entitlement.GetPassed(), wantEntitlement)
	}
	if !check("linux-base-image").GetPassed() {
		return fmt.Errorf("linux-base-image check must pass")
	}
	framework := check("virtualization-framework")
	if framework == nil {
		return fmt.Errorf("missing virtualization-framework check")
	}
	guests, images := caps.GetGuestOses(), caps.GetBuiltBaseImages()
	if framework.GetPassed() && wantEntitlement {
		if !slices.Contains(guests, ownsandv1.GuestOs_GUEST_OS_LINUX) || !slices.Contains(images, ownsandv1.BaseImage_BASE_IMAGE_LINUX) {
			return fmt.Errorf("missing Linux guest or image support: guests = %v, images = %v", guests, images)
		}
	} else if len(guests) != 0 || len(images) != 0 || caps.GetMacosGuestsSupported() {
		return fmt.Errorf("unexpected support with framework available = %t and entitlement enabled = %t: guests = %v, images = %v, macOS guests = %t",
			framework.GetPassed(), wantEntitlement, guests, images, caps.GetMacosGuestsSupported())
	}
	return nil
}

func TestVirtualizationEntitlementCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name      string
		framework bool
		entitled  bool
	}{
		{name: "unavailable/disabled"},
		{name: "unavailable/enabled", entitled: true},
		{name: "available/disabled", framework: true},
		{name: "available/enabled", framework: true, entitled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			framework := &ownsandv1.HypervisorCheck{CheckId: "virtualization-framework", Passed: tc.framework}
			entitlement := &ownsandv1.HypervisorCheck{CheckId: "virtualization-entitlement", Passed: tc.entitled}
			image := &ownsandv1.HypervisorCheck{CheckId: "linux-base-image", Passed: true}
			caps := &ownsandv1.DeviceCapabilities{HypervisorChecks: []*ownsandv1.HypervisorCheck{framework, entitlement, image}}
			if tc.framework && tc.entitled {
				caps.GuestOses = []ownsandv1.GuestOs{ownsandv1.GuestOs_GUEST_OS_LINUX}
				caps.BuiltBaseImages = []ownsandv1.BaseImage{ownsandv1.BaseImage_BASE_IMAGE_LINUX}
			}
			if err := checkVirtualizationEntitlementCapabilities(caps, tc.entitled); err != nil {
				t.Fatal(err)
			}
			reject := func(name, want string, change func(*ownsandv1.DeviceCapabilities)) {
				t.Run(name, func(t *testing.T) {
					invalid := proto.CloneOf(caps)
					change(invalid)
					if err := checkVirtualizationEntitlementCapabilities(invalid, tc.entitled); err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("validation error = %v, want %q", err, want)
					}
				})
			}
			reject("missing entitlement", "missing virtualization-entitlement", func(caps *ownsandv1.DeviceCapabilities) {
				caps.HypervisorChecks = slices.Delete(caps.HypervisorChecks, 1, 2)
			})
			reject("wrong entitlement", "virtualization-entitlement passed", func(caps *ownsandv1.DeviceCapabilities) {
				caps.HypervisorChecks[1].Passed = !tc.entitled
			})
			reject("missing image check", "linux-base-image", func(caps *ownsandv1.DeviceCapabilities) {
				caps.HypervisorChecks = caps.HypervisorChecks[:2]
			})
			reject("failed image check", "linux-base-image", func(caps *ownsandv1.DeviceCapabilities) {
				caps.HypervisorChecks[2].Passed = false
			})
			if tc.framework && tc.entitled {
				reject("missing Linux guest", "missing Linux guest or image support", func(caps *ownsandv1.DeviceCapabilities) {
					caps.GuestOses = nil
				})
				reject("missing Linux image", "missing Linux guest or image support", func(caps *ownsandv1.DeviceCapabilities) {
					caps.BuiltBaseImages = nil
				})
				return
			}
			reject("unexpected guests", "unexpected support", func(caps *ownsandv1.DeviceCapabilities) {
				caps.GuestOses = []ownsandv1.GuestOs{ownsandv1.GuestOs_GUEST_OS_LINUX}
			})
			reject("unexpected images", "unexpected support", func(caps *ownsandv1.DeviceCapabilities) {
				caps.BuiltBaseImages = []ownsandv1.BaseImage{ownsandv1.BaseImage_BASE_IMAGE_LINUX}
			})
			reject("unexpected macOS support", "unexpected support", func(caps *ownsandv1.DeviceCapabilities) {
				caps.MacosGuestsSupported = true
			})
		})
	}
}
