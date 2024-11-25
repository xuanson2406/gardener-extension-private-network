package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gardener/gardener-extension-private-network/pkg/extensionspec"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/loadbalancers"
	v2monitors "github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/monitors"
	v2pools "github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/pools"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/portforwarding"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/subnets"
	"gopkg.in/godo.v2/glob"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

const (
	OctaviaFeatureTimeout = 3

	waitLoadbalancerInitDelay   = 1 * time.Second
	waitLoadbalancerFactor      = 1.2
	waitLoadbalancerActiveSteps = 25
	waitLoadbalancerDeleteSteps = 12
	steps                       = 25

	activeStatus = "ACTIVE"
	errorStatus  = "ERROR"
)

// ErrNotFound is used to inform that the object is missing
var ErrNotFound = errors.New("failed to find object")

// ErrMultipleResults is used when we unexpectedly get back multiple results
var ErrMultipleResults = errors.New("multiple results where only one expected")

// floatingSubnetSpec contains the specification of the public subnet to use for
// a public network. If given it may either describe the subnet id or
// a subnet name pattern for the subnet to use. If a pattern is given
// the first subnet matching the name pattern with an allocatable floating ip
// will be selected.
type floatingSubnetSpec struct {
	subnetID   string
	subnet     string
	subnetTags string
}

// TweakSubNetListOpsFunction is used to modify List Options for subnets
type TweakSubNetListOpsFunction func(*subnets.ListOpts)

// matcher matches a subnet
type matcher func(subnet *subnets.Subnet) bool

func andMatcher(a, b matcher) matcher {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return func(s *subnets.Subnet) bool {
		return a(s) && b(s)
	}
}

// reexpNameMatcher creates a subnet matcher matching a subnet by name for a given regexp.
func regexpNameMatcher(r *regexp.Regexp) matcher {
	return func(s *subnets.Subnet) bool { return r.FindString(s.Name) == s.Name }
}

// subnetNameMatcher creates a subnet matcher matching a subnet by name for a given glob
// or regexp
func subnetNameMatcher(pat string) (matcher, error) {
	// try to create floating IP in matching subnets
	var match matcher
	not := false
	if strings.HasPrefix(pat, "!") {
		not = true
		pat = pat[1:]
	}
	if strings.HasPrefix(pat, "~") {
		rexp, err := regexp.Compile(pat[1:])
		if err != nil {
			return nil, fmt.Errorf("invalid subnet regexp pattern %q: %s", pat[1:], err)
		}
		match = regexpNameMatcher(rexp)
	} else {
		match = regexpNameMatcher(glob.Globexp(pat))
	}
	if not {
		match = negate(match)
	}
	return match, nil
}

// negate returns a negated matches for a given one
func negate(f matcher) matcher { return func(s *subnets.Subnet) bool { return !f(s) } }

// subnetTagMatcher matches a subnet by a given tag spec
func subnetTagMatcher(tags string) matcher {
	// try to create floating IP in matching subnets
	var match matcher

	list, not, all := tagList(tags)

	match = func(s *subnets.Subnet) bool {
		for _, tag := range list {
			found := false
			for _, t := range s.Tags {
				if t == tag {
					found = true
					break
				}
			}
			if found {
				if !all {
					return !not
				}
			} else {
				if all {
					return not
				}
			}
		}
		return not != all
	}
	return match
}

func (s *floatingSubnetSpec) Configured() bool {
	if s != nil && (s.subnetID != "" || s.MatcherConfigured()) {
		return true
	}
	return false
}

func (s *floatingSubnetSpec) ListSubnetsForNetwork(netClient *gophercloud.ServiceClient, networkID string) ([]subnets.Subnet, error) {
	matcher, err := s.Matcher(false)
	if err != nil {
		return nil, err
	}
	list, err := listSubnetsForNetwork(netClient, networkID, s.tweakListOpts)
	if err != nil {
		return nil, err
	}
	if matcher == nil {
		return list, nil
	}

	// filter subnets according to spec
	var foundSubnets []subnets.Subnet
	for _, subnet := range list {
		if matcher(&subnet) {
			foundSubnets = append(foundSubnets, subnet)
		}
	}
	return foundSubnets, nil
}

