package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"

	consts "github.com/gardener/gardener-extension-private-network/pkg/constants"
	"github.com/gardener/gardener-extension-private-network/pkg/extensionspec"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/flavors"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/loadbalancers"
	v2monitors "github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/monitors"
	v2pools "github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/pools"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/portforwarding"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/subnets"
	"github.com/gophercloud/gophercloud/pagination"
	"gopkg.in/godo.v2/glob"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
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
		Duration: consts.WaitLoadbalancerInitDelay,
		Factor:   consts.WaitLoadbalancerFactor,
		Steps:    consts.Steps,
	}

	var loadbalancer *loadbalancers.LoadBalancer
	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		klog.Infof("Load balancer lbID %s", loadbalancerID)
		var err error
		loadbalancer, err = loadbalancers.Get(client, loadbalancerID).Extract()
		if err != nil {
			return false, err
		}
		if loadbalancer.ProvisioningStatus == consts.ProvisioningActiveStatus {
			klog.Infof("Load balancer ACTIVE lbID %s", loadbalancerID)
			return true, nil
		} else if loadbalancer.ProvisioningStatus == consts.ProvisioningErrorStatus {
			return true, fmt.Errorf("Loadbalancer [ID=%s] has gone into ERROR state", loadbalancerID)
		} else {
			return false, nil
		}

	})

	if wait.Interrupted(err) {
		err = fmt.Errorf("timeout waiting for the loadbalancer %s reach %s state ", loadbalancerID, consts.ProvisioningActiveStatus)
	}

	return loadbalancer, err
}

