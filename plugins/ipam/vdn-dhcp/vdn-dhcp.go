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

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/coreos/go-systemd/journal"
)

func main() {
	skel.PluginMain(cmdAdd, cmdDel, version.All)
}

type NetConf struct {
	types.NetConf
	IPAM struct {
		Servers []Server `json:"servers"` // server for DHCP API
	} `json:"ipam,omitempty"`
	Kubeconfig string `json:"kubeconfig,omitempty"`
}

// K8sArgs is the valid CNI_ARGS used for Kubernetes
type K8sArgs struct {
	types.CommonArgs
	K8S_POD_NAME               types.UnmarshallableString
	K8S_POD_NAMESPACE          types.UnmarshallableString
	K8S_POD_INFRA_CONTAINER_ID types.UnmarshallableString
}

type Server struct {
	Url      string `json:"url"`                // server url
	User     string `json:"user,omitempty"`     // user
	Password string `json:"password,omitempty"` // password
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

func resultString(r *types.Result) string {
	data, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return string(data)
}

func createK8sClient(kubeconfig string) (*kubernetes.Clientset, error) {
	journal.Print(journal.PriInfo, "kubeconfig path: %s", kubeconfig)

	// uses the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}

	// creates the clientset
	return kubernetes.NewForConfig(config)
}

func loadArgs(args string) (*K8sArgs, error) {
	k8sArgs := K8sArgs{}
	if err := types.LoadArgs(args, &k8sArgs); err != nil {
		return nil, err
	}
	if string(k8sArgs.K8S_POD_NAMESPACE) == "" || string(k8sArgs.K8S_POD_NAME) == "" {
		return nil, fmt.Errorf("failed to get pod_namespace & pod_name from args: %v", args)
	}
	return &k8sArgs, nil
}

func getK8sPodAnnotations(client *kubernetes.Clientset, args *skel.CmdArgs) (map[string]string, error) {
	k8sArgs, err := loadArgs(args.Args)
	if err != nil {
		return nil, err
	}

	pod, err := client.CoreV1().Pods(string(k8sArgs.K8S_POD_NAMESPACE)).
		Get(string(k8sArgs.K8S_POD_NAME), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	return pod.Annotations, nil
}

func macAddr(containerNs, ifName string) (net.HardwareAddr, error) {
	var macAddr net.HardwareAddr
	err := ns.WithNetNSPath(containerNs, func(_ ns.NetNS) error {
		dev, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("failed to find nic %q: %v", ifName, err)
		}
		macAddr = dev.Attrs().HardwareAddr
		return nil
	})
	return macAddr, err
}

func cmdAdd(args *skel.CmdArgs) error {
	journal.Print(journal.PriWarning, "Vdn-dhcp IPAM Add")
	logParam(args)

	conf, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	// Get mac address of container
	macAddr, err := macAddr(args.Netns, args.IfName)
	if err != nil {
		return err
	}

	// Get vdnId from kubernetes api-server
	var vdnId string
	if kubeClient, err := createK8sClient(conf.Kubeconfig); err == nil {
		// Get pod information
		if annot, err := getK8sPodAnnotations(kubeClient, args); err == nil {
			vdnId = annot["vdnId"]
		}
	}
	journal.Print(journal.PriInfo, "Kubernetes vdnId: %v", vdnId)

	for _, server := range conf.IPAM.Servers {
		var dhcpResp DhcpResp
		dhcpReq := DhcpReq{
			ClientHwAddr: macAddr.String(),
			VdnId:        vdnId,
		}

		restClient := NewRestClient(server.Url, server.User, server.Password)
		journal.Print(journal.PriInfo, "Rest PUT Req: %s, %v", macAddr.String(), dhcpReq)
		err := restClient.Put(macAddr.String(), &dhcpReq, &dhcpResp)
		journal.Print(journal.PriInfo, "Rest Resp: %v", dhcpResp)
		if err == nil {
			// Make result
			result := &current.Result{
				Interfaces: []*current.Interface{},
				IPs:        []*current.IPConfig{},
				Routes:     []*types.Route{},
				DNS:        types.DNS{},
			}

			// DHCP IP address
			ipConfig := current.IPConfig{
				Version: "4",
				Address: net.IPNet{},
				Gateway: net.IP{},
			}
			result.IPs = append(result.IPs, &ipConfig)
			if ipAddr := net.ParseIP(dhcpResp.ClientIp); ipAddr != nil {
				ipConfig.Address.IP = ipAddr
			} else {
				return fmt.Errorf("invalid ip address: %s", dhcpResp.ClientIp)
			}

			// DHCP option: subnet mask
			if dhcpResp.SubnetMask != "" {
				if subnetMask := net.ParseIP(dhcpResp.SubnetMask); subnetMask != nil {
					ipConfig.Address.Mask = net.IPMask(subnetMask)
				} else {
					return fmt.Errorf("invalid subnet mask: %s", dhcpResp.SubnetMask)
				}
			} else {
				ipConfig.Address.Mask = net.IPMask(net.IPv4bcast)
			}

			// DHCP option: gateway
			if len(dhcpResp.Router) > 0 && dhcpResp.Router[0] != "" {
				if router := net.ParseIP(dhcpResp.Router[0]); router != nil {
					ipConfig.Gateway = router
					result.Routes = append(result.Routes, &types.Route{
						Dst: net.IPNet{IP: net.IPv4zero, Mask: net.IPMask(net.IPv4zero)},
						GW:  router,
					})
				} else {
					return fmt.Errorf("invalid router: %s", dhcpResp.Router[0])
				}
			}

			// DHCP option: nameserver
			for _, dns := range dhcpResp.DNS {
				if dns != "" {
					if net.ParseIP(dns) != nil {
						result.DNS.Nameservers = append(result.DNS.Nameservers, dns)
					} else {
						return fmt.Errorf("invalid dns: %s", dns)
					}
				}
			}

			newResult, err := result.GetAsVersion(conf.CNIVersion)
			if err != nil {
				return err
			}
			journal.Print(journal.PriWarning, "CNI IPAM Response: %s", resultString(&newResult))
			return newResult.Print()
			//return types.PrintResult(result, conf.CNIVersion)
		} else {
			journal.Print(journal.PriWarning, "DHCP server response error: %s, %s", server.Url, err)
		}
	}
	return fmt.Errorf("failed to connect dhcp servers")
}

func cmdDel(args *skel.CmdArgs) error {
	journal.Print(journal.PriWarning, "Vdn-dhcp IPAM Del")
	logParam(args)

	conf, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	macAddr, err := macAddr(args.Netns, args.IfName)
	if err != nil {
		return err
	}

	for _, server := range conf.IPAM.Servers {
		restClient := NewRestClient(server.Url, server.User, server.Password)
		journal.Print(journal.PriInfo, "Rest Delete Req: %s", macAddr.String())
		if err := restClient.Delete(macAddr.String()); err == nil {
			return nil
		} else {
			journal.Print(journal.PriWarning, "DHCP server response error: %s", err)
		}
	}
	return fmt.Errorf("failed to connect dhcp servers")
}