// tweakListOpts can be used to optimize a subnet list query for the
// actually described subnet filter
func (s *floatingSubnetSpec) tweakListOpts(opts *subnets.ListOpts) {
	if s.subnetTags != "" {
		list, not, all := tagList(s.subnetTags)
		tags := strings.Join(list, ",")
		if all {
			if not {
				opts.NotTagsAny = tags // at least one tag must be missing
			} else {
				opts.Tags = tags // all tags must be present
			}
		} else {
			if not {
				opts.NotTags = tags // none of the tags are present
			} else {
				opts.TagsAny = tags // at least one tag is present
			}
		}
	}
}

func (s *floatingSubnetSpec) MatcherConfigured() bool {
	if s != nil && s.subnetID == "" && (s.subnet != "" || s.subnetTags != "") {
		return true
	}
	return false
}

func addField(s, name, value string) string {
	if value == "" {
		return s
	}
	if s == "" {
		s += ", "
	}
	return fmt.Sprintf("%s%s: %q", s, name, value)
}

func (s *floatingSubnetSpec) String() string {
	if s == nil || (s.subnetID == "" && s.subnet == "" && s.subnetTags == "") {
		return "<none>"
	}
	pat := addField("", "subnetID", s.subnetID)
	pat = addField(pat, "pattern", s.subnet)
	return addField(pat, "tags", s.subnetTags)
}

func (s *floatingSubnetSpec) Matcher(tag bool) (matcher, error) {
	if !s.MatcherConfigured() {
		return nil, nil
	}
	var match matcher
	var err error
	if s.subnet != "" {
		match, err = subnetNameMatcher(s.subnet)
		if err != nil {
			return nil, err
		}
	}
	if tag && s.subnetTags != "" {
		match = andMatcher(match, subnetTagMatcher(s.subnetTags))
	}
	if match == nil {
		match = func(s *subnets.Subnet) bool { return true }
	}
	return match, nil
}

func tagList(tags string) ([]string, bool, bool) {
	not := strings.HasPrefix(tags, "!")
	if not {
		tags = tags[1:]
	}
	all := strings.HasPrefix(tags, "&")
	if all {
		tags = tags[1:]
	}
	list := strings.Split(tags, ",")
	for i := range list {
		list[i] = strings.TrimSpace(list[i])
	}
	return list, not, all
}

func listSubnetsForNetwork(netClient *gophercloud.ServiceClient, networkID string, tweak ...TweakSubNetListOpsFunction) ([]subnets.Subnet, error) {
	var opts = subnets.ListOpts{NetworkID: networkID}
	for _, f := range tweak {
		if f != nil {
			f(&opts)
		}
	}
	allPages, err := subnets.List(netClient, opts).AllPages()
	if err != nil {
		return nil, fmt.Errorf("error listing subnets of network %s: %v", networkID, err)
	}
	subs, err := subnets.ExtractSubnets(allPages)
	if err != nil {
		return nil, fmt.Errorf("error extracting subnets from pages: %v", err)
	}

	if len(subs) == 0 {
		return nil, fmt.Errorf("could not find subnets for network %s", networkID)
	}
	return subs, nil
}

// WaitActiveAndGetLoadBalancer wait for LB active then return the LB object for further usage
func WaitActiveAndGetLoadBalancer(client *gophercloud.ServiceClient, loadbalancerID string) (*loadbalancers.LoadBalancer, error) {
	fmt.Printf("Waiting for load balancer ACTIVE - lb %s", loadbalancerID)
	backoff := wait.Backoff{
		Duration: waitLoadbalancerInitDelay,
		Factor:   waitLoadbalancerFactor,
		Steps:    steps,
	}

	var loadbalancer *loadbalancers.LoadBalancer
	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		fmt.Printf("Load balancer lbID %s", loadbalancerID)
		var err error
		loadbalancer, err = loadbalancers.Get(client, loadbalancerID).Extract()
		if err != nil {
			return false, err
		}
		if loadbalancer.ProvisioningStatus == activeStatus {
			fmt.Printf("Load balancer ACTIVE lbID %s", loadbalancerID)
			return true, nil
		} else if loadbalancer.ProvisioningStatus == errorStatus {
			return true, fmt.Errorf("loadbalancer has gone into ERROR state")
		} else {
			return false, nil
		}

	})

	if wait.Interrupted(err) {
		err = fmt.Errorf("timeout waiting for the loadbalancer %s %s", loadbalancerID, activeStatus)
	}

	return loadbalancer, err
}

