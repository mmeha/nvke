// SPDX-License-Identifier: Apache-2.0

// Package public selects the resources exposed by each API server.
// Generated registrations bind public types to datastore resources;
// Tenant and Operator choose which registrations each server mounts.
package public

import "github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"

// Tenant exposes workload cluster management.
func Tenant() []resource.Registration {
	return []resource.Registration{NVKECluster()}
}

// Operator exposes release and machine-image publication.
func Operator() []resource.Registration {
	return []resource.Registration{ClusterRelease(), MachineImage()}
}
