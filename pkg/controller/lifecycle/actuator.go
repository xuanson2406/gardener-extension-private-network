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
	"github.com/gardener/gardener/extensions/pkg/controller"
	"github.com/gardener/gardener/extensions/pkg/controller/extension"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	istionetworkv1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	deletionTimeout  = 2 * time.Minute
	istioGatewayName = "kube-apiserver"
	keyIstio         = "istio-ingressgateway"
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
	istioNamespace, err := a.findIstioNamespaceForExtension(ctx, ex)
	if err != nil {
		// we ignore errors for hibernated clusters if they don't have a Gateway
		// resource for the extension to get the istio namespace from
		if controller.IsHibernated(cluster) {
			return client.IgnoreNotFound(err)
		}
		return err
	}
	vipLBistio, err := a.findVipLBistio(ctx, istioNamespace)
	if err != nil {
		return err
	}
	privateNetworkConfig, err := helper.GetGlobalConfigforPrivateNetwork(ctx, a.client, ex, cluster.Shoot.Name)
	if err != nil {
		return fmt.Errorf("Error to get private network configuration for shoot %s: [%v]", cluster.Shoot.Name, err)
	}
	lbPrivateNetwork, err := helper.CreateLoadBalancer(ctx, privateNetworkConfig, ex, vipLBistio)
	if err != nil {
		return err
	}
	extState.AddressLoadBalancer
	return a.updateStatus(ctx, ex, extState)
}

// Delete the Extension resource.
func (a *actuator) Delete(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	a.logger.Info("Hello World, I just entered the Delete method")
	return nil
}

// Restore the Extension resource.
func (a *actuator) Restore(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	return a.Reconcile(ctx, log, ex)
}

// Migrate the Extension resource.
func (a *actuator) Migrate(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	return a.Delete(ctx, log, ex)
}

func (a *actuator) findIstioNamespaceForExtension(
	ctx context.Context, ex *extensionsv1alpha1.Extension,
) (
	istioNamespace string,
	err error,
) {
	gw := istionetworkv1beta1.Gateway{}

	err = a.client.Get(ctx, client.ObjectKey{
		Namespace: ex.Namespace,
		Name:      istioGatewayName,
	}, &gw)
	if err != nil {
		return "", err
	}

	labelsSelector := client.MatchingLabels(gw.Spec.Selector)

	deployments := appsv1.DeploymentList{}
	err = a.client.List(ctx, &deployments, labelsSelector)
	if err != nil {
		return "", err
	}
	if len(deployments.Items) != 1 {
		return "", fmt.Errorf("no istio namespace could be selected, because the number of deployments found is %d", len(deployments.Items))
	}

	return deployments.Items[0].Namespace, nil
}

func (a *actuator) findVipLBistio(ctx context.Context, istioNamespace string) ([]string, error) {
	istioIPlist := []string{}
	svcList := &v1.ServiceList{}
	err := a.client.List(ctx, svcList, client.InNamespace(istioNamespace))
	if err != nil {
		return nil, fmt.Errorf("Failed to list services in namespace %s: %v", istioNamespace, err)
	}
	for _, svc := range svcList.Items {
		if svc.Spec.Type == v1.ServiceTypeLoadBalancer && svc.Spec.Selector["app"] == keyIstio {
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