func CreateLoadBalancer(ctx context.Context,
	config *PrivateNetworkConfig,
	extension *extensionsv1alpha1.Extension,
	istioVIP []string,
	nameLoadbalancer string) (*loadbalancers.LoadBalancer, error) {
	var workerSubnetID string
	provider, err := InitialClientOpenstack(config)
	if err != nil {
		return nil, err
	}
	// Initialize the networking client
	clientNetwork, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{
		Region: config.Region, // Replace with your region
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create network client: %v", err)
	}
	// Initialize the Loadbalancer client
	clientLB, err := openstack.NewLoadBalancerV2(provider, gophercloud.EndpointOpts{
		Region: config.Region, // Replace with your OpenStack region
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create network client: %v", err)
	}
	// List all subnets
	allPages, err := subnets.List(clientNetwork, nil).AllPages()
	if err != nil {
		log.Fatalf("Failed to list subnets: %v", err)
	}
	// Extract subnets
	allSubnets, err := subnets.ExtractSubnets(allPages)
	if err != nil {
		log.Fatalf("Failed to extract subnets: %v", err)
	}

	// Find the subnet ID for the given network ID
	for _, subnet := range allSubnets {
		if subnet.NetworkID == config.WorkerNetworkID {
			fmt.Printf("Found subnet ID: %s\n", subnet.ID)
			workerSubnetID = subnet.ID
		}
	}
	createOpts := loadbalancers.CreateOpts{
		Name:         nameLoadbalancer,
		Description:  fmt.Sprintf("Loadbalancer for private network cluster"),
		Provider:     "amphora",
		FlavorID:     config.FlavorID,
		VipNetworkID: config.WorkerNetworkID,
		VipSubnetID:  workerSubnetID,
	}
	listener_443 := buildListeners(nameLoadbalancer, config.IstioSubnetID, istioVIP, 443)
	listener_8443 := buildListeners(nameLoadbalancer, config.IstioSubnetID, istioVIP, 8443)
	listener_8132 := buildListeners(nameLoadbalancer, config.IstioSubnetID, istioVIP, 8132)
	createOpts.Listeners = append(createOpts.Listeners, listener_443, listener_8443, listener_8132)
	lb, err := loadbalancers.Create(clientLB, createOpts).Extract()
	if err != nil {
		return nil, err
	}
	lb, err = WaitActiveAndGetLoadBalancer(clientLB, lb.ID)
	if err != nil {
		return nil, fmt.Errorf("error creating Loadbalancer for Private Network: [%v]", err)
	}

	return lb, nil
}

func buildListeners(name, poolMemberSubnetID string, vipLBistio []string, protocolPort int) listeners.CreateOpts {
	listenerCreateOpt := listeners.CreateOpts{
		Name:         fmt.Sprintf("%s-%d", name, protocolPort),
		Protocol:     listeners.Protocol("TCP"),
		ProtocolPort: protocolPort,
	}
	var members []v2pools.BatchUpdateMemberOpts
	for _, vip := range vipLBistio {
		member := v2pools.BatchUpdateMemberOpts{
			Address:      vip,
			ProtocolPort: protocolPort,
			Name:         nil,
			SubnetID:     &poolMemberSubnetID,
		}
		members = append(members, member)
	}
	poolCreateOpt := v2pools.CreateOpts{
		Name:     fmt.Sprintf("%s-%d", name, protocolPort),
		Protocol: v2pools.Protocol("TCP"),
		LBMethod: "ROUND_ROBIN",
	}
	poolCreateOpt.Members = members
	healthopts := v2monitors.CreateOpts{
		Name:           name,
		Type:           "TCP",
		Delay:          5,
		Timeout:        10,
		MaxRetries:     4,
		MaxRetriesDown: 3,
	}
	poolCreateOpt.Monitor = &healthopts
	listenerCreateOpt.DefaultPool = &poolCreateOpt
	return listenerCreateOpt
}

func InitialClientOpenstack(config *PrivateNetworkConfig) (*gophercloud.ProviderClient, error) {
	opts := gophercloud.AuthOptions{
		IdentityEndpoint: config.Endpoint,
		Username:         config.Username,
		Password:         config.Password,
		DomainName:       config.DomainName,
		TenantID:         config.TenantID,
	}

	provider, err := openstack.AuthenticatedClient(opts)
	if err != nil {
		return nil, fmt.Errorf("Failed to authenticate: %v", err)
	}
	return provider, nil
}

func GetFloatingIPLoadbalancer(config *PrivateNetworkConfig, lb *loadbalancers.LoadBalancer) (*string, error) {
	var fipLB *string
	provider, err := InitialClientOpenstack(config)
	if err != nil {
		return nil, fmt.Errorf("Failed to create provider for get Floating IP of LB [ID=%s]: [%v]", lb.ID, err)
	}
	networkClient, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{
		Region: config.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("Failed to create networking client: %v", err)
	}
	allPages, err := floatingips.List(networkClient, nil).AllPages()
	if err != nil {
		return nil, fmt.Errorf("Failed to list floating IPs: %v", err)
	}

	allFloatingIPs, err := floatingips.ExtractFloatingIPs(allPages)
	if err != nil {
		return nil, fmt.Errorf("Failed to extract floating IPs: %v", err)
	}

	for _, fip := range allFloatingIPs {
		if fip.FixedIP == lb.VipAddress {
			fipLB = &fip.FloatingIP
			break
		}
	}
	if fipLB == nil {
		return nil, fmt.Errorf("Unable to get floating IP of Loadbalancer [ID=%s]: NOT FOUND", lb.ID)
	}
	return fipLB, nil
}

func GetLoadbalancerByName(config *PrivateNetworkConfig, name string) (*loadbalancers.LoadBalancer, error) {
	provider, err := InitialClientOpenstack(config)
	if err != nil {
		return nil, err
	}
	// Initialize the Loadbalancer client
	clientLB, err := openstack.NewLoadBalancerV2(provider, gophercloud.EndpointOpts{
		Region: config.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create network client: %v", err)
	}
	return getLoadbalancerByName(clientLB, name)
}

// getLoadbalancerByName get the load balancer which is in valid status by the given name/legacy name.
func getLoadbalancerByName(client *gophercloud.ServiceClient, name string) (*loadbalancers.LoadBalancer, error) {
	var validLBs []loadbalancers.LoadBalancer

	opts := loadbalancers.ListOpts{
		Name: name,
	}
	allLoadbalancers, err := GetLoadBalancers(client, opts)
	if err != nil {
		return nil, err
	}

	for _, lb := range allLoadbalancers {
		if lb.ProvisioningStatus != "DELETED" && lb.ProvisioningStatus != "PENDING_DELETE" {
			validLBs = append(validLBs, lb)
		}
	}

	if len(validLBs) > 1 {
		return nil, ErrMultipleResults
	}
	if len(validLBs) == 0 {
		return nil, ErrNotFound
	}

	return &validLBs[0], nil
}

// GetLoadBalancers returns all the filtered load balancer.
func GetLoadBalancers(client *gophercloud.ServiceClient, opts loadbalancers.ListOpts) ([]loadbalancers.LoadBalancer, error) {
	allPages, err := loadbalancers.List(client, opts).AllPages()
	if err != nil {
		return nil, err
	}
	allLoadbalancers, err := loadbalancers.ExtractLoadBalancers(allPages)
	if err != nil {
		return nil, err
	}

	return allLoadbalancers, nil
}

// DeleteLoadbalancer deletes a loadbalancer and wait for it's gone.
func DeleteLoadbalancer(config *PrivateNetworkConfig, loadbalancer *loadbalancers.LoadBalancer, cascade bool) error {
	provider, err := InitialClientOpenstack(config)
	if err != nil {
		return err
	}
	// Initialize the Loadbalancer client
	clientLB, err := openstack.NewLoadBalancerV2(provider, gophercloud.EndpointOpts{
		Region: config.Region,
	})
	if err != nil {
		return fmt.Errorf("failed to create network client: %v", err)
	}
	if loadbalancer.ProvisioningStatus != activeStatus && loadbalancer.ProvisioningStatus != errorStatus {
		return fmt.Errorf("load balancer %s is in immutable status, current provisioning status: %s", loadbalancer.ID, loadbalancer.ProvisioningStatus)
	}

	return deleteLoadbalancer(clientLB, loadbalancer.ID, cascade)
}
func deleteLoadbalancer(client *gophercloud.ServiceClient, lbID string, cascade bool) error {
	opts := loadbalancers.DeleteOpts{}
	if cascade {
		opts.Cascade = true
	}

	err := loadbalancers.Delete(client, lbID, opts).ExtractErr()
	if err != nil && !IsNotFound(err) {
		return fmt.Errorf("error deleting loadbalancer %s: %v", lbID, err)
	}

	if err := waitLoadbalancerDeleted(client, lbID); err != nil {
		return err
	}

	return nil
}

func waitLoadbalancerDeleted(client *gophercloud.ServiceClient, loadbalancerID string) error {
	klog.V(4).InfoS("Waiting for load balancer deleted", "lbID", loadbalancerID)
	backoff := wait.Backoff{
		Duration: waitLoadbalancerInitDelay,
		Factor:   waitLoadbalancerFactor,
		Steps:    waitLoadbalancerDeleteSteps,
	}
	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		_, err := loadbalancers.Get(client, loadbalancerID).Extract()
		if err != nil {
			if IsNotFound(err) {
				klog.V(4).InfoS("Load balancer deleted", "lbID", loadbalancerID)
				return true, nil
			}
			return false, err
		}
		return false, nil
	})

	if wait.Interrupted(err) {
		err = fmt.Errorf("loadbalancer failed to delete within the allotted time")
	}

	return err
}

