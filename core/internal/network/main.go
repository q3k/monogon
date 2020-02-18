// Copyright 2020 The Monogon Project Authors.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package network

import (
	"context"
	"fmt"
	"net"
	"os"

	"git.monogon.dev/source/nexantic.git/core/internal/common/service"

	"github.com/vishvananda/netlink"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

const (
	resolvConfPath     = "/etc/resolv.conf"
	resolvConfSwapPath = "/etc/resolv.conf.new"
)

type Service struct {
	*service.BaseService
	config Config

	dhcp *dhcpClient
}

type Config struct {
}

func NewNetworkService(config Config, logger *zap.Logger) (*Service, error) {
	s := &Service{
		config: config,
		dhcp:   newDHCPClient(logger),
	}
	s.BaseService = service.NewBaseService("network", logger, s)
	return s, nil
}

func setResolvconf(nameservers []net.IP, searchDomains []string) error {
	_ = os.Mkdir("/etc", 0755)
	newResolvConf, err := os.Create(resolvConfSwapPath)
	if err != nil {
		return err
	}
	defer newResolvConf.Close()
	defer os.Remove(resolvConfSwapPath)
	for _, ns := range nameservers {
		if _, err := newResolvConf.WriteString(fmt.Sprintf("nameserver %v\n", ns)); err != nil {
			return err
		}
	}
	for _, searchDomain := range searchDomains {
		if _, err := newResolvConf.WriteString(fmt.Sprintf("search %v", searchDomain)); err != nil {
			return err
		}
	}
	newResolvConf.Close()
	// Atomically swap in new config
	return unix.Rename(resolvConfSwapPath, resolvConfPath)
}

func (s *Service) addNetworkRoutes(link netlink.Link, addr net.IPNet, gw net.IP) error {
	if err := netlink.AddrReplace(link, &netlink.Addr{IPNet: &addr}); err != nil {
		return err
	}

	if gw.IsUnspecified() {
		s.Logger.Info("No default route set, only local network will be reachable", zap.String("local", addr.String()))
		return nil
	}

	route := &netlink.Route{
		Dst:   &net.IPNet{IP: net.IPv4(0, 0, 0, 0), Mask: net.IPv4Mask(0, 0, 0, 0)},
		Gw:    gw,
		Scope: netlink.SCOPE_UNIVERSE,
	}
	if err := netlink.RouteAdd(route); err != nil {
		return fmt.Errorf("could not add default route: netlink.RouteAdd(%+v): %v", route, err)
	}
	return nil
}

func (s *Service) useInterface(iface netlink.Link) error {
	go s.dhcp.run(s.Context(), iface)

	status, err := s.dhcp.status(s.Context(), true)
	if err != nil {
		return fmt.Errorf("could not get DHCP status: %v", err)
	}

	if err := setResolvconf(status.dns, []string{}); err != nil {
		s.Logger.Warn("failed to set resolvconf", zap.Error(err))
	}

	if err := s.addNetworkRoutes(iface, net.IPNet{IP: status.address.IP, Mask: status.address.Mask}, status.gateway); err != nil {
		s.Logger.Warn("failed to add routes", zap.Error(err))
	}
	return nil
}

// GetIP returns the current IP (and optionally waits for one to be assigned)
func (s *Service) GetIP(ctx context.Context, wait bool) (*net.IP, error) {
	status, err := s.dhcp.status(ctx, wait)
	if err != nil {
		return nil, err
	}
	return &status.address.IP, nil
}

func (s *Service) OnStart() error {
	s.Logger.Info("Starting network service")
	links, err := netlink.LinkList()
	if err != nil {
		s.Logger.Fatal("Failed to list network links", zap.Error(err))
	}
	var ethernetLinks []netlink.Link
	for _, link := range links {
		attrs := link.Attrs()
		if link.Type() == "device" && len(attrs.HardwareAddr) > 0 {
			if len(attrs.HardwareAddr) == 6 { // Ethernet
				if attrs.Flags&net.FlagUp != net.FlagUp {
					netlink.LinkSetUp(link) // Attempt to take up all ethernet links
				}
				ethernetLinks = append(ethernetLinks, link)
			} else {
				s.Logger.Info("Ignoring non-Ethernet interface", zap.String("interface", attrs.Name))
			}
		} else if link.Attrs().Name == "lo" {
			if err := netlink.LinkSetUp(link); err != nil {
				s.Logger.Error("Failed to take up loopback interface", zap.Error(err))
			}
		}
	}
	if len(ethernetLinks) != 1 {
		s.Logger.Warn("Network service needs exactly one link, bailing")
		return nil
	}

	link := ethernetLinks[0]
	go s.useInterface(link)

	return nil
}

func (s *Service) OnStop() error {
	return nil
}
