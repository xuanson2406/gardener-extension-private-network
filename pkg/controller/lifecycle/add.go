// SPDX-FileCopyrightText: 2021 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"github.com/gardener/gardener/extensions/pkg/controller/extension"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	// Type is the type of Extension resource.
	Type   = "private-network"
	Suffix = "-extension-service"
)

var (
	// DefaultAddOptions are the default AddOptions for AddToManager.
	DefaultAddOptions = AddOptions{}
)

// AddOptions are options to apply when adding the shoot service controller to the manager.
type AddOptions struct {
	// ControllerOptions contains options for the controller.
	ControllerOptions controller.Options
	// IgnoreOperationAnnotation specifies whether to ignore the operation annotation or not.
	IgnoreOperationAnnotation bool
}

// AddToManager adds a controller with the default Options to the given Controller Manager.
func AddToManager(mgr manager.Manager) error {
	return AddToManagerWithOptions(mgr, &DefaultAddOptions)
}

// AddToManagerWithOptions adds a controller with the given Options to the given manager.
// The opts.Reconciler is being set with a newly instantiated actuator.
func AddToManagerWithOptions(mgr manager.Manager, opts *AddOptions) error {
	return extension.Add(mgr, extension.AddArgs{
		Actuator:          NewActuator(mgr),
		ControllerOptions: opts.ControllerOptions,
		Name:              Type + Suffix,
		FinalizerSuffix:   Type + Suffix,
		Resync:            0,
		Predicates:        extension.DefaultPredicates(DefaultAddOptions.IgnoreOperationAnnotation),
		Type:              Type,
	})
}