func IsNotFound(err error) bool {
	if err == ErrNotFound {
		return true
	}

	if _, ok := err.(gophercloud.ErrDefault404); ok {
		return true
	}

	if _, ok := err.(gophercloud.ErrResourceNotFound); ok {
		return true
	}

	if errCode, ok := err.(gophercloud.ErrUnexpectedResponseCode); ok {
		if errCode.Actual == http.StatusNotFound {
			return true
		}
	}

	return false
}

func AttachFloatingIP(lb *loadbalancers.LoadBalancer,
	config *PrivateNetworkConfig,
	ex *extensionsv1alpha1.Extension) (string, error) {
	provider, err := InitialClientOpenstack(config)
	if err != nil {
		return "", err
	}
	// Initialize the networking client
	clientNetwork, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{
		Region: config.Region, // Replace with your region
	})
	if err != nil {
		return "", fmt.Errorf("failed to create network client: %v", err)
	}
	portID := lb.VipPortID
	extSpec := &extensionspec.ExtensionSpec{}
	if ex.Spec.ProviderConfig != nil && ex.Spec.ProviderConfig.Raw != nil {
		if err := json.Unmarshal(ex.Spec.ProviderConfig.Raw, &extSpec); err != nil {
			return "", err
		}
	}
	floatIP, err := getFloatingIPByPortID(clientNetwork, portID)
	if err != nil {
		return "", fmt.Errorf("failed when getting floating IP for port %s: %v", portID, err)
	}
	if floatIP != nil {
		klog.Infof("Found floating ip %v by loadbalancer port id %q", floatIP, portID)
	}
	if extSpec.PrivateCluster {
		if floatIP != nil {
			// if FIP wasn't deleted we should still detach it because we use private cluster
			_, err = updateFloatingIP(clientNetwork, floatIP, nil)
			if err != nil {
				return "", fmt.Errorf("erorr to detach fip from LB [ID=%s] private cluster: [%v]", lb.ID, err)
			}
		}
		return lb.VipAddress, nil
	}
	if floatIP == nil {
		klog.Infof("Checking floating IP for loadbalancer %s in poolID %s", lb.ID, config.FloatingNetworkId)
		networkLB, err := networks.Get(clientNetwork, config.WorkerNetworkID).Extract()
		if err != nil {
			return "", fmt.Errorf("failed to get network of Loadbalancer for finding floating ip with ID '%s': %v", config.WorkerNetworkID, err)
		}
		opts := floatingips.ListOpts{
			FloatingNetworkID: config.FloatingNetworkId,
			Status:            "DOWN",
			PortID:            "",
			ProjectID:         networkLB.ProjectID,
		}
		availableIPs, err := getFloatingIPs(clientNetwork, opts)
		if err != nil {
			return "", fmt.Errorf("failed when trying to get available floating IP in pool %s, error: %v", config.FloatingNetworkId, err)
		}
		if len(availableIPs) > 0 {
			for _, fip := range availableIPs {
				allPages, err := portforwarding.List(clientNetwork, portforwarding.ListOpts{}, fip.ID).AllPages()
				if err != nil {
					panic(err)
				}

				allPFs, err := portforwarding.ExtractPortForwardings(allPages)
				if err != nil {
					panic(err)
				}
				if len(fip.PortID) == 0 && len(allPFs) == 0 { // Checking fip has no port forwarding to available for associate to LB
					floatIP, err = updateFloatingIP(clientNetwork, &fip, &lb.VipPortID)
					if err != nil {
						return "", err
					}
				}
			}
		}
		if floatIP == nil {
			lbPublicSubnetSpec := &floatingSubnetSpec{
				subnetID:   "",
				subnet:     "",
				subnetTags: "",
			}
			klog.V(2).Infof("Creating floating IP for loadbalancer %s", lb.ID)
			floatIPOpts := floatingips.CreateOpts{
				FloatingNetworkID: config.FloatingNetworkId,
				PortID:            portID,
				Description:       fmt.Sprintf("Floating IP for Private Kubernetes Cluster service from cluster %s", ex.Namespace),
				ProjectID:         networkLB.ProjectID,
			}
			var foundSubnet subnets.Subnet
			// tweak list options for tags
			foundSubnets, err := lbPublicSubnetSpec.ListSubnetsForNetwork(clientNetwork, config.FloatingNetworkId)
			if err != nil {
				return "", err
			}
			if len(foundSubnets) == 0 {
				return "", fmt.Errorf("no subnet matching found for network %s",
					config.FloatingNetworkId)
			}

			// try to create floating IP in matching subnets (tags already filtered by list options)
			klog.V(4).Infof("found %d subnets matching for network %s", len(foundSubnets), config.FloatingNetworkId)
			for _, subnet := range foundSubnets {
				floatIPOpts.SubnetID = subnet.ID
				floatIP, err = createFloatingIP(clientNetwork, fmt.Sprintf("Trying subnet %s for creating", subnet.Name), floatIPOpts)
				if err == nil {
					foundSubnet = subnet
					break
				}
				klog.V(2).Infof("cannot use subnet %s: %s", subnet.Name, err)
			}
			if err != nil {
				return "", fmt.Errorf("no free subnet matching found for network %s (last error %s)", config.FloatingNetworkId, err)
			}
			klog.V(2).Infof("Successfully created floating IP %s for loadbalancer %s on subnet %s(%s)", floatIP.FloatingIP, lb.ID, foundSubnet.Name, foundSubnet.ID)
		}
	}
	if floatIP != nil {
		return floatIP.FloatingIP, nil
	}

	return lb.VipAddress, nil
}

