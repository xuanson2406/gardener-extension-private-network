package healthcheck

import (
	"context"
	"fmt"

	consts "github.com/gardener/gardener-extension-private-network/pkg/constants"
	"github.com/gardener/gardener-extension-private-network/pkg/helper"
	"github.com/gardener/gardener/extensions/pkg/controller/healthcheck"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/go-logr/logr"
	"gopkg.in/gcfg.v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// DefaultHealthChecker all the information for the Extension HealthCheck.
type DefaultHealthChecker struct {
	logger        logr.Logger
	seedClient    client.Client
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
	h.logger.Info("LB to healthcheck", "namespace", request.Namespace, "LB name", nameLB)
	extension := &extensionsv1alpha1.Extension{}
	if err := h.seedClient.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: h.extensionName}, extension); err != nil {
		if apierrors.IsNotFound(err) {
			klog.Infof("Error when get extension: [%v]", err)
			return &healthcheck.SingleCheckResult{
				Status: gardencorev1beta1.ConditionFalse,
				Detail: fmt.Sprintf("Private Network extension in namespace %q not found", request.Namespace),
			}, nil
		}
		klog.Infof("Error when get extension: NOT FOUND [%v]", err)
		err := fmt.Errorf("unable to check Loadbalancer. Failed to get extension in namespace %q: %w", request.Namespace, err)
		h.logger.Error(err, "Health check failed")
		return nil, err
	}
	secret := &v1.Secret{}
	err := h.seedClient.Get(ctx, client.ObjectKey{
		Namespace: consts.Namespace,
		Name:      consts.SecretConfig,
	}, secret)
	if err != nil {
		klog.Infof("Error to get secrets contain config: [%v]", err)
		return nil, err
	}
	encodedData := secret.Data["cloud.conf"]
	if encodedData == nil {
		klog.Infof("cloud.conf key not found in the secret")
		return nil, fmt.Errorf("cloud.conf key not found in the secret")
	}
	// Parse the decoded data into the struct
	var cfg helper.Config
	_ = gcfg.ReadStringInto(&cfg, string(encodedData))
	config := &helper.PrivateNetworkConfig{
		AuthOpt: helper.AuthOpt{
			Endpoint:   cfg.Global.AuthURL,
			Username:   cfg.Global.Username,
			Password:   cfg.Global.Password,
			DomainName: cfg.Global.DomainName,
			TenantID:   cfg.Global.TenantID,
			Region:     cfg.Global.Region,
		},
	}
	loadbalancer, err := helper.GetLoadbalancerByName(config, nameLB)
	if err != nil {
		if err != helper.ErrNotFound {
			klog.Infof("Error when get loadbalancer: [%v]", err)
			return nil, fmt.Errorf("error getting loadbalancer for extension %s: [%v]", extension.Namespace, err)
		}
		klog.Infof("Not found loadbalancer %s", nameLB)
		return &healthcheck.SingleCheckResult{
			Status: gardencorev1beta1.ConditionFalse,
			Detail: fmt.Sprintf("Not Found Loadbalancer [Name=%s] for private network extension in namespace %q", nameLB, request.Namespace),
		}, nil
	}
	klog.Infof("Success get LB name %s - Operation %s - Provisioning %s", loadbalancer.Name, loadbalancer.OperatingStatus, loadbalancer.ProvisioningStatus)
	if loadbalancer.ProvisioningStatus != consts.ProvisioningActiveStatus {
		if loadbalancer.ProvisioningStatus == consts.ProvisioningErrorStatus {
			klog.Infof("Provisioning status of LB [Name=%s] in namespace %q is %s", nameLB, request.Namespace, loadbalancer.OperatingStatus)
			return &healthcheck.SingleCheckResult{
				Status: gardencorev1beta1.ConditionFalse,
				Detail: fmt.Sprintf("Provisioning status of LB [Name=%s] in namespace %q is %s", nameLB, request.Namespace, loadbalancer.OperatingStatus),
			}, nil
		}
		klog.Infof("Provisioning status of LB [Name=%s] in namespace %q is not Active: Status=%s", nameLB, request.Namespace, loadbalancer.OperatingStatus)
		return &healthcheck.SingleCheckResult{
			Status: gardencorev1beta1.ConditionProgressing,
			Detail: fmt.Sprintf("Provisioning status of LB [Name=%s] in namespace %q is not Active: Status=%s", nameLB, request.Namespace, loadbalancer.OperatingStatus),
		}, nil
	}
	if loadbalancer.OperatingStatus != consts.OperatingOnlineStatus {
		if loadbalancer.OperatingStatus == consts.OperationErrorStatus || loadbalancer.OperatingStatus == consts.OperationOfflineStatus {
			klog.Infof("Operating status of LB [Name=%s] in namespace %q is %s", nameLB, request.Namespace, loadbalancer.OperatingStatus)
			return &healthcheck.SingleCheckResult{
				Status: gardencorev1beta1.ConditionFalse,
				Detail: fmt.Sprintf("Operating status of LB [Name=%s] in namespace %q is %s", nameLB, request.Namespace, loadbalancer.OperatingStatus),
			}, nil
		}
		klog.Infof("Operating status of LB [Name=%s] in namespace %q is not Active: Status=%s", nameLB, request.Namespace, loadbalancer.OperatingStatus)
		return &healthcheck.SingleCheckResult{

			Status: gardencorev1beta1.ConditionProgressing,
			Detail: fmt.Sprintf("Operating status of LB [Name=%s] in namespace %q is not Active: Status=%s", nameLB, request.Namespace, loadbalancer.OperatingStatus),
		}, nil
	}
	klog.Infof("Success healthcheck LB %s - operating %s - provisioning %s", loadbalancer.Name, loadbalancer.OperatingStatus, loadbalancer.ProvisioningStatus)
	return &healthcheck.SingleCheckResult{Status: gardencorev1beta1.ConditionTrue}, nil
}
