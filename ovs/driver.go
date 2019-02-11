package ovs

import (
	"fmt"
	"net"
	"strconv"
	"time"

	log "github.com/Sirupsen/logrus"
	dknet "github.com/docker/go-plugins-helpers/network"
	"github.com/samalba/dockerclient"
	"github.com/socketplane/libovsdb"
	"github.com/vishvananda/netlink"
)

const (
	defaultRoute     = "0.0.0.0/0"
	ovsPortPrefix    = "ovs-veth0-"
	bridgePrefix     = "ovsbr-"
	containerEthName = "eth"

	optionKey = "com.docker.network.generic"

	mtuOption           = "net.gopher.ovs.bridge.mtu"
	modeOption          = "net.gopher.ovs.bridge.mode"
	bridgeNameOption    = "net.gopher.ovs.bridge.name"
	bindInterfaceOption = "net.gopher.ovs.bridge.bind_interface"

	modeNAT  = "nat"
	modeFlat = "flat"

	defaultMTU  = 1500
	defaultMode = modeNAT
)

var (
	validModes = map[string]bool{
		modeNAT:  true,
		modeFlat: true,
	}
)

type Driver struct {
	name string
	dknet.Driver
	dockerer
	ovsdber
	networks map[string]*NetworkState
	OvsdbNotifier
}

// NetworkState is filled in at network creation time
// it contains state that we wish to keep for each network
type NetworkState struct {
	BridgeName        string
	MTU               int
	Mode              string
	Gateway           string
	GatewayMask       string
	FlatBindInterface string
}

func (d *Driver) findNetworkState(id string) (*NetworkState, error) {
	ns, found := d.networks[id]
	if found {
		return ns, nil
	}

	if d.dockerer.client == nil {
		return nil, fmt.Errorf("Docker client disabled; unable to get network state")
	}


	network, err := d.dockerer.client.InspectNetwork(id)
	if err != nil {
		return nil, err
	}

	log.Debugf("findNetworkState: %+v", network)

	if network.Driver != d.name {
		return nil, fmt.Errorf("Not our network")
	}

	gateway := ""

	for _, value := range network.IPAM.Config {
		ip := net.ParseIP(value.Gateway)
		if ip.To4() != nil {
			gateway = value.Gateway
			break
		}
	}

	return d.setupNetworkState(id, network.Options, gateway)
}

func (d *Driver) CreateNetwork(r *dknet.CreateNetworkRequest) error {
	log.Debugf("Create network request: %+v", r)
	// FIXME
	_, err := d.setupNetworkState(r.NetworkID, stringOptions(r), r.IPv4Data[0].Gateway)
	return err
}

// By bboreham from https://github.com/weaveworks/weave/
//
// Deal with excessively-generic way the options get decoded from JSON
func stringOptions(create *dknet.CreateNetworkRequest) map[string]string {
	if create.Options != nil {
		if data, found := create.Options[optionKey]; found {
			if options, ok := data.(map[string]interface{}); ok {
				out := make(map[string]string, len(options))
				for key, value := range options {
					if str, ok := value.(string); ok {
						out[key] = str
					}
				}
				return out
			}
		}
	}
	return nil
}

func (d *Driver) setupNetworkState(id string, options map[string]string, gateway string) (*NetworkState, error) {
	bridgeName, err := getBridgeName(id, options)
	if err != nil {
		return nil, err
	}

	mtu, err := getBridgeMTU(options)
	if err != nil {
		return nil, err
	}

	mode, err := getBridgeMode(options)
	if err != nil {
		return nil, err
	}

	// FIXME
	mask := "24"

/*
	gateway, mask, err := getGatewayIP(options)
	if err != nil {
		return nil, err
	}
*/

	bindInterface, err := getBindInterface(options)
	if err != nil {
		return nil, err
	}

	ns := &NetworkState{
		BridgeName:        bridgeName,
		MTU:               mtu,
		Mode:              mode,
		Gateway:           gateway,
		GatewayMask:       mask,
		FlatBindInterface: bindInterface,
	}
	d.networks[id] = ns

	log.Debugf("Initializing bridge for network %s", id)
	if err := d.initBridge(id); err != nil {
		delete(d.networks, id)
		return nil, err
	}
	return ns, nil
}