func findSubnetByCIDR(clientNetwork *gophercloud.ServiceClient, networkConfig Networks) (*subnets.Subnet, error) {
	// List all subnets
	allPages, err := subnets.List(clientNetwork, nil).AllPages()
	if err != nil {
		return nil, fmt.Errorf("Failed to list subnets: %v", err)
	}
	// Extract subnets
	allSubnets, err := subnets.ExtractSubnets(allPages)
	if err != nil {
		return nil, fmt.Errorf("Failed to extract subnets: %v", err)
	}

	// Find the subnet ID for the given network ID
	for _, subnet := range allSubnets {
		if subnet.NetworkID == networkConfig.ID && subnet.CIDR == networkConfig.Workers {
			fmt.Printf("Found subnet ID: %s\n", subnet.ID)
			return &subnet, nil
		}
	}
	return nil, fmt.Errorf("unable to find the match subnet for network ID = %s - %s", networkConfig.ID, networkConfig.Workers)
}
func CreateLoadBalancer(ctx context.Context,
	config *PrivateNetworkConfig,
	extension *extensionsv1alpha1.Extension,
	istioVIP []string,
	nameLoadbalancer string,
	lbSourceRangesIPv4 IPNet) (*loadbalancers.LoadBalancer, error) {
	provider, err := InitialClientOpenstack(config)
	if err != nil {
		return nil, err
	}
	// Initialize the networking client
	clientNetwork, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{
		Region: config.AuthOpt.Region, // Replace with your region
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create network client: %v", err)
	}
	// Initialize the Loadbalancer client
	clientLB, err := openstack.NewLoadBalancerV2(provider, gophercloud.EndpointOpts{
		Region: config.AuthOpt.Region, // Replace with your OpenStack region
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create network client: %v", err)
	}
	subnetWorker, err := findSubnetByCIDR(clientNetwork, config.WorkerNetwork)
	if err != nil {
		return nil, err
	}
	flavorLBID, err := getFlavorIDByType(clientLB, config.FlavorType)
	if err != nil {
		return nil, err
	}
	createOpts := loadbalancers.CreateOpts{
		Name:         nameLoadbalancer,
		Description:  fmt.Sprintf("Loadbalancer for private network cluster"),
		Provider:     "amphora",
		FlavorID:     flavorLBID,
		VipNetworkID: config.WorkerNetwork.ID,
		VipSubnetID:  subnetWorker.ID,
	}
	listener_443 := buildListeners(nameLoadbalancer, config.IstioSubnetID, istioVIP, lbSourceRangesIPv4, 443)
	listener_8443 := buildListeners(nameLoadbalancer, config.IstioSubnetID, istioVIP, lbSourceRangesIPv4, 8443)
	listener_8132 := buildListeners(nameLoadbalancer, config.IstioSubnetID, istioVIP, lbSourceRangesIPv4, 8132)
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

func buildListeners(name, poolMemberSubnetID string, vipLBistio []string, lbSourceRangesIPv4 IPNet, protocolPort int) listeners.CreateOpts {
	listenerCreateOpt := listeners.CreateOpts{
		Name:         fmt.Sprintf("%s-%d", name, protocolPort),
		Protocol:     listeners.Protocol("TCP"),
		ProtocolPort: protocolPort,
		AllowedCIDRs: lbSourceRangesIPv4.StringSlice(),
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
		IdentityEndpoint: config.AuthOpt.Endpoint,
		Username:         config.AuthOpt.Username,
		Password:         config.AuthOpt.Password,
		DomainName:       config.AuthOpt.DomainName,
		TenantID:         config.AuthOpt.TenantID,
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
		Region: config.AuthOpt.Region,
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
		Region: config.AuthOpt.Region,
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
		Region: config.AuthOpt.Region,
	})
	if err != nil {
		return fmt.Errorf("failed to create network client: %v", err)
	}
	if loadbalancer.ProvisioningStatus != consts.ProvisioningActiveStatus && loadbalancer.ProvisioningStatus != consts.ProvisioningErrorStatus {
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
		Duration: consts.WaitLoadbalancerInitDelay,
		Factor:   consts.WaitLoadbalancerFactor,
		Steps:    consts.WaitLoadbalancerDeleteSteps,
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

// AttachFloatingIP associate the suitable floatingIP for Loadbalancer and return this fip and error
func AttachFloatingIP(lb *loadbalancers.LoadBalancer,
	config *PrivateNetworkConfig,
	ex *extensionsv1alpha1.Extension) (string, error) {
	provider, err := InitialClientOpenstack(config)
	if err != nil {
		return "", err
	}
	// Initialize the networking client
	clientNetwork, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{
		Region: config.AuthOpt.Region, // Replace with your region
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
		klog.Infof("Checking floating IP for loadbalancer %s in floating pool name %s", lb.ID, config.FloatingPoolName)
		networkLB, err := networks.Get(clientNetwork, config.WorkerNetwork.ID).Extract()
		if err != nil {
			return "", fmt.Errorf("failed to get network of Loadbalancer for finding floating ip with ID '%s': %v", config.WorkerNetwork.ID, err)
		}
		FloatingNetwork, err := getNetworkByName(clientNetwork, config.FloatingPoolName)
		if err != nil {
			return "", err
		}
		opts := floatingips.ListOpts{
			FloatingNetworkID: FloatingNetwork.ID,
			Status:            "DOWN",
			PortID:            "",
			ProjectID:         networkLB.ProjectID,
		}
		availableIPs, err := getFloatingIPs(clientNetwork, opts)
		if err != nil {
			return "", fmt.Errorf("failed when trying to get available floating IP in pool %s, error: %v", FloatingNetwork.ID, err)
		}
		if len(availableIPs) > 0 {
			for _, fip := range availableIPs {
				allPages, err := portforwarding.List(clientNetwork, portforwarding.ListOpts{}, fip.ID).AllPages()
				if err != nil {
					return "", fmt.Errorf("failed when trying to list port forwarding of floating IP in pool %s, error: %v", FloatingNetwork.ID, err)
				}

				allPFs, err := portforwarding.ExtractPortForwardings(allPages)
				if err != nil {
					return "", err
				}
				if len(fip.PortID) == 0 && len(allPFs) == 0 { // Checking fip has no port forwarding to available for associate to LB
					fip.Description = fmt.Sprintf("Floating IP for Private Kubernetes Cluster service from cluster %s", ex.Namespace)
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
				FloatingNetworkID: FloatingNetwork.ID,
				PortID:            portID,
				Description:       fmt.Sprintf("Floating IP for Private Kubernetes Cluster service from cluster %s", ex.Namespace),
				ProjectID:         networkLB.ProjectID,
			}
			var foundSubnet subnets.Subnet
			// tweak list options for tags
			foundSubnets, err := lbPublicSubnetSpec.ListSubnetsForNetwork(clientNetwork, FloatingNetwork.ID)
			if err != nil {
				return "", err
			}
			if len(foundSubnets) == 0 {
				return "", fmt.Errorf("no subnet matching found for network %s", FloatingNetwork.ID)
			}

			// try to create floating IP in matching subnets (tags already filtered by list options)
			klog.V(4).Infof("found %d subnets matching for network %s", len(foundSubnets), FloatingNetwork.ID)
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
				return "", fmt.Errorf("no free subnet matching found for network %s (last error %s)", FloatingNetwork.ID, err)
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

// createFloatingIP create a new floating ip in VPC with specified CreateOpts
func createFloatingIP(netClient *gophercloud.ServiceClient, msg string, floatIPOpts floatingips.CreateOpts) (*floatingips.FloatingIP, error) {
	klog.Infof("%s floating ip with opts %+v", msg, floatIPOpts)
	floatIP, err := floatingips.Create(netClient, floatIPOpts).Extract()
	if err != nil {
		return floatIP, fmt.Errorf("error creating LB floatingip: %s", err)
	}
	return floatIP, err
}

// getNetworkByName get the network resource in openstack by name of network, used for case to find provider-network by name
func getNetworkByName(netClient *gophercloud.ServiceClient, name string) (*networks.Network, error) {
	allPages, err := networks.List(netClient, networks.ListOpts{Name: name}).AllPages()
	if err != nil {
		return nil, fmt.Errorf("failed to list networks: %v", err)
	}

	allNetworks, err := networks.ExtractNetworks(allPages)
	if err != nil {
		return nil, fmt.Errorf("failed to extract networks: %v", err)
	}

	if len(allNetworks) == 0 {
		return nil, fmt.Errorf("not found network name %s", name)
	}
	return &allNetworks[0], nil
}

func CheckLoadBalancer(ctx context.Context,
	lb *loadbalancers.LoadBalancer,
	config *PrivateNetworkConfig,
	allowRangeCIDRs IPNet) (*loadbalancers.LoadBalancer, error) {
	provider, err := InitialClientOpenstack(config)
	if err != nil {
		return lb, err
	}
	// Initialize the loadbalancer client
	clientLB, err := openstack.NewLoadBalancerV2(provider, gophercloud.EndpointOpts{
		Region: config.AuthOpt.Region, // Replace with your region
	})
	if err != nil {
		return lb, fmt.Errorf("failed to create loadbalancer client: %v", err)
	}
	listenerAllowedCIDRs := allowRangeCIDRs.StringSlice()
	listerners := lb.Listeners
	for _, listener := range listerners {
		listenerCheck, err := GetListenerByName(clientLB, listener.Name, lb.ID)
		if err != nil {
			return lb, fmt.Errorf("failed to get listener [name=%s] in LB [name=%s]: %v", listener.Name, lb.Name, err)
		}
		if len(listenerAllowedCIDRs) > 0 && !reflect.DeepEqual(listenerCheck.AllowedCIDRs, listenerAllowedCIDRs) {
			klog.Infof("listener.AllowedCIDRs: %v - listenerAllowedCIDRs: %v", listenerCheck.AllowedCIDRs, listenerAllowedCIDRs)
			_, err := listeners.Update(clientLB, listenerCheck.ID, listeners.UpdateOpts{
				AllowedCIDRs: &listenerAllowedCIDRs,
			}).Extract()
			if err != nil {
				return lb, fmt.Errorf("failed to update listener allowed CIDRs: %v", err)
			}

			klog.Infof("listenerID: %s of LB [Name=%s] - listener allowed CIDRs updated", listenerCheck.ID, lb.Name)
		}
		lb, err = WaitActiveAndGetLoadBalancer(clientLB, lb.ID)
		if err != nil {
			return lb, fmt.Errorf("loadbalancer %s not in ACTIVE status after creating listener, error: %v", lb.ID, err)
		}
	}
	return lb, nil
}

func getFlavorIDByType(clientLB *gophercloud.ServiceClient, typeName string) (string, error) {
	allPages, err := flavors.List(clientLB, nil).AllPages()
	if err != nil {
		return "", fmt.Errorf("Failed to list flavors: %v", err)
	}

	allFlavors, err := flavors.ExtractFlavors(allPages)
	if err != nil {
		return "", fmt.Errorf("Failed to extract flavors: %v", err)
	}

	for _, flavor := range allFlavors {
		if flavor.Name == typeName {
			return flavor.ID, nil
		}
	}
	return "", fmt.Errorf("Not found flavor ID with type %s", typeName)
}

// GetListenerByName gets a listener by its name, raise error if not found or get multiple ones.
func GetListenerByName(client *gophercloud.ServiceClient, name string, lbID string) (*listeners.Listener, error) {
	opts := listeners.ListOpts{
		Name:           name,
		LoadbalancerID: lbID,
	}
	pager := listeners.List(client, opts)
	var listenerList []listeners.Listener

	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		v, err := listeners.ExtractListeners(page)
		if err != nil {
			return false, err
		}
		listenerList = append(listenerList, v...)
		if len(listenerList) > 1 {
			return false, consts.ErrMultipleResults
		}
		return true, nil
	})
	if err != nil {
		if IsNotFound(err) {
			return nil, consts.ErrNotFound
		}
		return nil, err
	}

	if len(listenerList) == 0 {
		return nil, consts.ErrNotFound
	}

	return &listenerList[0], nil
}
