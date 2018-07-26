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
	"os"
	"strconv"
	"strings"

	"github.com/containernetworking/cni/pkg/invoke"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/version"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/coreos/go-systemd/journal"
)

type NetConf struct {
	types.NetConf
	Kubeconfig string                   `json:"kubeconfig"`
	Default    string                   `json:"default"`
	Plugins    []map[string]interface{} `json:"plugins"`
}

// K8sArgs is the valid CNI_ARGS used for Kubernetes
type K8sArgs struct {
	types.CommonArgs
	K8S_POD_NAME               types.UnmarshallableString
	K8S_POD_NAMESPACE          types.UnmarshallableString
	K8S_POD_INFRA_CONTAINER_ID types.UnmarshallableString
}

//taken from cni/plugins/meta/flannel/flannel.go
func isString(i interface{}) bool {
	_, ok := i.(string)
	return ok
}

func isBool(i interface{}) bool {
	_, ok := i.(bool)
	return ok
}

func loadNetConf(bytes []byte) (*NetConf, error) {
	netconf := &NetConf{}
	if err := json.Unmarshal(bytes, netconf); err != nil {
		return nil, fmt.Errorf("failed to load netconf: %v", err)
	}

	if netconf.Default == "" {
		return nil, fmt.Errorf("'default' field must specify default plugin name")
	}
	if len(netconf.Plugins) == 0 {
		return nil, fmt.Errorf(`no plugins list`)
	}

	return netconf, nil
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

func getifname(ifname string) func() (string, bool) {
	ifIdx := 0
	prefix := ""

	idx := strings.LastIndexAny(ifname, "0123456789")
	if idx < 0 {
		prefix = ifname
	} else {
		runes := []rune(ifname)
		prefix = string(runes[0:idx])
		if i, err := strconv.Atoi(string(runes[idx:])); err != nil {
			ifIdx = i
		}
	}

	return func() (string, bool) {
		if ifIdx == 0 {
			ifIdx++
			return ifname, true
		}
		iface := fmt.Sprintf("%s%d", prefix, ifIdx)
		ifIdx++
		return iface, false
	}
}

func checkPlugin(netconf map[string]interface{}) error {
	if netconf["type"] == nil || !isString(netconf["type"]) {
		return fmt.Errorf("must have the field 'type' with a string")
	}
	if netconf["name"] == nil || !isString(netconf["name"]) {
		return fmt.Errorf("must have the field 'name' with a string")
	}
	return nil
}

func delegateAdd(podif func() (string, bool), netconf map[string]interface{}) error {
	netconfBytes, err := json.Marshal(netconf)
	if err != nil {
		return fmt.Errorf("error serializing multicni delegate netconf: %v", err)
	}

	iface, isDefault := podif()
	if os.Setenv("CNI_IFNAME", iface) != nil {
		return fmt.Errorf("error in setting CNI_IFNAME")
	}

	journal.Print(journal.PriInfo, "DelegateAdd: %s", netconf["type"].(string))
	result, err := invoke.DelegateAdd(netconf["type"].(string), netconfBytes)
	if err != nil {
		journal.Print(journal.PriErr, "error in invoke Delegate add - %q: %v", netconf["type"].(string), err)
		return fmt.Errorf("error in invoke Delegate add - %q: %v", netconf["type"].(string), err)
	}

	if isDefault {
		journal.Print(journal.PriWarning, "Multicni Response: %s", ResultString(&result))
		return result.Print()
	}
	return nil
}

func delegateDel(podif func() (string, bool), netconf map[string]interface{}) error {
	netconfBytes, err := json.Marshal(netconf)
	if err != nil {
		return fmt.Errorf("error serializing multicni delegate netconf: %v", err)
	}

	iface, _ := podif()
	if os.Setenv("CNI_IFNAME", iface) != nil {
		return fmt.Errorf("error in setting CNI_IFNAME")
	}

	journal.Print(journal.PriInfo, "DelegateDel: %s", netconf["type"].(string))
	err = invoke.DelegateDel(netconf["type"].(string), netconfBytes)
	if err != nil {
		journal.Print(journal.PriErr, "error in invoke Delegate del - %q: %v", netconf["type"].(string), err)
		return fmt.Errorf("error in invoke Delegate del - %q: %v", netconf["type"].(string), err)
	}

	return nil
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

func parsePodNetworkObject(podnetwork string) ([]string, error) {
	var podNet []string

	if err := json.Unmarshal([]byte(podnetwork), &podNet); err != nil {
		return nil, fmt.Errorf("parsePodNetworkObject: failed to load pod plugin err: %v | pod network: %v", err, podnetwork)
	}
	return podNet, nil
}

func cmdAdd(args *skel.CmdArgs) error {
	journal.Print(journal.PriWarning, "Multicni CNI Add")
	logParam(args)

	conf, err := loadNetConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("err in loading netconf: %v", err)
	}

	for _, plugin := range conf.Plugins {
		if err := checkPlugin(plugin); err != nil {
			return fmt.Errorf("Multicni: Err in plugin conf: %v", err)
		}
	}

	// Get pod's cni-plugin list from kubernetes api-server
	var podPlugins []string
	if kubeClient, err := createK8sClient(conf.Kubeconfig); err == nil {
		// Get pod information
		if annot, err := getK8sPodAnnotations(kubeClient, args); err == nil {
			if plugins := annot["cni-plugins"]; plugins != "" {
				podPlugins, err = parsePodNetworkObject(plugins)
				if err != nil {
					return fmt.Errorf("Multicni: Err in pod annotation 'cni-plugins': %v", plugins)
				}
			}
		}
	}
	journal.Print(journal.PriInfo, "Kubernetes cni-plugins: %v", podPlugins)

	podifName := getifname(args.IfName)
	if len(podPlugins) > 0 {
		// Add pod plugins
	NextPlugin:
		for _, podPlugin := range podPlugins {
			for _, plugin := range conf.Plugins {
				if plugin["name"] == podPlugin {
					if err := delegateAdd(podifName, plugin); err != nil {
						return err
					}
					continue NextPlugin
				}
			}
			return fmt.Errorf("failed to find plugin: %s", podPlugin)
		}
	} else {
		// Add default plugin
		for _, plugin := range conf.Plugins {
			if plugin["name"] == conf.Default {
				if err := delegateAdd(podifName, plugin); err != nil {
					return err
				}
				break
			}
		}
	}

	return nil
}

func cmdDel(args *skel.CmdArgs) error {
	journal.Print(journal.PriWarning, "Multicni CNI Del")
	logParam(args)

	conf, err := loadNetConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("err in loading netconf: %v", err)
	}

	for _, plugin := range conf.Plugins {
		if err := checkPlugin(plugin); err != nil {
			return fmt.Errorf("Multicni: Err in plugin conf: %v", err)
		}
	}

	// Get pod's cni-plugin list from kubernetes api-server
	var podPlugins []string
	if kubeClient, err := createK8sClient(conf.Kubeconfig); err == nil {
		// Get pod information
		if annot, err := getK8sPodAnnotations(kubeClient, args); err == nil {
			if plugins := annot["cni-plugins"]; plugins != "" {
				podPlugins, err = parsePodNetworkObject(plugins)
				if err != nil {
					return fmt.Errorf("Multicni: Err in pod annotation 'cni-plugins': %v", plugins)
				}
			}
		}
	}
	journal.Print(journal.PriInfo, "Kubernetes cni-plugins: %v", podPlugins)

	podifName := getifname(args.IfName)
	if len(podPlugins) > 0 {
		// Remove pod plugins
	NextPlugin:
		for _, podPlugin := range podPlugins {
			for _, plugin := range conf.Plugins {
				if plugin["name"] == podPlugin {
					if err := delegateDel(podifName, plugin); err != nil {
						return err
					}
					continue NextPlugin
				}
			}
			return fmt.Errorf("failed to find plugin: %s", podPlugin)
		}
	} else {
		// Remove default plugin
		for _, plugin := range conf.Plugins {
			if plugin["name"] == conf.Default {
				if err := delegateDel(podifName, plugin); err != nil {
					return err
				}
				break
			}
		}
	}

	return nil
}

func main() {
	skel.PluginMain(cmdAdd, cmdDel, version.All)
}
