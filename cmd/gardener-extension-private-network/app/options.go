// SPDX-FileCopyrightText: 2021 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"

	"github.com/gardener/gardener-extension-private-network/pkg/controller/healthcheck"
	"github.com/gardener/gardener-extension-private-network/pkg/controller/lifecycle"
	controllercmd "github.com/gardener/gardener/extensions/pkg/controller/cmd"
	extensionshealthcheckcontroller "github.com/gardener/gardener/extensions/pkg/controller/healthcheck"
)

// ExtensionName is the name of the extension.
const ExtensionName = "private-network"

// Options holds configuration passed to the private-network controller.
type Options struct {
	generalOptions     *controllercmd.GeneralOptions
	restOptions        *controllercmd.RESTOptions
	managerOptions     *controllercmd.ManagerOptions
	controllerOptions  *controllercmd.ControllerOptions
	lifecycleOptions   *controllercmd.ControllerOptions
	healthOptions      *controllercmd.ControllerOptions
	controllerSwitches *controllercmd.SwitchOptions
	reconcileOptions   *controllercmd.ReconcilerOptions
	optionAggregator   controllercmd.OptionAggregator
}

// NewOptions creates a new Options instance.
func NewOptions() *Options {
	options := &Options{
		generalOptions: &controllercmd.GeneralOptions{},
		restOptions:    &controllercmd.RESTOptions{},
		managerOptions: &controllercmd.ManagerOptions{
			// These are default values.
			LeaderElection:          true,
			LeaderElectionID:        controllercmd.LeaderElectionNameID(ExtensionName),
			LeaderElectionNamespace: os.Getenv("LEADER_ELECTION_NAMESPACE"),
		},
		controllerOptions: &controllercmd.ControllerOptions{
			// This is a default value.
			MaxConcurrentReconciles: 10,
		},
		lifecycleOptions: &controllercmd.ControllerOptions{
			// This is a default value.
			MaxConcurrentReconciles: 10,
		},
		healthOptions: &controllercmd.ControllerOptions{
			// This is a default value.
			MaxConcurrentReconciles: 10,
		},
		reconcileOptions: &controllercmd.ReconcilerOptions{},
		controllerSwitches: controllercmd.NewSwitchOptions(
			controllercmd.Switch("private-network_lifecycle_controller", lifecycle.AddToManager),
			controllercmd.Switch(extensionshealthcheckcontroller.ControllerName, healthcheck.AddToManager)),
	}

	options.optionAggregator = controllercmd.NewOptionAggregator(
		options.generalOptions,
		options.restOptions,
		options.managerOptions,
		options.controllerOptions,
		controllercmd.PrefixOption("lifecycle-", options.lifecycleOptions),
		controllercmd.PrefixOption("healthcheck-", options.healthOptions),
		options.controllerSwitches,
		options.reconcileOptions,
	)

	return options
}
