// SPDX-License-Identifier: FSL-1.1-ALv2

// Package ownsandfake is an in-memory fake of the OwnSand API for tide-api
// tests. It implements ownsandclient.Factory and Client: API keys that
// belong to fake organizations, devices, templates and sandboxes whose
// states the test moves. It is not a model of the OwnSand scheduler; tests
// script the outcome of CreateSandbox.
package ownsandfake

import (
	"context"
	"errors"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient"
)

// Fake is the fake OwnSand API.
type Fake struct {
	mu   sync.Mutex
	orgs map[string]*Org
	// calls counts calls by RPC name.
	calls map[string]int
}

// Org is one fake OwnSand organization.
type Org struct {
	ID     string
	APIKey string
	// Devices are returned by ListDevices.
	Devices []*ownsandv1.Device
	// Templates maps a template ID to its guest OS.
	Templates map[string]ownsandv1.GuestOs
}

// New returns an empty fake.
func New() *Fake {
	return &Fake{orgs: map[string]*Org{}, calls: map[string]int{}}
}

// AddOrg adds an organization with one API key.
func (f *Fake) AddOrg(id, apiKey string) *Org {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := &Org{ID: id, APIKey: apiKey, Templates: map[string]ownsandv1.GuestOs{}}
	f.orgs[id] = o
	return o
}

// RevokeKey makes the organization's key invalid (unauthenticated).
func (f *Fake) RevokeKey(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orgs[id].APIKey = ""
}

// SetTemplate records a template of an organization.
func (f *Fake) SetTemplate(orgID, templateID string, guest ownsandv1.GuestOs) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orgs[orgID].Templates[templateID] = guest
}

// SetDevices replaces the devices of an organization.
func (f *Fake) SetDevices(orgID string, devices ...*ownsandv1.Device) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orgs[orgID].Devices = devices
}

// Calls returns the number of calls of an RPC.
func (f *Fake) Calls(rpc string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[rpc]
}

// New implements ownsandclient.Factory.
func (f *Fake) New(ownsandOrganizationID, apiKey string) (ownsandclient.Client, error) {
	return &client{f: f, org: ownsandOrganizationID, key: apiKey}, nil
}

type client struct {
	f   *Fake
	org string
	key string
}

// begin checks the key like ownsand-api: an unknown key is unauthenticated,
// and a key of another organization gets not_found for this organization.
// It returns the organization with the fake lock held.
func (c *client) begin(rpc string) (*Org, error) {
	c.f.mu.Lock()
	c.f.calls[rpc]++
	for _, o := range c.f.orgs {
		if o.APIKey != "" && o.APIKey == c.key {
			if o.ID != c.org {
				c.f.mu.Unlock()
				return nil, connect.NewError(connect.CodeNotFound, errors.New("organization not found"))
			}
			return o, nil
		}
	}
	c.f.mu.Unlock()
	return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid API key"))
}

func (c *client) ListDevices(ctx context.Context) ([]*ownsandv1.Device, error) {
	o, err := c.begin("ListDevices")
	if err != nil {
		return nil, err
	}
	defer c.f.mu.Unlock()
	out := make([]*ownsandv1.Device, 0, len(o.Devices))
	for _, d := range o.Devices {
		out = append(out, proto.Clone(d).(*ownsandv1.Device))
	}
	return out, nil
}

func (c *client) TemplateGuestOS(ctx context.Context, templateID string) (ownsandv1.GuestOs, error) {
	o, err := c.begin("ListTemplates")
	if err != nil {
		return 0, err
	}
	defer c.f.mu.Unlock()
	g, ok := o.Templates[templateID]
	if !ok {
		return 0, connect.NewError(connect.CodeNotFound, errors.New("template not found"))
	}
	return g, nil
}

func (c *client) CreateSandbox(ctx context.Context, req *ownsandv1.CreateSandboxRequest) (*ownsandv1.Sandbox, error) {
	if _, err := c.begin("CreateSandbox"); err != nil {
		return nil, err
	}
	defer c.f.mu.Unlock()
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("the fake has no sandboxes yet"))
}

func (c *client) GetSandbox(ctx context.Context, sandboxID string) (*ownsandv1.Sandbox, error) {
	if _, err := c.begin("GetSandbox"); err != nil {
		return nil, err
	}
	defer c.f.mu.Unlock()
	return nil, connect.NewError(connect.CodeNotFound, errors.New("sandbox not found"))
}

func (c *client) DeleteSandbox(ctx context.Context, sandboxID string) (*ownsandv1.Sandbox, error) {
	if _, err := c.begin("DeleteSandbox"); err != nil {
		return nil, err
	}
	defer c.f.mu.Unlock()
	return nil, connect.NewError(connect.CodeNotFound, errors.New("sandbox not found"))
}
