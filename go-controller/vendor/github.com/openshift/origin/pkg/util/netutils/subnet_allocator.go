package netutils

import (
	"fmt"
	"net"
	"sync"
)

var ErrSubnetAllocatorFull = fmt.Errorf("No subnets available.")

type SubnetAllocator interface {
	GetNetwork() (*net.IPNet, error)
	ReleaseNetwork(ipnet *net.IPNet) error
}

type ipv4SubnetAllocator struct {
	network    *net.IPNet
	hostBits   uint32
	leftShift  uint32
	leftMask   uint32
	rightShift uint32
	rightMask  uint32
	next       uint32
	allocMap   map[string]bool
	mutex      sync.Mutex
}

type ipv6SubnetAllocator struct {
	network  *net.IPNet
	hostBits uint32
	allocMap map[string]bool
	mutex    sync.Mutex
}

func NewSubnetAllocator(network string, hostBits uint32, inUse []string) (SubnetAllocator, error) {
	_, netIP, err := net.ParseCIDR(network)
	if err != nil {
		return nil, fmt.Errorf("Failed to parse network address: %q", network)
	}

	if netIP.IP.To4() != nil {
		return newIPv4SubnetAllocator(netIP, hostBits, inUse)
	} else {
		return newIPv6SubnetAllocator(netIP, hostBits, inUse)
	}
}

func newIPv4SubnetAllocator(netIP *net.IPNet, hostBits uint32, inUse []string) (SubnetAllocator, error) {
	netMaskSize, _ := netIP.Mask.Size()
	if hostBits == 0 {
		return nil, fmt.Errorf("Host capacity cannot be zero.")
	} else if hostBits > (32 - uint32(netMaskSize)) {
		return nil, fmt.Errorf("Subnet capacity cannot be larger than number of networks available.")
	}
	subnetBits := 32 - uint32(netMaskSize) - hostBits

	// In the simple case, the subnet part of the 32-bit IP address is just the subnet
	// number shifted hostBits to the left. However, if hostBits isn't a multiple of
	// 8, then it can be difficult to distinguish the subnet part and the host part
	// visually. (Eg, given network="10.1.0.0/16" and hostBits=6, then "10.1.0.50" and
	// "10.1.0.70" are on different networks.)
	//
	// To try to avoid this confusion, if the subnet extends into the next higher
	// octet, we rotate the bits of the subnet number so that we use the subnets with
	// all 0s in the shared octet first. So again given network="10.1.0.0/16",
	// hostBits=6, we first allocate 10.1.0.0/26, 10.1.1.0/26, etc, through
	// 10.1.255.0/26 (just like we would with /24s in the hostBits=8 case), and only
	// if we use up all of those subnets do we start allocating 10.1.0.64/26,
	// 10.1.1.64/26, etc.
	var leftShift, rightShift uint32
	var leftMask, rightMask uint32
	if hostBits%8 != 0 && ((hostBits-1)/8 != (hostBits+subnetBits-1)/8) {
		leftShift = 8 - (hostBits % 8)
		leftMask = uint32(1)<<(32-uint32(netMaskSize)) - 1
		rightShift = subnetBits - leftShift
		rightMask = (uint32(1)<<leftShift - 1) << hostBits
	} else {
		leftShift = 0
		leftMask = 0xFFFFFFFF
		rightShift = 0
		rightMask = 0
	}

	amap := make(map[string]bool)
	for _, netStr := range inUse {
		_, nIp, err := net.ParseCIDR(netStr)
		if err != nil {
			fmt.Println("Failed to parse network address: ", netStr)
			continue
		}
		if !netIP.Contains(nIp.IP) {
			fmt.Println("Provided subnet doesn't belong to network: ", nIp)
			continue
		}
		amap[nIp.String()] = true
	}
	return &ipv4SubnetAllocator{
		network:    netIP,
		hostBits:   hostBits,
		leftShift:  leftShift,
		leftMask:   leftMask,
		rightShift: rightShift,
		rightMask:  rightMask,
		next:       0,
		allocMap:   amap,
	}, nil
}

