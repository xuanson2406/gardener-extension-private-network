// SPDX-FileCopyrightText: 2021 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gardener/gardener-extension-private-network/pkg/extensionspec"
	"github.com/gardener/gardener-extension-private-network/pkg/helper"
	"github.com/gardener/gardener/extensions/pkg/controller/extension"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/loadbalancers"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	deletionTimeout                     = 2 * time.Minute
	istioGatewayName                    = "kube-apiserver"
	keyIstio                            = "istio-ingressgateway"
	namespaceIstioIngress               = "istio-ingress"
	activeStatus                        = "ACTIVE"
	errorStatus                         = "ERROR"
	defaultLoadBalancerSourceRangesIPv4 = "0.0.0.0/0"
	prefixLB                            = "private-network"
)

// NewActuator returns an actuator responsible for Extension resources.
func NewActuator(mgr manager.Manager) extension.Actuator {
	return &actuator{
		logger:  log.Log.WithName("FirstLogger"),
		client:  mgr.GetClient(),
		config:  mgr.GetConfig(),
		decoder: serializer.NewCodecFactory(mgr.GetScheme(), serializer.EnableStrict).UniversalDecoder(),
	}
}

type actuator struct {
	logger  logr.Logger // logger
	client  client.Client
	config  *rest.Config
	decoder runtime.Decoder
}

// ExtensionState contains the State of the Extension
type ExtensionState struct {
	AddressLoadBalancer *string `json:"addressLoadBalancer"`
}

// Reconcile the Extension resource.
func (a *actuator) Reconcile(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	a.logger.Info("Hello World, I just entered the Reconcile method")
	nameLB := fmt.Sprintf("%s-%s", prefixLB, ex.Namespace)
	var loadbalancer *loadbalancers.LoadBalancer
	cluster, err := helper.GetClusterForExtension(ctx, a.client, ex)
	if err != nil {
		return err
	}
	extSpec := &extensionspec.ExtensionSpec{}
	if ex.Spec.ProviderConfig != nil && ex.Spec.ProviderConfig.Raw != nil {
		if err := json.Unmarshal(ex.Spec.ProviderConfig.Raw, &extSpec); err != nil {
			return err
		}
	}
	extState, err := getExtensionState(ex)
	if err != nil {
		return err
	}
	vipLBistio, err := a.findVipLBistio(ctx, namespaceIstioIngress)
	if err != nil {
		return err
	}
	privateNetworkConfig, err := helper.GetGlobalConfigforPrivateNetwork(ctx, a.client, ex, cluster.Shoot.Name)
	if err != nil {
		return fmt.Errorf("error to get private network configuration for shoot %s: [%v]", cluster.Shoot.Name, err)
	}
	loadbalancer, err = helper.GetLoadbalancerByName(privateNetworkConfig, nameLB)
	if err != nil {
		if err != helper.ErrNotFound {
			return fmt.Errorf("error getting loadbalancer for extension %s: [%v]", ex.Namespace, err)
		}
		klog.InfoS("Creating loadbalancer", "lbName", nameLB, "extension", klog.KObj(ex))
		loadbalancer, err = helper.CreateLoadBalancer(ctx, privateNetworkConfig, ex, vipLBistio, nameLB)
		if err != nil {
			return err
		}
	}
	if loadbalancer.ProvisioningStatus != activeStatus {
		if loadbalancer.ProvisioningStatus == errorStatus {
			err := helper.DeleteLoadbalancer(privateNetworkConfig, loadbalancer, false)
			if err != nil {
				return fmt.Errorf("error to delete the error Loadbalancer [ID=%s] [Name=%s]: [%v]", loadbalancer.ID, loadbalancer.Name, err)
			}
			return fmt.Errorf("load balancer for extension private-network [ns=%s] current provisioning status is %s, deleted it and recreate",
				ex.Namespace, errorStatus)
		}
		return fmt.Errorf("load balancer %s is not ACTIVE, current provisioning status: %s", loadbalancer.ID, loadbalancer.ProvisioningStatus)
	}
	floatIP, err := helper.AttachFloatingIP(loadbalancer, privateNetworkConfig, ex)
	if err != nil {
		return fmt.Errorf("error to attach floating IP for LB [ID=%s]: [%v]", loadbalancer.ID, err)
	}
	extState.AddressLoadBalancer = &floatIP
	return a.updateStatus(ctx, ex, extState)
}

// Delete the Extension resource.
func (a *actuator) Delete(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	a.logger.Info("Hello World, I just entered the Delete method")
	namespace := ex.GetNamespace()
	log.Info("Component is being deleted", "component", "", "namespace", namespace)
	nameLB := fmt.Sprintf("%s-%s", prefixLB, ex.Namespace)
	cluster, err := helper.GetClusterForExtension(ctx, a.client, ex)
	if err != nil {
		return err
	}
	privateNetworkConfig, err := helper.GetGlobalConfigforPrivateNetwork(ctx, a.client, ex, cluster.Shoot.Name)
	if err != nil {
		return fmt.Errorf("error to get private network configuration for shoot %s: [%v]", cluster.Shoot.Name, err)
	}
	loadbalancer, err := helper.GetLoadbalancerByName(privateNetworkConfig, nameLB)
	if err != nil {
		if err != helper.ErrNotFound {
			return fmt.Errorf("error getting loadbalancer for extension %s: [%v]", ex.Namespace, err)
		}
		return nil
	}
	return helper.DeleteLoadbalancer(privateNetworkConfig, loadbalancer, false)
}

// Restore the Extension resource.
func (a *actuator) Restore(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	return a.Reconcile(ctx, log, ex)
}

// Migrate the Extension resource.
func (a *actuator) Migrate(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	return a.Delete(ctx, log, ex)
}

func (a *actuator) findVipLBistio(ctx context.Context, istioNamespace string) ([]string, error) {
	istioIPlist := []string{}
	svcList := &v1.ServiceList{}
	err := a.client.List(ctx, svcList, client.InNamespace(istioNamespace))
	if err != nil {
		return nil, fmt.Errorf("failed to list services in namespace %s: %v", istioNamespace, err)
	}
	for _, svc := range svcList.Items {
		if svc.Spec.Type == v1.ServiceTypeLoadBalancer && svc.Spec.Selector["app"] == keyIstio {
			klog.Infof("Add IP external %s to list", svc.Status.LoadBalancer.Ingress[0].IP)
			istioIPlist = append(istioIPlist, svc.Status.LoadBalancer.Ingress[0].IP)
		}
	}
	return istioIPlist, nil
}

func getExtensionState(ex *extensionsv1alpha1.Extension) (*ExtensionState, error) {
	extState := &ExtensionState{}
	if ex.Status.State != nil && ex.Status.State.Raw != nil {
		if err := json.Unmarshal(ex.Status.State.Raw, &extState); err != nil {
			return nil, err
		}
	}

	return extState, nil
}

func (a *actuator) updateStatus(
	ctx context.Context,
	ex *extensionsv1alpha1.Extension,
	state *ExtensionState,
) error {
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return err
	}

	patch := client.MergeFrom(ex.DeepCopy())

	ex.Status.State = &runtime.RawExtension{Raw: stateJSON}
	return a.client.Status().Patch(ctx, ex, patch)
}
