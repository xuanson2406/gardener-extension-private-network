package helper

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gardener/gardener-extension-private-network/pkg/extensionspec"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"gopkg.in/gcfg.v1"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	secretConfig                        = "external-openstack-cloud-config"
	namespace                           = "kube-system"
	flavorType                          = "basic"
	defaultLoadBalancerSourceRangesIPv4 = "0.0.0.0/0"
)

type Global struct {
	AuthURL    string `gcfg:"auth-url"`
	Username   string `gcfg:"username"`
	Password   string `gcfg:"password"`
	Region     string `gcfg:"region"`
	TenantID   string `gcfg:"tenant-id"`
	DomainName string `gcfg:"domain-name"`
	TenantName string `gcfg:"tenant-name"`
}

type LoadBalancer struct {
	MonitorTimeout    string `gcfg:"monitor-timeout"`
	MonitorMaxRetries int    `gcfg:"monitor-max-retries"`
	NetworkID         string `gcfg:"network-id"`
	SubnetID          string `gcfg:"subnet-id"`
	FloatingNetworkID string `gcfg:"floating-network-id"`
	FloatingSubnetID  string `gcfg:"floating-subnet-id"`
	LBProvider        string `gcfg:"lb-provider"`
}

type Config struct {
	Global       Global
	LoadBalancer LoadBalancer
}

type NetworkWorker struct {
	Network          Networks `json:"networks"`
	FloatingPoolName string   `json:"floatingPoolName"`
}

type Networks struct {
	ID      string `json:"id"`
	Workers string `json:"workers"`
}

type AuthOpt struct {
	Endpoint   string
	Username   string
	Password   string
	DomainName string
	TenantID   string
	Region     string
}

type PrivateNetworkConfig struct {
	AuthOpt          AuthOpt
	WorkerNetwork    Networks
	IstioSubnetID    string
	FlavorType       string
	FloatingPoolName string
}

// GetInfrastructureForExtension returns Infrastructure object for an extension object
func GetGlobalConfigforPrivateNetwork(
	ctx context.Context,
	c client.Reader,
	extension *extensionsv1alpha1.Extension,
	shootName string,
) (*PrivateNetworkConfig, error) {

	cluster, err := GetClusterForExtension(ctx, c, extension)
	if err != nil {
		return nil, err
	}
	netSpec := NetworkWorker{}
	if cluster.Shoot.Spec.Provider.InfrastructureConfig != nil && cluster.Shoot.Spec.Provider.InfrastructureConfig.Raw != nil {
		if err := json.Unmarshal(cluster.Shoot.Spec.Provider.InfrastructureConfig.Raw, &netSpec); err != nil {
			return nil, err
		}
	}
	secret := &v1.Secret{}
	err = c.Get(ctx, client.ObjectKey{
		Namespace: namespace,
		Name:      secretConfig,
	}, secret)
	if err != nil {
		return nil, err
	}
	encodedData := secret.Data["cloud.conf"]
	if encodedData == nil {
		return nil, fmt.Errorf("cloud.conf key not found in the secret")
	}
	// Parse the decoded data into the struct
	var cfg Config
	_ = gcfg.ReadStringInto(&cfg, string(encodedData))
	// if err != nil {
	// 	return nil, err
	// }
	config := &PrivateNetworkConfig{
		AuthOpt: AuthOpt{
			Endpoint:   cfg.Global.AuthURL,
			Username:   cfg.Global.Username,
			Password:   cfg.Global.Password,
			DomainName: cfg.Global.DomainName,
			TenantID:   cfg.Global.TenantID,
			Region:     cfg.Global.Region,
		},
		WorkerNetwork:    netSpec.Network,
		IstioSubnetID:    cfg.LoadBalancer.SubnetID,
		FloatingPoolName: netSpec.FloatingPoolName,
		FlavorType:       flavorType,
	}
	return config, nil
}

// GetLoadBalancerSourceRanges get the list of Source Ranges IPv4 can be allowed access to Loadbalancer
func GetLoadBalancerSourceRanges(ctx context.Context, extSpec *extensionspec.ExtensionSpec, config *PrivateNetworkConfig) (IPNet, error) {
	var (
		ipnets IPNet
		err    error
		specs  []string
	)
	if extSpec.AlowCIDRs != nil {
		specs = extSpec.AlowCIDRs
		specs = append(specs, config.WorkerNetwork.Workers)
	} else {
		specs = append(specs, defaultLoadBalancerSourceRangesIPv4)
	}

	ipnets, err = ParseIPNets(specs...)

	if err != nil {
		return nil, fmt.Errorf("extension.Spec.allowCIDRs: %v is not valid. Expecting a list of IP ranges. For example, 10.0.0.0/24. Error msg: %v", specs, err)
	}
	return ipnets, nil
}