func (d *Driver) DeleteNetwork(r *dknet.DeleteNetworkRequest) error {
	log.Debugf("Delete network request: %+v", r)
	bridgeName := d.networks[r.NetworkID].BridgeName
	log.Debugf("Deleting Bridge %s", bridgeName)
	err := d.deleteBridge(bridgeName)
	if err != nil {
		log.Errorf("Deleting bridge %s failed: %s", bridgeName, err)
		return err
	}
	delete(d.networks, r.NetworkID)
	return nil
}

func (d *Driver) CreateEndpoint(r *dknet.CreateEndpointRequest) (*dknet.CreateEndpointResponse, error) {
	log.Debugf("Create endpoint request: %+v", r)
	return nil, nil
}

func (d *Driver) DeleteEndpoint(r *dknet.DeleteEndpointRequest) error {
	log.Debugf("Delete endpoint request: %+v", r)
	return nil
}

func (d *Driver) EndpointInfo(r *dknet.InfoRequest) (*dknet.InfoResponse, error) {
	res := &dknet.InfoResponse{
		Value: make(map[string]string),
	}
	return res, nil
}

func (d *Driver) Join(r *dknet.JoinRequest) (*dknet.JoinResponse, error) {
	// create and attach local name to the bridge
	localVethPair := vethPair(truncateID(r.EndpointID))
	if err := netlink.LinkAdd(localVethPair); err != nil {
		log.Errorf("failed to create the veth pair named: [ %v ] error: [ %s ] ", localVethPair, err)
		return nil, err
	}
	// Bring the veth pair up
	err := netlink.LinkSetUp(localVethPair)
	if err != nil {
		log.Warnf("Error enabling  Veth local iface: [ %v ]", localVethPair)
		return nil, err
	}

	ns, err := d.findNetworkState(r.NetworkID)
	if err != nil {
		log.Errorf("no network state [ %s ]", r.NetworkID)
		return nil, err
	}
	bridgeName := ns.BridgeName
	err = d.addOvsVethPort(bridgeName, localVethPair.Name, 0)
	if err != nil {
		log.Errorf("error attaching veth [ %s ] to bridge [ %s ]", localVethPair.Name, bridgeName)
		return nil, err
	}
	log.Infof("Attached veth [ %s ] to bridge [ %s ]", localVethPair.Name, bridgeName)

	// SrcName gets renamed to DstPrefix + ID on the container iface
	res := &dknet.JoinResponse{
		InterfaceName: dknet.InterfaceName{
			SrcName:   localVethPair.PeerName,
			DstPrefix: containerEthName,
		},
		Gateway: ns.Gateway,
	}
	log.Debugf("Join endpoint %s:%s to %s", r.NetworkID, r.EndpointID, r.SandboxKey)
	return res, nil
}

func (d *Driver) Leave(r *dknet.LeaveRequest) error {
	log.Debugf("Leave request: %+v", r)
	localVethPair := vethPair(truncateID(r.EndpointID))
	if err := netlink.LinkDel(localVethPair); err != nil {
		log.Errorf("unable to delete veth on leave: %s", err)
	}
	portID := fmt.Sprintf(ovsPortPrefix + truncateID(r.EndpointID))
	bridgeName := d.networks[r.NetworkID].BridgeName
	err := d.ovsdber.deletePort(bridgeName, portID)
	if err != nil {
		log.Errorf("OVS port [ %s ] delete transaction failed on bridge [ %s ] due to: %s", portID, bridgeName, err)
		return err
	}
	log.Infof("Deleted OVS port [ %s ] from bridge [ %s ]", portID, bridgeName)
	log.Debugf("Leave %s:%s", r.NetworkID, r.EndpointID)
	return nil
}

func (d *Driver) ProgramExternalConnectivity(r *dknet.ProgramExternalConnectivityRequest) error {
	log.Debugf("Program external connectivity request: %+v", r)
	return nil
}

func (d *Driver) RevokeExternalConnectivity(r *dknet.RevokeExternalConnectivityRequest) error {
	log.Debugf("Revoke external connectivity request: %+v", r)
	return nil
}

