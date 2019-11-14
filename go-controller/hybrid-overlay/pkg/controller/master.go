package controller

import (
	"bytes"
	"fmt"
	"net"

	"github.com/sirupsen/logrus"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/openshift/origin/pkg/util/netutils"
	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// MasterController is the master hybrid overlay controller
type MasterController struct {
	kube      *kube.Kube
	allocator []netutils.SubnetAllocator
}

// NewMaster a new master controller that listens for node events
func NewMaster(clientset kubernetes.Interface, subnets []config.CIDRNetworkEntry) (*MasterController, error) {
	m := &MasterController{
		kube: &kube.Kube{KClient: clientset},
	}

	alreadyAllocated := make([]string, 0)
	existingNodes, err := m.kube.GetNodes()
	if err != nil {
		return nil, fmt.Errorf("Error in initializing/fetching subnets: %v", err)
	}
	for _, node := range existingNodes.Items {
		if houtil.IsWindowsNode(&node) {
			hostsubnet, ok := node.Annotations[types.HybridOverlayHostSubnet]
			if ok {
				alreadyAllocated = append(alreadyAllocated, hostsubnet)
			}
		}
	}

	masterSubnetAllocatorList := make([]netutils.SubnetAllocator, 0)
	// NewSubnetAllocator is a subnet IPAM, which takes a CIDR (first argument)
	// and gives out subnets of length 'hostSubnetLength' (second argument)
	// but omitting any that exist in 'subrange' (third argument)
	for _, subnet := range subnets {
		subrange := make([]string, 0)
		for _, allocatedRange := range alreadyAllocated {
			firstAddress, _, err := net.ParseCIDR(allocatedRange)
			if err != nil {
				logrus.Errorf("error parsing already allocated hostsubnet %q: %v", allocatedRange, err)
				continue
			}
			if subnet.CIDR.Contains(firstAddress) {
				subrange = append(subrange, allocatedRange)
			}
		}
		subnetAllocator, err := netutils.NewSubnetAllocator(subnet.CIDR.String(), 32-subnet.HostSubnetLength, subrange)
		if err != nil {
			return nil, fmt.Errorf("error creating subnet allocator for %q: %v", subnet.CIDR.String(), err)
		}
		masterSubnetAllocatorList = append(masterSubnetAllocatorList, subnetAllocator)
	}
	m.allocator = masterSubnetAllocatorList

	return m, nil
}

// Start is the top level function to run hybrid overlay in master mode
func (m *MasterController) Start(wf *factory.WatchFactory) error {
	return houtil.StartNodeWatch(m, wf)
}

func parseNodeHostSubnet(node *kapi.Node, annotation string) (*net.IPNet, error) {
	sub, ok := node.Annotations[annotation]
	if !ok {
		return nil, nil
	}

	_, subnet, err := net.ParseCIDR(sub)
	if err != nil {
		return nil, fmt.Errorf("error parsing node %s annotation %s value %q: %v",
			node.Name, annotation, sub, err)
	}

	return subnet, nil
}

func sameCIDR(a, b *net.IPNet) bool {
	if a == b {
		return true
	} else if (a == nil && b != nil) || (a != nil && b == nil) {
		return false
	}
	return a.IP.Equal(b.IP) && bytes.Equal(a.Mask, b.Mask)
}

// updateNodeAnnotation returns:
// 1) the annotation name
// 2) the annotation value (if any)
// 3) true to add the annotation, false to delete it from the node
// 4) any error that occurred
func (m *MasterController) updateNodeAnnotation(node *kapi.Node, annotator kube.Annotator) error {
	extHostsubnet, _ := parseNodeHostSubnet(node, types.HybridOverlayHostSubnet)
	ovnHostsubnet, _ := parseNodeHostSubnet(node, ovn.OvnHostSubnet)

	if !houtil.IsWindowsNode(node) {
		// Sync/remove subnet annotations for Linux nodes
		if ovnHostsubnet == nil {
			if extHostsubnet != nil {
				// remove any HybridOverlayHostSubnet
				logrus.Infof("Will remove node %s hybrid overlay HostSubnet %s", node.Name, extHostsubnet.String())
				annotator.Del(types.HybridOverlayHostSubnet)
			}
		} else if !sameCIDR(ovnHostsubnet, extHostsubnet) {
			// sync the HybridHostSubnet with the OVN-assigned one
			logrus.Infof("will sync node %s hybrid overlay HostSubnet %s", node.Name, ovnHostsubnet.String())
			annotator.Set(types.HybridOverlayHostSubnet, ovnHostsubnet.String())
		}
		return nil
	}

	// Do not allocate a subnet if the node already has one
	if extHostsubnet != nil {
		return nil
	}

	// No subnet reserved; allocate a new one
	for _, subnetAllocator := range m.allocator {
		if subnet, err := subnetAllocator.GetNetwork(); err == nil {
			logrus.Infof("Allocated node %s hybrid overlay HostSubnet %s", node.Name, subnet.String())
			annotator.SetWithFailureHandler(types.HybridOverlayHostSubnet, subnet.String(), func(node *kapi.Node, key, val string) {
				if _, cidr, _ := net.ParseCIDR(val); cidr != nil {
					_ = m.releaseNodeSubnet(node.Name, cidr)
				}
			})
			return nil
		} else if err != netutils.ErrSubnetAllocatorFull {
			return err
		}
		// Current subnet exhausted, check next possible subnet
	}

	// All subnets exhausted
	return fmt.Errorf("no available subnets to allocate")
}

func (m *MasterController) releaseNodeSubnet(nodeName string, nodeSubnet *net.IPNet) error {
	// allocator.network is unexported, so we must iterate all allocators
	// and attempt to release the subnet for each one. If no allocator
	// can release the subnet, return an error.
	for _, possibleSubnet := range m.allocator {
		if err := possibleSubnet.ReleaseNetwork(nodeSubnet); err == nil {
			logrus.Infof("Deleted HostSubnet %v for node %s", nodeSubnet, nodeName)
			return nil
		}
	}
	return fmt.Errorf("failed to delete subnet %s for node %q: subnet not found in any CIDR range or already available", nodeSubnet, nodeName)
}

func (m *MasterController) handleOverlayPort(node *kapi.Node, annotator kube.Annotator) error {
	// Only applicable to Linux nodes
	if houtil.IsWindowsNode(node) {
		return nil
	}

	_, haveDRMACAnnotation := node.Annotations[types.HybridOverlayDrMac]

	subnet, err := parseNodeHostSubnet(node, ovn.OvnHostSubnet)
	if subnet == nil || err != nil {
		// No subnet allocated yet; clean up
		if haveDRMACAnnotation {
			m.deleteOverlayPort(node)
			annotator.Del(types.HybridOverlayDrMac)
		}
		return nil
	}

	if haveDRMACAnnotation {
		// already set up; do nothing
		return nil
	}

	portName := houtil.GetHybridOverlayPortName(node.Name)
	portMAC, portIP, _ := util.GetPortAddresses(portName)
	if portMAC == nil || portIP == nil {
		if portMAC == nil {
			portMAC, _ = net.ParseMAC(util.GenerateMac())
		}
		if portIP == nil {
			// Get the 3rd address in the node's subnet; the first is taken
			// by the k8s-cluster-router port, the second by the management port
			first := util.NextIP(subnet.IP)
			second := util.NextIP(first)
			portIP = util.NextIP(second)
		}

		var stderr string
		_, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", node.Name, portName,
			"--", "lsp-set-addresses", portName, portMAC.String()+" "+portIP.String())
		if err != nil {
			return fmt.Errorf("failed to add hybrid overlay port for node %s"+
				", stderr:%s: %v", node.Name, stderr, err)
		}

	}
	annotator.Set(types.HybridOverlayDrMac, portMAC.String())

	return nil
}

