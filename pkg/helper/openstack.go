package helper

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/loadbalancers"
	v2monitors "github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/monitors"
	v2pools "github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/pools"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/subnets"
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
		return nil, fmt.Errorf("Failed to create network client: %v", err)
	}
	// Initialize the Loadbalancer client
	clientLB, err := openstack.NewLoadBalancerV2(provider, gophercloud.EndpointOpts{
		Region: config.Region, // Replace with your OpenStack region
	})
	if err != nil {
		return nil, fmt.Errorf("Failed to create network client: %v", err)
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
	listener_443 := buildListeners(nameLoadbalancer, workerSubnetID, istioVIP, 443)
	listener_8443 := buildListeners(nameLoadbalancer, workerSubnetID, istioVIP, 8443)
	listener_8132 := buildListeners(nameLoadbalancer, workerSubnetID, istioVIP, 8132)
	createOpts.Listeners = append(createOpts.Listeners, listener_443, listener_8443, listener_8132)
	lb, err := loadbalancers.Create(clientLB, createOpts).Extract()
	if err != nil {
		return nil, err
	}
	lb, err = WaitActiveAndGetLoadBalancer(clientLB, lb.ID)
	if err != nil {
		return nil, fmt.Errorf("Error creating Loadbalancer for Private Network: [%v]", err)
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
