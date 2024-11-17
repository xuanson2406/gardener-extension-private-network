// SPDX-FileCopyrightText: 2021 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"time"

	"github.com/gardener/gardener/extensions/pkg/controller/extension"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// DefaultAddOptions contains configuration for the private-network controller
var DefaultAddOptions = AddOptions{}

// AddOptions are options to apply when adding the private-network controller to the manager.
type AddOptions struct {
	// ControllerOptions contains options for the controller.
	ControllerOptions controller.Options
	// IgnoreOperationAnnotation specifies whether to ignore the operation annotation or not.
	IgnoreOperationAnnotation bool
}

// AddToManager adds a private-network Lifecycle controller to the given Controller Manager.
func AddToManager(mgr manager.Manager) error {
	return extension.Add(mgr, extension.AddArgs{
		Actuator:          NewActuator(),
		ControllerOptions: DefaultAddOptions.ControllerOptions,
		Name:              "private-network_lifecycle_controller",
		FinalizerSuffix:   "private-network",
		Resync:            60 * time.Minute,
		Predicates:        extension.DefaultPredicates(DefaultAddOptions.IgnoreOperationAnnotation),
		Type:              "private-network",
	})
}
