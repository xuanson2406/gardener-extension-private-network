package helper

import (
	"context"
	"fmt"
	"log"
	"time"

	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/loadbalancers"
	v2monitors "github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/monitors"
	v2pools "github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/pools"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/subnets"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	OctaviaFeatureFlavors = 2
	OctaviaFeatureTimeout = 3

	waitLoadbalancerInitDelay   = 1 * time.Second
	waitLoadbalancerFactor      = 1.2
	waitLoadbalancerActiveSteps = 23
	waitLoadbalancerDeleteSteps = 12
	steps                       = 23

	activeStatus = "ACTIVE"
	errorStatus  = "ERROR"
)

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
	istioVIP []string) (*loadbalancers.LoadBalancer, error) {
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
	lbName := fmt.Sprintf("private-network-%s", extension.Namespace)
	createOpts := loadbalancers.CreateOpts{
		Name:         lbName,
		Description:  fmt.Sprintf("Loadbalancer for private network cluster"),
		Provider:     "amphora",
		FlavorID:     config.FlavorID,
		VipNetworkID: config.WorkerNetworkID,
		VipSubnetID:  workerSubnetID,
	}
	listener_443 := buildListeners(lbName, workerSubnetID, istioVIP, 443)
	listener_8443 := buildListeners(lbName, workerSubnetID, istioVIP, 8443)
	listener_8132 := buildListeners(lbName, workerSubnetID, istioVIP, 8132)
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
