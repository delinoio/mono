package firecracker

import (
	"errors"
	"fmt"
	"net/netip"
	"sync"
)

// DefaultGuestRange holds the /30 guest links. The agent checks at start that
// no host address or route overlaps it.
var DefaultGuestRange = netip.MustParsePrefix("10.231.0.0/16")

// Link is the /30 link of one sandbox.
type Link struct {
	Index   int
	Tap     string
	Subnet  netip.Prefix
	Gateway netip.Addr
	Guest   netip.Addr
	MAC     string
}

// Allocator hands out /30 links from a guest range.
type Allocator struct {
	mu    sync.Mutex
	rng   netip.Prefix
	used  map[int]bool
	slots int
}

// NewAllocator returns an allocator for rng (IPv4, at most /30).
func NewAllocator(rng netip.Prefix) *Allocator {
	return &Allocator{rng: rng.Masked(), used: map[int]bool{}, slots: 1 << (30 - rng.Bits())}
}

// ErrNoLink reports an exhausted guest range or a taken preferred index.
var ErrNoLink = errors.New("no free guest link")

// LinkAt returns the link of index i.
func (a *Allocator) LinkAt(i int) Link {
	base := a.rng.Addr().As4()
	v := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	v += uint32(i) * 4
	at := func(off uint32) netip.Addr {
		x := v + off
		return netip.AddrFrom4([4]byte{byte(x >> 24), byte(x >> 16), byte(x >> 8), byte(x)})
	}
	g := at(2).As4()
	return Link{
		Index:   i,
		Tap:     fmt.Sprintf("osb%d", i),
		Subnet:  netip.PrefixFrom(at(0), 30),
		Gateway: at(1),
		Guest:   at(2),
		MAC:     fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", g[0], g[1], g[2], g[3]),
	}
}

// Allocate returns a free link. With preferred >= 0 it returns that index
// only, or ErrNoLink when it is taken.
func (a *Allocator) Allocate(preferred int) (Link, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if preferred >= 0 {
		if preferred >= a.slots || a.used[preferred] {
			return Link{}, ErrNoLink
		}
		a.used[preferred] = true
		return a.LinkAt(preferred), nil
	}
	for i := 0; i < a.slots; i++ {
		if !a.used[i] {
			a.used[i] = true
			return a.LinkAt(i), nil
		}
	}
	return Link{}, ErrNoLink
}

// Release frees a link.
func (a *Allocator) Release(i int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.used, i)
}
