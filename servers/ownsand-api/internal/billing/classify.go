// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	c "github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// RPCClass is the billing class of an RPC.
type RPCClass int

const (
	// RPCClassUnclassified is an RPC that the classification does not list.
	// It is never billed; the classification test fails for it.
	RPCClassUnclassified RPCClass = iota
	// RPCClassNotBilled is an RPC that is never billed.
	RPCClassNotBilled
	// RPCClassA is a Class A RPC ($4.50 per million calls).
	RPCClassA
	// RPCClassB is a Class B RPC ($0.36 per million calls).
	RPCClassB
)

// String returns the stable name of the class.
func (k RPCClass) String() string {
	switch k {
	case RPCClassNotBilled:
		return "not_billed"
	case RPCClassA:
		return "class_a"
	case RPCClassB:
		return "class_b"
	}
	return "unclassified"
}

// Meter returns the meter of a billed class.
func (k RPCClass) Meter() (ownsandv1.BillingMeter, bool) {
	switch k {
	case RPCClassA:
		return ownsandv1.BillingMeter_BILLING_METER_CLASS_A_RPC, true
	case RPCClassB:
		return ownsandv1.BillingMeter_BILLING_METER_CLASS_B_RPC, true
	}
	return ownsandv1.BillingMeter_BILLING_METER_UNSPECIFIED, false
}

// rpcClasses is the classification table of the protos contract
// (docs/protos-ownsand-api-contract.md#billing). Every procedure of every
// generated OwnSand service is listed; a new RPC is added in the change that
// adds it.
var rpcClasses = map[string]RPCClass{
	// OrganizationService. CreateOrganization and ListOrganizations have no
	// organization as their billing subject.
	c.OrganizationServiceCreateOrganizationProcedure: RPCClassNotBilled,
	c.OrganizationServiceGetOrganizationProcedure:    RPCClassB,
	c.OrganizationServiceListOrganizationsProcedure:  RPCClassNotBilled,
	c.OrganizationServiceDeleteOrganizationProcedure: RPCClassA,
	c.OrganizationServiceTransferOwnershipProcedure:  RPCClassA,

	// MemberService. AcceptInvitation has no organization in the request.
	c.MemberServiceCreateInvitationProcedure: RPCClassA,
	c.MemberServiceListInvitationsProcedure:  RPCClassB,
	c.MemberServiceRevokeInvitationProcedure: RPCClassA,
	c.MemberServiceAcceptInvitationProcedure: RPCClassNotBilled,
	c.MemberServiceListMembersProcedure:      RPCClassB,
	c.MemberServiceUpdateMemberRoleProcedure: RPCClassA,
	c.MemberServiceRemoveMemberProcedure:     RPCClassA,

	// DeviceService.
	c.DeviceServiceCreateEnrollmentTokenProcedure:    RPCClassA,
	c.DeviceServiceListEnrollmentTokensProcedure:     RPCClassB,
	c.DeviceServiceRevokeEnrollmentTokenProcedure:    RPCClassA,
	c.DeviceServiceListDevicesProcedure:              RPCClassB,
	c.DeviceServiceGetDeviceProcedure:                RPCClassB,
	c.DeviceServiceUpdateDeviceLimitsProcedure:       RPCClassA,
	c.DeviceServiceReplaceDeviceLimitsProcedure:      RPCClassA,
	c.DeviceServiceRemoveDeviceProcedure:             RPCClassA,
	c.DeviceServiceGetAgentVersionPolicyProcedure:    RPCClassB,
	c.DeviceServiceUpdateAgentVersionPolicyProcedure: RPCClassA,

	// ApiKeyService.
	c.ApiKeyServiceCreateApiKeyProcedure: RPCClassA,
	c.ApiKeyServiceListApiKeysProcedure:  RPCClassB,
	c.ApiKeyServiceRevokeApiKeyProcedure: RPCClassA,

	// SandboxService.
	c.SandboxServiceCreateSandboxProcedure: RPCClassA,
	c.SandboxServiceGetSandboxProcedure:    RPCClassB,
	c.SandboxServiceListSandboxesProcedure: RPCClassB,
	c.SandboxServiceDeleteSandboxProcedure: RPCClassA,
	c.SandboxServicePauseSandboxProcedure:  RPCClassA,
	c.SandboxServiceResumeSandboxProcedure: RPCClassA,

	// SnapshotService.
	c.SnapshotServiceCreateSnapshotProcedure: RPCClassA,
	c.SnapshotServiceGetSnapshotProcedure:    RPCClassB,
	c.SnapshotServiceListSnapshotsProcedure:  RPCClassB,
	c.SnapshotServiceDeleteSnapshotProcedure: RPCClassA,
	c.SnapshotServiceExportSnapshotProcedure: RPCClassA,
	c.SnapshotServiceCreateTemplateProcedure: RPCClassA,
	c.SnapshotServiceListTemplatesProcedure:  RPCClassB,
	c.SnapshotServiceDeleteTemplateProcedure: RPCClassA,

	// DataPathService.
	c.DataPathServiceGetIceServersProcedure:  RPCClassB,
	c.DataPathServiceOpenSessionProcedure:    RPCClassA,
	c.DataPathServiceAuthorizeRelayProcedure: RPCClassA,

	// RelayService: each call, also each WriteFile chunk and each stream
	// continuation.
	c.RelayServiceExecProcedure:       RPCClassA,
	c.RelayServiceAttachExecProcedure: RPCClassA,
	c.RelayServiceReadFileProcedure:   RPCClassA,
	c.RelayServiceWriteFileProcedure:  RPCClassA,

	// AgentService: only Enroll, authenticated by its enrollment token and
	// billed to the token's organization. The agent channel is not billed.
	c.AgentServiceEnrollProcedure:          RPCClassA,
	c.AgentServiceCreateChallengeProcedure: RPCClassNotBilled,
	c.AgentServiceAuthenticateProcedure:    RPCClassNotBilled,
	c.AgentServiceConfirmRemovalProcedure:  RPCClassNotBilled,
	c.AgentServiceChannelProcedure:         RPCClassNotBilled,

	// RelayAgentService.
	c.RelayAgentServiceConnectProcedure: RPCClassNotBilled,

	// GuestService never reaches the control plane.
	c.GuestServiceCheckReadyProcedure: RPCClassNotBilled,
	c.GuestServiceSetClockProcedure:   RPCClassNotBilled,
	c.GuestServiceServeProcedure:      RPCClassNotBilled,

	// BillingService is never billed, so an organization can always
	// recover.
	c.BillingServiceGetBillingStatusProcedure:            RPCClassNotBilled,
	c.BillingServiceGetBillingUsageProcedure:             RPCClassNotBilled,
	c.BillingServiceListLinkableBillingAccountsProcedure: RPCClassNotBilled,
	c.BillingServiceLinkBillingAccountProcedure:          RPCClassNotBilled,
	c.BillingServiceUnlinkBillingAccountProcedure:        RPCClassNotBilled,
}

// ClassifyRPC returns the billing class of a Connect procedure
// (`/delino.ownsand.v1.SandboxService/CreateSandbox`), or
// RPCClassUnclassified for a procedure that the table does not list.
func ClassifyRPC(procedure string) RPCClass {
	return rpcClasses[procedure]
}
