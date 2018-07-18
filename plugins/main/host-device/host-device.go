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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"

	"github.com/coreos/go-systemd/journal"
)

type NetConf struct {
	types.NetConf
	Devices     []string `json:"devices"`     // Device-Name, something like eth0 or can0 etc.
	HWAddrs     []string `json:"hwaddrs"`     // MAC Address of target network interface
	KernelPaths []string `json:"kernelpaths"` // Kernelpath of the device
}

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

func loadConf(bytes []byte) (*NetConf, error) {
	n := &NetConf{}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, fmt.Errorf("failed to load netconf: %v", err)
	}
	if (n.Devices == nil || len(n.Devices) == 0) &&
		(n.HWAddrs == nil || len(n.HWAddrs) == 0) &&
		(n.KernelPaths == nil || len(n.KernelPaths) == 0) {
		return nil, fmt.Errorf(`specify either "device", "hwaddr" or "kernelpath"`)
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

func ResultString(r *types.Result) string {
	data, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return string(data)
}

func cmdAdd(args *skel.CmdArgs) error {
	journal.Print(journal.PriInfo, "Host-device CNI Add")
	logParam(args)

	cfg, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	containerNs, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer containerNs.Close()

	hostDev, err := getLink(cfg.Devices, cfg.HWAddrs, cfg.KernelPaths)
	if err != nil {
		return fmt.Errorf("failed to find host device: %v", err)
	}

	contDev, err := moveLinkIn(hostDev, containerNs, args.IfName)
	if err != nil {
		return fmt.Errorf("failed to move link %v", err)
	}

	// Move out link if err to avoid link hold in this ns
	defer func() {
		if err != nil {
			containerNs.Do(func(_ ns.NetNS) error {
				return moveLinkOut(containerNs, args.IfName)
			})
		}
	}()

	// run the IPAM plugin and get back the config to apply
	r, err := ipam.ExecAdd(cfg.IPAM.Type, args.StdinData)
	if err != nil {
		return err
	}

	// Invoke ipam del if err to avoid ip leak
	defer func() {
		if err != nil {
			ipam.ExecDel(cfg.IPAM.Type, args.StdinData)
		}
	}()

	// Convert whatever the IPAM result was into the current Result type
	result, err := current.NewResultFromResult(r)
	if err != nil {
		return err
	}
	if len(result.IPs) == 0 {
		return errors.New("IPAM plugin returned missing IP config")
	}

	result.Interfaces = []*current.Interface{
		&current.Interface{
			Name:    args.IfName,
			Mac:     contDev.Attrs().HardwareAddr.String(),
			Sandbox: containerNs.Path()}}

	for _, ipc := range result.IPs {
		// All addresses apply to the container interface
		ipc.Interface = current.Int(0)
	}

	err = containerNs.Do(func(_ ns.NetNS) error {
		if err := ipam.ConfigureIface(args.IfName, result); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}

	result.DNS = cfg.DNS

	newResult, err := result.GetAsVersion(cfg.CNIVersion)
	if err != nil {
		return err
	}
	journal.Print(journal.PriInfo, "CNI Response: %s", ResultString(&newResult))
	return newResult.Print()
	//return types.PrintResult(result, cfg.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	journal.Print(journal.PriInfo, "Host-device CNI Del")
	logParam(args)

	cfg, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	err = ipam.ExecDel(cfg.IPAM.Type, args.StdinData)
	if err != nil {
		return err
	}

	containerNs, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer containerNs.Close()

	if err := moveLinkOut(containerNs, args.IfName); err != nil {
		return err
	}

	return nil
}

func moveLinkIn(hostDev netlink.Link, containerNs ns.NetNS, ifName string) (netlink.Link, error) {
	if err := netlink.LinkSetDown(hostDev); err != nil {
		return nil, fmt.Errorf("failed to host interface down %q: %v", ifName, err)
	}
	if err := netlink.LinkSetNsFd(hostDev, int(containerNs.Fd())); err != nil {
		return nil, err
	}

	var contDev netlink.Link
	if err := containerNs.Do(func(_ ns.NetNS) error {
		var err error
		contDev, err = netlink.LinkByName(hostDev.Attrs().Name)
		if err != nil {
			return fmt.Errorf("failed to find %q: %v", hostDev.Attrs().Name, err)
		}
		// Save host device name into the container device's alias property
		if err := netlink.LinkSetAlias(contDev, hostDev.Attrs().Name); err != nil {
			return fmt.Errorf("failed to set alias to %q: %v", hostDev.Attrs().Name, err)
		}
		// Rename container device to respect args.IfName
		if err := netlink.LinkSetName(contDev, ifName); err != nil {
			return fmt.Errorf("failed to rename device %q to %q: %v", hostDev.Attrs().Name, ifName, err)
		}
		// Retrieve link again to get up-to-date name and attributes
		contDev, err = netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("failed to find %q: %v", ifName, err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return contDev, nil
}

func moveLinkOut(containerNs ns.NetNS, ifName string) error {
	defaultNs, err := ns.GetCurrentNS()
	if err != nil {
		return err
	}
	defer defaultNs.Close()

	return containerNs.Do(func(_ ns.NetNS) error {
		dev, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("failed to find %q: %v", ifName, err)
		}

		// Rename device to it's original name
		if err := netlink.LinkSetDown(dev); err != nil {
			return fmt.Errorf("failed to container interface down %q: %v", ifName, err)
		}
		if err := netlink.LinkSetName(dev, dev.Attrs().Alias); err != nil {
			return fmt.Errorf("failed to restore %q to original name %q: %v", ifName, dev.Attrs().Alias, err)
		}
		if err := netlink.LinkSetNsFd(dev, int(defaultNs.Fd())); err != nil {
			return fmt.Errorf("failed to move %q to host netns: %v", dev.Attrs().Alias, err)
		}
		return nil
	})
}

func getLink(devices, hwaddrs, kernelpaths []string) (netlink.Link, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("failed to list node links: %v", err)
	}

	for _, device := range devices {
		if link, err := netlink.LinkByName(device); err == nil {
			return link, nil
		}
	}

	for _, hwaddr := range hwaddrs {
		if hwAddr, err := net.ParseMAC(hwaddr); err != nil {
			journal.Print(journal.PriErr,
				"failed to parse MAC address %q: %v", hwaddr, err)
		} else {
			for _, link := range links {
				if bytes.Equal(link.Attrs().HardwareAddr, hwAddr) {
					return link, nil
				}
			}
		}
	}

	for _, kernelpath := range kernelpaths {
		if !filepath.IsAbs(kernelpath) || !strings.HasPrefix(kernelpath, "/sys/devices/") {
			journal.Print(journal.PriErr,
				"kernel device path %q must be absolute and begin with /sys/devices/", kernelpath)
		} else {
			netDir := filepath.Join(kernelpath, "net")
			files, err := ioutil.ReadDir(netDir)
			if err != nil {
				journal.Print(journal.PriErr,
					"failed to find network devices at %q", netDir)
			} else {
				// Grab the first device from eg /sys/devices/pci0000:00/0000:00:19.0/net
				for _, file := range files {
					// Make sure it's really an interface
					for _, l := range links {
						if file.Name() == l.Attrs().Name {
							return l, nil
						}
					}
				}
			}
		}
	}

	return nil, fmt.Errorf("failed to find physical interface")
}

func main() {
	skel.PluginMain(cmdAdd, cmdDel, version.All)
}
