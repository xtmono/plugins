// Copyright 2015 CNI authors
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

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"

	"github.com/coreos/go-systemd/journal"
)

func main() {
	skel.PluginMain(cmdAdd, cmdDel, version.All)
}

type Server struct {
	Url      string `json:"url"`                // server url
	User     string `json:"user,omitempty"`     // user
	Password string `json:"password,omitempty"` // password
}

type NetConf struct {
	types.NetConf
	Servers []Server `json:"servers"` // server for DHCP API
}

type DhcpReq struct {
	ClientHwAddr string `json:"clientHwAddr"`        // clientHwAddr
	VdnId        string `json:"vdnId,omitempty"`     // vdnId
	RequestIp    string `json:"requestIp,omitempty"` // requestIp
}

type DhcpResp struct {
	ClientIp     string   `json:"clientIp"`             // clientIp
	ClientHwAddr string   `json:"clientHwAddr"`         // clientHwAddr
	VdnId        string   `json:"vdnId,omitempty"`      // vdnId
	SubnetMask   string   `json:"subnetMask,omitempty"` // subnet mask
	Router       []string `json:"router,omitempty"`     // router
	DNS          []string `json:"dns,omitempty"`        // dns server
	LeaseTime    int      `json:"leaseTime,omitempty"`  // leaseTime
}

func loadConf(bytes []byte) (*NetConf, error) {
	n := &NetConf{}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, fmt.Errorf("failed to load netconf: %v", err)
	}
	return n, nil
}

func logParam(args *skel.CmdArgs) {
	journal.Print(journal.PriInfo, "  ContainerID: %s", args.ContainerID)
	journal.Print(journal.PriInfo, "  Netns: %s", args.Netns)
	journal.Print(journal.PriInfo, "  IfName: %s", args.IfName)
	journal.Print(journal.PriInfo, "  Args: %s", args.Args)
	journal.Print(journal.PriInfo, "  Path: %s", args.Path)
	journal.Print(journal.PriInfo, "  StdinData: %s", string(args.StdinData))
}

func macAddr(containerNs, ifName string) (net.HardwareAddr, error) {
	var macAddr net.HardwareAddr
	err := ns.WithNetNSPath(containerNs, func(_ ns.NetNS) error {
		dev, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("failed to find %q: %v", ifName, err)
		}
		macAddr = dev.Attrs().HardwareAddr
		return nil
	})
	return macAddr, err
}

func ResultString(r *types.Result) string {
	data, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return string(data)
}

func cmdAdd(args *skel.CmdArgs) error {
	journal.Print(journal.PriInfo, "Vdn-dhcp IPAM Add")
	logParam(args)

	conf, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	macAddr, err := macAddr(args.Netns, args.IfName)
	if err != nil {
		return err
	}

	for _, server := range conf.Servers {
		var dhcpResp DhcpResp
		dhcpReq := DhcpReq{ClientHwAddr: macAddr.String()}

		client := NewRestClient(server.User, server.Password, server.Url)
		convMacAddr := strings.NewReplacer(":", "-").Replace(macAddr.String())
		err := client.Put(convMacAddr, dhcpReq, dhcpResp)
		if err == nil {
			ipConfig := current.IPConfig{Version: "4"}
			dnsConfig := types.DNS{}

			// DHCP IP address
			if ipAddr := net.ParseIP(dhcpResp.ClientIp); ipAddr != nil {
				ipConfig.Address.IP = ipAddr
			} else {
				return fmt.Errorf("invalid ip address: %s", dhcpResp.ClientIp)
			}

			// DHCP options
			if dhcpResp.SubnetMask != "" {
				if subnetMask := net.ParseIP(dhcpResp.SubnetMask); subnetMask != nil {
					ipConfig.Address.Mask = net.IPMask(subnetMask)
				} else {
					return fmt.Errorf("invalid subnet mask: %s", dhcpResp.SubnetMask)
				}
			}
			if len(dhcpResp.Router) > 0 && dhcpResp.Router[0] != "" {
				if router := net.ParseIP(dhcpResp.Router[0]); router != nil {
					ipConfig.Gateway = router
				} else {
					return fmt.Errorf("invalid router: %s", dhcpResp.Router[0])
				}
			}

			for n, d := range dhcpResp.DNS {
				if dns := net.ParseIP(d); dns != nil {
					dnsConfig.Nameservers[n] = d
				} else {
					return fmt.Errorf("invalid dns: %s", d)
				}
			}

			// Make result
			result := &current.Result{
				IPs: []*current.IPConfig{&ipConfig},
				DNS: dnsConfig,
			}

			newResult, err := result.GetAsVersion(conf.CNIVersion)
			if err != nil {
				return err
			}
			journal.Print(journal.PriInfo, "CNI IPAM Response: %s", ResultString(&newResult))
			return newResult.Print()
			//return types.PrintResult(result, conf.CNIVersion)
		} else {
			journal.Print(journal.PriWarning, "DHCP server response error: %s", err)
		}
	}
	return fmt.Errorf("failed to connect dhcp servers")
}

func cmdDel(args *skel.CmdArgs) error {
	journal.Print(journal.PriInfo, "Vdn-dhcp IPAM Del")
	logParam(args)

	conf, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	macAddr, err := macAddr(args.Netns, args.IfName)
	if err != nil {
		return err
	}

	for _, server := range conf.Servers {
		client := NewRestClient(server.User, server.Password, server.Url)
		convMacAddr := strings.NewReplacer(":", "-").Replace(macAddr.String())
		if err := client.Delete(convMacAddr); err == nil {
			return nil
		} else {
			journal.Print(journal.PriWarning, "DHCP server response error: %s", err)
		}
	}
	return fmt.Errorf("failed to connect dhcp servers")
}