func (m *MasterController) deleteOverlayPort(node *kapi.Node) {
	portName := houtil.GetHybridOverlayPortName(node.Name)
	_, _, _ = util.RunOVNNbctl("--", "--if-exists", "lsp-del", portName)
}

// Add handles node additions
func (m *MasterController) Add(node *kapi.Node) {
	annotator := kube.NewNodeAnnotator(m.kube, node)

	if err := m.updateNodeAnnotation(node, annotator); err != nil {
		logrus.Errorf("failed to update node %q hybrid overlay subnet annotation: %v", node.Name, err)
	}

	if err := m.handleOverlayPort(node, annotator); err != nil {
		logrus.Errorf("failed to set up hybrid overlay logical switch port for %s: %v", node.Name, err)
	}

	annotator.Run()
}

// Update handles node updates
func (m *MasterController) Update(oldNode, newNode *kapi.Node) {
	m.Add(newNode)
}

// Delete handles node deletions
func (m *MasterController) Delete(node *kapi.Node) {
	// Run delete for all nodes in case the OS annotation was lost or changed

	if subnet, _ := parseNodeHostSubnet(node, types.HybridOverlayHostSubnet); subnet != nil {
		if err := m.releaseNodeSubnet(node.Name, subnet); err != nil {
			logrus.Errorf(err.Error())
		}
	}

	if _, ok := node.Annotations[types.HybridOverlayDrMac]; ok {
		m.deleteOverlayPort(node)
	}
}

// Sync handles synchronizing the initial node list
func (m *MasterController) Sync(nodes []*kapi.Node) {
	// Unused because our initial node list sync needs to return
	// errors which this function cannot do
}
