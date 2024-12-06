// SPDX-FileCopyrightText: 2021 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

// Package constants defines constants used
package constants

import (
	"errors"
	"time"
)

const (
	// Type is the type of Extension resource.
	Type                                = "private-network"
	Suffix                              = "-extension-service"
	DeletionTimeout                     = 2 * time.Minute
	IstioGatewayName                    = "kube-apiserver"
	KeyIstio                            = "istio-ingressgateway"
	NamespaceIstioIngress               = "istio-ingress"
	ProvisioningActiveStatus            = "ACTIVE"
	ProvisioningErrorStatus             = "ERROR"
	OperatingOnlineStatus               = "ONLINE"
	OperationErrorStatus                = "ERROR"
	OperationOfflineStatus              = "OFFLINE"
	PrefixLB                            = "private_network"
	ClusterTypePublic                   = "Public"
	ClusterTypePrivate                  = "Private"
	SecretConfig                        = "external-openstack-cloud-config-private"
	Namespace                           = "kube-system"
	FlavorType                          = "basic"
	DefaultLoadBalancerSourceRangesIPv4 = "0.0.0.0/0"
	OctaviaFeatureTimeout               = 3

	WaitLoadbalancerInitDelay   = 1 * time.Second
	WaitLoadbalancerFactor      = 1.2
	WaitLoadbalancerActiveSteps = 25
	WaitLoadbalancerDeleteSteps = 12
	Steps                       = 25
)

// ErrMultipleResults is used when we unexpectedly get back multiple results
var ErrMultipleResults = errors.New("multiple results where only one expected")

// ErrNotFound is used to inform that the object is missing
var ErrNotFound = errors.New("failed to find object")
