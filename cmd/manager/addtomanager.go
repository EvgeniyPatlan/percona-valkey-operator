package main

// This file is the controller fan-out seam in cmd/manager. It exists so that
// from M2 onward, each per-resource controller package can be blank-imported
// here for its registration side-effect (its init() calls
// controller.Register(...)), keeping main.go free of a growing import list.
//
// The actual fan-out happens in newManager via controller.AddToManager(mgr),
// which ranges over the controller.AddToManagerFuncs registry populated by
// these imports.
//
// See docs/architecture/02-repo-layout.md §3, §9.

import (
	// PerconaValkeyCluster controller (M3): blank-imported for its init() registration.
	_ "valkey.percona.com/percona-valkey-operator/pkg/controller/perconavalkeycluster"
	// ValkeyNode controller (M2): blank-imported for its init() registration.
	_ "valkey.percona.com/percona-valkey-operator/pkg/controller/valkeynode"
)
