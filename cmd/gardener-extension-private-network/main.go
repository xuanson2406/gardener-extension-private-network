// SPDX-FileCopyrightText: 2021 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/gardener/gardener-extension-private-network/cmd/gardener-extension-private-network/app"

	"github.com/gardener/gardener/cmd/utils"
	"github.com/gardener/gardener/pkg/logger"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
)

func main() {
	utils.DeduplicateWarnings()

	logf.SetLogger(logger.MustNewZapLogger(logger.InfoLevel, logger.FormatJSON))

	ctx := signals.SetupSignalHandler()
	if err := app.NewServiceControllerCommand().ExecuteContext(ctx); err != nil {
		fmt.Println(err)
	}
}
