// Package controller is the controller fan-out registry: the single seam where
// every per-resource controller registers itself with the manager.
//
// In M0 (bootstrap) AddToManagerFuncs is EMPTY — no controllers exist yet. From
// M2 onward each controller package appends its AddToManager function (via an
// init() or an explicit registration in cmd/manager) so the manager wires all
// reconcilers through one call: controller.AddToManager(mgr).
//
// See docs/architecture/02-repo-layout.md §3, §9 (cmd/manager is the single
// composition root; no controller imports another controller).
package controller

import "sigs.k8s.io/controller-runtime/pkg/manager"

// AddToManagerFuncs is the controller registration registry. Each per-resource
// controller appends its AddToManager func here; cmd/manager ranges over it.
// EMPTY in M0.
var AddToManagerFuncs []func(manager.Manager) error

// Register adds a controller AddToManager function to the registry. Controller
// packages call this (typically from an init()) so cmd/manager need not import
// each controller package by name.
func Register(f func(manager.Manager) error) {
	AddToManagerFuncs = append(AddToManagerFuncs, f)
}

// AddToManager wires every registered controller into the supplied manager. It
// returns the first registration error, if any. On the empty M0 registry it is
// a no-op returning nil.
func AddToManager(mgr manager.Manager) error {
	for _, add := range AddToManagerFuncs {
		if err := add(mgr); err != nil {
			return err
		}
	}
	return nil
}
