// Copyright (c) 2018 Timo Savola. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package dnszone implements a simple DNS zone container.
//
// See the top-level package for general documentation.
package dnszone

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/tsavola/acmedns/dns"
)

// Container of zones.  Implements acmedns.DNS, autocert.DNS, and
// dnsserver.Resolver.
type Container struct {
	mutex sync.RWMutex
	zones []*Zone

	changeReady chan struct{}
	changeZones map[*Zone]struct{}
}

// Init zones.
func Init(zones ...*Zone) *Container {
	return InitWithSerial(TimeSerial(time.Now()), zones...)
}

// Init zones with a custom initial serial number.
func InitWithSerial(serial uint32, zones ...*Zone) *Container {
	for _, z := range zones {
		z.serial = serial
	}

	return &Container{
		zones: zones,
	}
}

func (c *Container) ResolveResource(name string) (result dns.Node, serial uint32) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	for _, z := range c.zones {
		if node, ok := z.matchResource(name); ok {
			result.Name = node
			if rs := z.resolveNode(node); rs != nil {
				result.Records = rs.DeepCopy()
			}
			serial = z.serial
			return
		}
	}

	return
}

func (c *Container) ResolveZone(ctx context.Context, hostname string) (domain string, err error) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	var zoneFound bool

	for _, z := range c.zones {
		if node, ok := z.matchResource(hostname); ok {
			zoneFound = true
			if z.resolveNode(node) != nil {
				domain = z.Domain
				return
			}
		}
	}

	if zoneFound {
		err = newNodeError(hostname)
	} else {
		err = newZoneError(hostname)
	}
	return
}

func (c *Container) TransferZone(name string) (results []dns.Node, serial uint32) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	for _, z := range c.zones {
		if z.Domain == name {
			results = z.transfer()
			serial = z.serial
			return
		}
	}

	return
}

func (c *Container) ModifyTXTRecord(ctx context.Context, zoneName, node string, values []string, ttl uint32) error {
	c.mutex.Lock()

	var targetZone *Zone

	for _, z := range c.zones {
		if z.Domain == zoneName {
			targetZone = z
			break
		}
	}

	if targetZone != nil {
		// Modify zone immediately without changing serial number.
		targetZone.modifyTXTRecord(node, values, ttl)

		// Coalesce all serial number changes over a one-second period, and
		// increment each zone's serial number just once at the end of that
		// period.  That way they don't run ahead of Serial().
		ready := c.scheduleChange(targetZone)

		c.mutex.Unlock()

		// Block until the serial number change is visible.
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-ready:
			return nil
		}
	} else {
		c.mutex.Unlock()

		return newZoneError(zoneName)
	}
}

// scheduleChange must be called with write lock held.
func (c *Container) scheduleChange(z *Zone) <-chan struct{} {
	if c.changeReady == nil {
		c.changeReady = make(chan struct{})
		c.changeZones = make(map[*Zone]struct{})
		time.AfterFunc(time.Second, c.applyChanges)
	}

	c.changeZones[z] = struct{}{}
	return c.changeReady
}

func (c *Container) applyChanges() {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	for z := range c.changeZones {
		z.serial++
	}

	close(c.changeReady)
	c.changeReady = nil
	c.changeZones = nil
}

// Zone enumerates the nodes of a domain.
//
// Must not be modified directly after its Container has been used for
// resolving resources or transferring zones.
type Zone struct {
	Domain string
	Nodes  map[string]*dns.Records

	serial uint32 // managed by Container
}

func (z *Zone) matchResource(name string) (node string, ok bool) {
	switch {
	case z.Domain == name:
		node = dns.Apex
		ok = true

	case strings.HasSuffix(name, "."+z.Domain):
		prefix := name[:len(name)-1-len(z.Domain)]
		if !strings.Contains(prefix, ".") {
			node = prefix
			ok = true
		}
	}

	return
}

func (z *Zone) resolveNode(node string) (rs *dns.Records) {
	rs = z.Nodes[node]
	if rs == nil && node != dns.Apex { // wildcard doesn't apply to apex
		rs = z.Nodes[dns.Wildcard]
	}
	return
}

func (z *Zone) transfer() (results []dns.Node) {
	results = make([]dns.Node, 0, len(z.Nodes))

	if rs := z.Nodes[dns.Apex]; rs != nil {
		results = append(results, dns.Node{
			Name:    dns.Apex,
			Records: rs.DeepCopy(),
		})
	}

	for name, rs := range z.Nodes {
		if name != dns.Apex && name != dns.Wildcard {
			results = append(results, dns.Node{
				Name:    name,
				Records: rs.DeepCopy(),
			})
		}
	}

	if rs := z.Nodes[dns.Wildcard]; rs != nil {
		results = append(results, dns.Node{
			Name:    dns.Wildcard,
			Records: rs.DeepCopy(),
		})
	}

	return
}

func (z *Zone) modifyTXTRecord(node string, values []string, ttl uint32) {
	if len(values) > 0 {
		if z.Nodes == nil {
			z.Nodes = make(map[string]*dns.Records)
		}

		rs := z.Nodes[node]
		if rs == nil {
			rs = new(dns.Records)
			z.Nodes[node] = rs
		}

		rs.TXT = dns.TextRecord{
			Values: append([]string(nil), values...), // copy
			TTL:    ttl,
		}
	} else {
		rs := z.Nodes[node]
		if rs != nil {
			rs.TXT = dns.TextRecord{
				Values: nil,
				TTL:    0,
			}

			if rs.Empty() {
				delete(z.Nodes, node)
			}
		}
	}
}
