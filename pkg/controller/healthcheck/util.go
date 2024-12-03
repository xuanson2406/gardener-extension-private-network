package healthcheck

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gardener/gardener-extension-private-network/pkg/extensionspec"
	"github.com/gardener/gardener-extension-private-network/pkg/helper"
	"github.com/gardener/gardener/extensions/pkg/controller/healthcheck"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// DefaultHealthChecker all the information for the Worker HealthCheck.
// This check assumes that the MachineControllerManager (https://github.com/gardener/machine-controller-manager) has been
// deployed by the Worker extension controller.
type DefaultHealthChecker struct {
	logger logr.Logger
	// Needs to be set by actuator before calling the Check function
	seedClient client.Client
	// // make sure shoot client is instantiated
	prefixLBName  string
	extensionName string
}

// NewNodesChecker is a health check function which performs certain checks about the nodes registered in the cluster.
// It implements the healthcheck.HealthCheck interface.
func NewLoadbalancerChecker(prefixName, exName string) *DefaultHealthChecker {

	return &DefaultHealthChecker{
		prefixLBName:  prefixName,
		extensionName: exName,
	}
}

// InjectSeedClient injects the seed client.
func (h *DefaultHealthChecker) InjectSeedClient(seedClient client.Client) {
	h.seedClient = seedClient
}

// SetLoggerSuffix injects the logger.
func (h *DefaultHealthChecker) SetLoggerSuffix(provider, extension string) {
	h.logger = log.Log.WithName(fmt.Sprintf("%s-%s-healthcheck-loadbalancer", provider, extension))
}

// DeepCopy clones the healthCheck struct by making a copy and returning the pointer to that new copy.
func (h *DefaultHealthChecker) DeepCopy() healthcheck.HealthCheck {
	copy := *h
	return &copy
}

// Check executes the health check.
func (h *DefaultHealthChecker) Check(ctx context.Context, request types.NamespacedName) (*healthcheck.SingleCheckResult, error) {
	nameLB := fmt.Sprintf("%s-%s", h.prefixLBName, request.Namespace)
	h.logger.Info("name LB to healthcheck is", nameLB)
	extension := &extensionsv1alpha1.Extension{}
	if err := h.seedClient.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: h.extensionName}, extension); err != nil {
		if apierrors.IsNotFound(err) {
			return &healthcheck.SingleCheckResult{
				Status: gardencorev1beta1.ConditionFalse,
				Detail: fmt.Sprintf("Managed Resource %q in namespace %q not found", healthChecker.managedResourceName, request.Namespace),
			}, nil
		}
		err := fmt.Errorf("unable to check Loadbalancer. Failed to get extension in namespace %q: %w", request.Namespace, err)
		h.logger.Error(err, "Health check failed")
		return nil, err
	}
	cluster, err := helper.GetClusterForExtension(ctx, h.seedClient, extension)
	if err != nil {
		return err
	}
	extSpec := &extensionspec.ExtensionSpec{}
	if ex.Spec.ProviderConfig != nil && ex.Spec.ProviderConfig.Raw != nil {
		if err := json.Unmarshal(ex.Spec.ProviderConfig.Raw, &extSpec); err != nil {
			return err
		}
	}
	return &healthcheck.SingleCheckResult{Status: gardencorev1beta1.ConditionTrue}, nil
}