func NewDriver(name string) (*Driver, error) {
	docker, err := dockerclient.NewDockerClient("unix:///var/run/docker.sock", nil)
	if err != nil {
		return nil, fmt.Errorf("could not connect to docker: %s", err)
	}

	// initiate the ovsdb manager port binding
	var ovsdb *libovsdb.OvsdbClient
	retries := 3
	for i := 0; i < retries; i++ {
		ovsdb, err = libovsdb.Connect(localhost, ovsdbPort)
		if err == nil {
			break
		}
		log.Errorf("could not connect to openvswitch on port [ %d ]: %s. Retrying in 5 seconds", ovsdbPort, err)
		time.Sleep(5 * time.Second)
	}

	if ovsdb == nil {
		return nil, fmt.Errorf("could not connect to open vswitch")
	}

	d := &Driver{
		name: name,
		dockerer: dockerer{
			client: docker,
		},
		ovsdber: ovsdber{
			ovsdb: ovsdb,
		},
		networks: make(map[string]*NetworkState),
	}
	// Initialize ovsdb cache at rpc connection setup
	d.ovsdber.initDBCache()
	return d, nil
}

// Create veth pair. Peername is renamed to eth0 in the container
func vethPair(suffix string) *netlink.Veth {
	return &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: ovsPortPrefix + suffix},
		PeerName:  "ethc" + suffix,
	}
}

// Enable a netlink interface
func interfaceUp(name string) error {
	iface, err := netlink.LinkByName(name)
	if err != nil {
		log.Debugf("Error retrieving a link named [ %s ]", iface.Attrs().Name)
		return err
	}
	return netlink.LinkSetUp(iface)
}

func truncateID(id string) string {
	return id[:5]
}

func getBridgeMTU(options map[string]string) (int, error) {
	bridgeMTU := defaultMTU
	if mtu, err := strconv.Atoi(options[mtuOption]); err == nil {
		bridgeMTU = mtu
	}
	return bridgeMTU, nil
}

func getBridgeName(id string, options map[string]string) (string, error) {
	bridgeName := bridgePrefix + truncateID(id)
	if name, ok := options[bridgeNameOption]; ok {
		bridgeName = name
	}
	return bridgeName, nil
}

func getBridgeMode(options map[string]string) (string, error) {
	bridgeMode := defaultMode
	if mode, ok := options[modeOption]; ok {
		if _, isValid := validModes[mode]; !isValid {
			return "", fmt.Errorf("%s is not a valid mode", mode)
		}
		bridgeMode = mode
	}
	return bridgeMode, nil
}

func getGatewayIP(options map[string]string) (string, string, error) {
/*
	// FIXME: Dear future self, I'm sorry for leaving you with this mess, but I want to get this working ASAP
	// This should be an array
	// We need to handle case where we have
	// a. v6 and v4 - dual stack
	// auxilliary address
	// multiple subnets on one network
	// also in that case, we'll need a function to determine the correct default gateway based on it's IP/Mask
	var gatewayIP string

	if len(r.IPv6Data) > 0 {
		if r.IPv6Data[0] != nil {
			if r.IPv6Data[0].Gateway != "" {
				gatewayIP = r.IPv6Data[0].Gateway
			}
		}
	}
	// Assumption: IPAM will provide either IPv4 OR IPv6 but not both
	// We may want to modify this in future to support dual stack
	if len(r.IPv4Data) > 0 {
		if r.IPv4Data[0] != nil {
			if r.IPv4Data[0].Gateway != "" {
				gatewayIP = r.IPv4Data[0].Gateway
			}
		}
	}

	if gatewayIP == "" {
		return "", "", fmt.Errorf("No gateway IP found")
	}
	parts := strings.Split(gatewayIP, "/")
	if parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("Cannot split gateway IP address")
	}
	return parts[0], parts[1], nil
*/
	return "", "", fmt.Errorf("FIXME")
}

func getBindInterface(options map[string]string) (string, error) {
	if mode, ok := options[bindInterfaceOption]; ok {
		return mode, nil
	}
	// As bind interface is optional and has no default, don't return an error
	return "", nil
}