func (sna *ipv4SubnetAllocator) GetNetwork() (*net.IPNet, error) {
	var (
		numSubnets    uint32
		numSubnetBits uint32
	)
	sna.mutex.Lock()
	defer sna.mutex.Unlock()

	baseipu := IPToUint32(sna.network.IP)
	netMaskSize, _ := sna.network.Mask.Size()
	numSubnetBits = 32 - uint32(netMaskSize) - sna.hostBits
	numSubnets = 1 << numSubnetBits

	var i uint32
	for i = 0; i < numSubnets; i++ {
		n := (i + sna.next) % numSubnets
		shifted := n << sna.hostBits
		ipu := baseipu | ((shifted << sna.leftShift) & sna.leftMask) | ((shifted >> sna.rightShift) & sna.rightMask)
		genIp := Uint32ToIP(ipu)
		genSubnet := &net.IPNet{IP: genIp, Mask: net.CIDRMask(int(numSubnetBits)+netMaskSize, 32)}
		if !sna.allocMap[genSubnet.String()] {
			sna.allocMap[genSubnet.String()] = true
			sna.next = n + 1
			return genSubnet, nil
		}
	}

	sna.next = 0
	return nil, ErrSubnetAllocatorFull
}

func (sna *ipv4SubnetAllocator) ReleaseNetwork(ipnet *net.IPNet) error {
	sna.mutex.Lock()
	defer sna.mutex.Unlock()
	if !sna.network.Contains(ipnet.IP) {
		return fmt.Errorf("Provided subnet %v doesn't belong to the network %v.", ipnet, sna.network)
	}

	ipnetStr := ipnet.String()
	if !sna.allocMap[ipnetStr] {
		return fmt.Errorf("Provided subnet %v is already available.", ipnet)
	}

	sna.allocMap[ipnetStr] = false

	return nil
}

func newIPv6SubnetAllocator(netIP *net.IPNet, hostBits uint32, inUse []string) (SubnetAllocator, error) {
	if hostBits == 0 {
		return nil, fmt.Errorf("Host capacity cannot be zero.")
	}
	netMaskSize, _ := netIP.Mask.Size()
	if hostBits > 128-uint32(netMaskSize) {
		return nil, fmt.Errorf("Subnet capacity cannot be larger than number of networks available.")
	}

	amap := make(map[string]bool)
	for _, netStr := range inUse {
		_, nIp, err := net.ParseCIDR(netStr)
		if err != nil {
			fmt.Println("Failed to parse network address: ", netStr)
			continue
		}
		if !netIP.Contains(nIp.IP) {
			fmt.Println("Provided subnet doesn't belong to network: ", nIp)
			continue
		}
		amap[nIp.String()] = true
	}
	return &ipv6SubnetAllocator{
		network:  netIP,
		hostBits: hostBits,
		allocMap: amap,
	}, nil
}

// Example: getSubnet({"fd01::/48", 64, 1) => "fd01::1:0:0:0:0/64"
func getSubnet(network *net.IPNet, hostBits, index uint32) *net.IPNet {
	upper, lower := IPToUint64(network.IP)
	netMaskSize, bits := network.Mask.Size()

	if netMaskSize >= 64 {
		// This is because of OVN LSP dynamic addressing based
		// on EUI64 only uses the /64 prefix of the host network
		panic("FIXME: cluster networks must be larger than /64")
	}
	upper = ((upper >> (hostBits - 64)) + uint64(index)) << (hostBits - 64)

	return &net.IPNet{
		IP:   Uint64ToIP(upper, lower),
		Mask: net.CIDRMask(bits-int(hostBits), bits),
	}
}

func (sna *ipv6SubnetAllocator) GetNetwork() (*net.IPNet, error) {
	var (
		numSubnets    uint32
		numSubnetBits uint32
	)
	sna.mutex.Lock()
	defer sna.mutex.Unlock()

	netMaskSize, _ := sna.network.Mask.Size()
	numSubnetBits = 128 - uint32(netMaskSize) - sna.hostBits
	numSubnets = 1 << numSubnetBits

	var i uint32
	for i = 0; i < numSubnets; i++ {
		genSubnet := getSubnet(sna.network, sna.hostBits, i)
		if !sna.allocMap[genSubnet.String()] {
			sna.allocMap[genSubnet.String()] = true
			return genSubnet, nil
		}
	}

	return nil, ErrSubnetAllocatorFull
}

func (sna *ipv6SubnetAllocator) ReleaseNetwork(ipnet *net.IPNet) error {
	sna.mutex.Lock()
	defer sna.mutex.Unlock()
	if !sna.network.Contains(ipnet.IP) {
		return fmt.Errorf("Provided subnet %v doesn't belong to the network %v.", ipnet, sna.network)
	}

	ipnetStr := ipnet.String()
	if !sna.allocMap[ipnetStr] {
		return fmt.Errorf("Provided subnet %v is already available.", ipnet)
	}

	sna.allocMap[ipnetStr] = false

	return nil
}
