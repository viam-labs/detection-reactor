// Package main runs the detection-reactor Viam module.
package main

import (
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"

	"github.com/viam-labs/detection-reactor/reactor"
)

func main() {
	module.ModularMain(
		resource.APIModel{API: generic.API, Model: reactor.Model},
	)
}
