// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"fmt"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// wantClasses is the classification table of
// docs/protos-ownsand-api-contract.md#billing, by service and method name.
// It is written out independently of rpcClasses so that a change to either
// fails the test.
var wantClasses = map[string]map[string]RPCClass{
	"OrganizationService": {
		"CreateOrganization": RPCClassNotBilled, "GetOrganization": RPCClassB, "ListOrganizations": RPCClassNotBilled,
		"DeleteOrganization": RPCClassA, "TransferOwnership": RPCClassA,
	},
	"MemberService": {
		"CreateInvitation": RPCClassA, "ListInvitations": RPCClassB, "RevokeInvitation": RPCClassA, "AcceptInvitation": RPCClassNotBilled,
		"ListMembers": RPCClassB, "UpdateMemberRole": RPCClassA, "RemoveMember": RPCClassA,
	},
	"DeviceService": {
		"CreateEnrollmentToken": RPCClassA, "ListEnrollmentTokens": RPCClassB, "RevokeEnrollmentToken": RPCClassA,
		"ListDevices": RPCClassB, "GetDevice": RPCClassB, "UpdateDeviceLimits": RPCClassA, "ReplaceDeviceLimits": RPCClassA,
		"RemoveDevice": RPCClassA, "GetAgentVersionPolicy": RPCClassB, "UpdateAgentVersionPolicy": RPCClassA,
	},
	"ApiKeyService": {"CreateApiKey": RPCClassA, "ListApiKeys": RPCClassB, "RevokeApiKey": RPCClassA},
	"SandboxService": {
		"CreateSandbox": RPCClassA, "GetSandbox": RPCClassB, "ListSandboxes": RPCClassB, "DeleteSandbox": RPCClassA,
		"PauseSandbox": RPCClassA, "ResumeSandbox": RPCClassA,
	},
	"SnapshotService": {
		"CreateSnapshot": RPCClassA, "GetSnapshot": RPCClassB, "ListSnapshots": RPCClassB, "DeleteSnapshot": RPCClassA,
		"ExportSnapshot": RPCClassA, "CreateTemplate": RPCClassA, "ListTemplates": RPCClassB, "DeleteTemplate": RPCClassA,
	},
	"DataPathService": {"GetIceServers": RPCClassB, "OpenSession": RPCClassA, "AuthorizeRelay": RPCClassA},
	"RelayService":    {"Exec": RPCClassA, "AttachExec": RPCClassA, "ReadFile": RPCClassA, "WriteFile": RPCClassA},
	"AgentService": {
		"Enroll": RPCClassA, "CreateChallenge": RPCClassNotBilled, "Authenticate": RPCClassNotBilled,
		"ConfirmRemoval": RPCClassNotBilled, "Channel": RPCClassNotBilled,
	},
	"RelayAgentService": {"Connect": RPCClassNotBilled},
	"GuestService":      {"CheckReady": RPCClassNotBilled, "SetClock": RPCClassNotBilled, "Serve": RPCClassNotBilled},
	"BillingService": {
		"GetBillingStatus": RPCClassNotBilled, "GetBillingUsage": RPCClassNotBilled, "ListLinkableBillingAccounts": RPCClassNotBilled,
		"LinkBillingAccount": RPCClassNotBilled, "UnlinkBillingAccount": RPCClassNotBilled,
	},
}

// generatedProcedures walks every service descriptor of the generated
// delino.ownsand.v1 package (the services that ownsandv1connect serves) and
// returns each procedure with its service and method name.
func generatedProcedures(t *testing.T) map[string][2]string {
	t.Helper()
	// Referencing the package registers its files.
	_ = ownsandv1.File_delino_ownsand_v1_billing_proto
	out := map[string][2]string{}
	protoregistry.GlobalFiles.RangeFilesByPackage("delino.ownsand.v1", func(fd protoreflect.FileDescriptor) bool {
		services := fd.Services()
		for i := range services.Len() {
			sd := services.Get(i)
			methods := sd.Methods()
			for j := range methods.Len() {
				md := methods.Get(j)
				out[fmt.Sprintf("/%s/%s", sd.FullName(), md.Name())] = [2]string{string(sd.Name()), string(md.Name())}
			}
		}
		return true
	})
	if len(out) == 0 {
		t.Fatal("no generated OwnSand services found")
	}
	return out
}

// TestClassifyEveryGeneratedProcedure fails when a generated RPC is not
// classified, or is classified differently from the contract table.
func TestClassifyEveryGeneratedProcedure(t *testing.T) {
	procedures := generatedProcedures(t)
	for procedure, names := range procedures {
		t.Run(procedure, func(t *testing.T) {
			got := ClassifyRPC(procedure)
			if got == RPCClassUnclassified {
				t.Fatalf("%s is not classified; add it to rpcClasses and the protos contract billing table", procedure)
			}
			want, ok := wantClasses[names[0]][names[1]]
			if !ok {
				t.Fatalf("%s is missing from the expected contract table of this test", procedure)
			}
			if got != want {
				t.Fatalf("%s is %v, the contract says %v", procedure, got, want)
			}
		})
	}
	// No stale entries: every classified procedure and every expected entry
	// exists in the generated code.
	for procedure := range rpcClasses {
		if _, ok := procedures[procedure]; !ok {
			t.Errorf("rpcClasses lists %s, which no generated service has", procedure)
		}
	}
	n := 0
	for _, methods := range wantClasses {
		n += len(methods)
	}
	if n != len(procedures) {
		t.Errorf("the expected table has %d procedures, the generated services %d", n, len(procedures))
	}
}

func TestRPCClassMeter(t *testing.T) {
	if m, ok := RPCClassA.Meter(); !ok || m != classA {
		t.Errorf("class A meter = %v %v", m, ok)
	}
	if m, ok := RPCClassB.Meter(); !ok || m != classB {
		t.Errorf("class B meter = %v %v", m, ok)
	}
	for _, k := range []RPCClass{RPCClassNotBilled, RPCClassUnclassified} {
		if _, ok := k.Meter(); ok {
			t.Errorf("%v has a meter", k)
		}
	}
	if ClassifyRPC("/delino.ownsand.v1.SandboxService/Unknown") != RPCClassUnclassified {
		t.Error("an unknown procedure is classified")
	}
}
