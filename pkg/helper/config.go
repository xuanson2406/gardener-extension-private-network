package helper

import (
	"context"
	"encoding/json"
	"fmt"

	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"gopkg.in/gcfg.v1"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	secretConfig = "external-openstack-cloud-config"
	namespace    = "kube-system"
	flavorID     = "9a36b2e9-11ca-4e68-9093-e48e5cee7367"
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
	Network Networks `json:"networks"`
}

type Networks struct {
	ID           string       `json:"id"`
	FloatingPool FloatingPool `json:"floatingPool"`
}

type FloatingPool struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type PrivateNetworkConfig struct {
	Endpoint          string
	Username          string
	Password          string
	DomainName        string
	TenantID          string
	Region            string
	WorkerNetworkID   string
	IstioSubnetID     string
	FlavorID          string
	FloatingNetworkId string
}

// GetInfrastructureForExtension returns Infrastructure object for an extension object
func GetGlobalConfigforPrivateNetwork(
	ctx context.Context,
	c client.Reader,
	extension *extensionsv1alpha1.Extension,
	shootName string,
) (*PrivateNetworkConfig, error) {

	infra, err := GetInfrastructureForExtension(ctx, c, extension, shootName)
	if err != nil {
		return nil, err
	}
	netSpec := NetworkWorker{}
	if infra.Status.ProviderStatus != nil && infra.Status.ProviderStatus.Raw != nil {
		if err := json.Unmarshal(infra.Status.ProviderStatus.Raw, &netSpec); err != nil {
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
		Endpoint:          cfg.Global.AuthURL,
		Username:          cfg.Global.Username,
		Password:          cfg.Global.Password,
		DomainName:        cfg.Global.DomainName,
		TenantID:          cfg.Global.TenantID,
		Region:            cfg.Global.Region,
		WorkerNetworkID:   netSpec.Network.ID,
		IstioSubnetID:     cfg.LoadBalancer.SubnetID,
		FloatingNetworkId: netSpec.Network.FloatingPool.ID,
		FlavorID:          flavorID,
	}
	return config, nil
}