// updateFloatingIP is used to attach/detach fip to/from Loadbalancer
func updateFloatingIP(c *gophercloud.ServiceClient, floatingip *floatingips.FloatingIP, portID *string) (*floatingips.FloatingIP, error) {
	floatUpdateOpts := floatingips.UpdateOpts{
		PortID: portID,
	}
	if portID != nil {
		klog.V(4).Infof("Attaching floating ip %q to loadbalancer port %q", floatingip.FloatingIP, portID)
	} else {
		klog.V(4).Infof("Detaching floating ip %q from port %q", floatingip.FloatingIP, floatingip.PortID)
	}
	floatingip, err := floatingips.Update(c, floatingip.ID, floatUpdateOpts).Extract()
	if err != nil {
		return nil, fmt.Errorf("error updating LB floatingip %+v: %v", floatUpdateOpts, err)
	}
	return floatingip, nil
}

// getFloatingIPs returns all the filtered floating IPs
func getFloatingIPs(client *gophercloud.ServiceClient, opts floatingips.ListOpts) ([]floatingips.FloatingIP, error) {
	var floatingIPList []floatingips.FloatingIP

	allPages, _ := floatingips.List(client, opts).AllPages()
	floatingIPList, err := floatingips.ExtractFloatingIPs(allPages)
	if err != nil {
		return floatingIPList, err
	}

	return floatingIPList, nil
}

// getFloatingIPByPortID get the floating IP of the given port.
func getFloatingIPByPortID(client *gophercloud.ServiceClient, portID string) (*floatingips.FloatingIP, error) {
	opt := floatingips.ListOpts{
		PortID: portID,
	}
	ips, err := getFloatingIPs(client, opt)
	if err != nil {
		return nil, err
	}

	if len(ips) == 0 {
		return nil, nil
	}

	return &ips[0], nil
}

func createFloatingIP(netClient *gophercloud.ServiceClient, msg string, floatIPOpts floatingips.CreateOpts) (*floatingips.FloatingIP, error) {
	klog.V(4).Infof("%s floating ip with opts %+v", msg, floatIPOpts)
	floatIP, err := floatingips.Create(netClient, floatIPOpts).Extract()
	if err != nil {
		return floatIP, fmt.Errorf("error creating LB floatingip: %s", err)
	}
	return floatIP, err
}
