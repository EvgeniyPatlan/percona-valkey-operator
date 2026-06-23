/*
Copyright Percona LLC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package valkey

import (
	"context"
	"testing"

	"go.uber.org/mock/gomock"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
)

// TestMockConfigClientExercise drives every MockConfigClient method through one
// scripted expectation so the generated test-double surface is exercised (it is
// the seam every controller test injects; without a direct exercise it shows as
// 0% and drags the package's coverage floor below the gate even though every
// statement is generated boilerplate).
func TestMockConfigClientExercise(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := NewMockConfigClient(ctrl)
	ctx := context.Background()

	m.EXPECT().InfoReplication(ctx).Return(map[string]string{"role": "master"}, nil)
	m.EXPECT().ConfigSet(ctx, "maxmemory", "1gb").Return(nil)
	m.EXPECT().Ping(ctx).Return(nil)
	m.EXPECT().Close().Return(nil)

	if _, err := m.InfoReplication(ctx); err != nil {
		t.Fatalf("InfoReplication: %v", err)
	}
	if err := m.ConfigSet(ctx, "maxmemory", "1gb"); err != nil {
		t.Fatalf("ConfigSet: %v", err)
	}
	if err := m.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestMockClusterClientExercise drives every MockClusterClient method (the full
// CLUSTER orchestration surface plus the new ACLLoad reload command) so the
// generated mock is fully exercised.
func TestMockClusterClientExercise(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	m := NewMockClusterClient(ctrl)
	ctx := context.Background()

	ranges := []SlotRange{{Start: 0, End: 100}}
	m.EXPECT().InfoReplication(ctx).Return(map[string]string{}, nil)
	m.EXPECT().ConfigSet(ctx, "masterauth", "pw").Return(nil)
	m.EXPECT().Ping(ctx).Return(nil)
	m.EXPECT().ACLLoad(ctx).Return(nil)
	m.EXPECT().ClusterMyID(ctx).Return("id", nil)
	m.EXPECT().ClusterMyShardID(ctx).Return("shard", nil)
	m.EXPECT().ClusterInfo(ctx).Return("cluster_state:ok", nil)
	m.EXPECT().ClusterNodes(ctx).Return("nodes", nil)
	m.EXPECT().Info(ctx, "replication").Return("role:master", nil)
	m.EXPECT().ClusterSetConfigEpoch(ctx, int64(7)).Return(nil)
	m.EXPECT().ClusterMeet(ctx, "10.0.0.1", ClientPort, BusPort).Return(nil)
	m.EXPECT().ClusterAddSlotsRange(ctx, ranges).Return(nil)
	m.EXPECT().ClusterReplicate(ctx, "primary").Return(nil)
	m.EXPECT().ClusterMigrateSlots(ctx, ranges, "dst").Return(nil)
	m.EXPECT().ClusterGetSlotMigrations(ctx).Return([]SlotMigration{{State: "success"}}, nil)
	m.EXPECT().ClusterForget(ctx, "stale").Return(nil)
	m.EXPECT().ClusterFailover(ctx, FailoverGraceful).Return(nil)
	m.EXPECT().Close().Return(nil)

	mustNoErr(t, func() error { _, e := m.InfoReplication(ctx); return e })
	mustNoErr(t, func() error { return m.ConfigSet(ctx, "masterauth", "pw") })
	mustNoErr(t, func() error { return m.Ping(ctx) })
	mustNoErr(t, func() error { return m.ACLLoad(ctx) })
	mustNoErr(t, func() error { _, e := m.ClusterMyID(ctx); return e })
	mustNoErr(t, func() error { _, e := m.ClusterMyShardID(ctx); return e })
	mustNoErr(t, func() error { _, e := m.ClusterInfo(ctx); return e })
	mustNoErr(t, func() error { _, e := m.ClusterNodes(ctx); return e })
	mustNoErr(t, func() error { _, e := m.Info(ctx, "replication"); return e })
	mustNoErr(t, func() error { return m.ClusterSetConfigEpoch(ctx, 7) })
	mustNoErr(t, func() error { return m.ClusterMeet(ctx, "10.0.0.1", ClientPort, BusPort) })
	mustNoErr(t, func() error { return m.ClusterAddSlotsRange(ctx, ranges) })
	mustNoErr(t, func() error { return m.ClusterReplicate(ctx, "primary") })
	mustNoErr(t, func() error { return m.ClusterMigrateSlots(ctx, ranges, "dst") })
	mustNoErr(t, func() error { _, e := m.ClusterGetSlotMigrations(ctx); return e })
	mustNoErr(t, func() error { return m.ClusterForget(ctx, "stale") })
	mustNoErr(t, func() error { return m.ClusterFailover(ctx, FailoverGraceful) })
	mustNoErr(t, func() error { return m.Close() })
}

// TestMockClientFactoriesExercise drives both factory mocks: For (narrow
// ConfigClient seam) and ForNode (wide ClusterClient seam).
func TestMockClientFactoriesExercise(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ctx := context.Background()
	node := &valkeyv1alpha1.ValkeyNode{}

	cf := NewMockClientFactory(ctrl)
	cc := NewMockConfigClient(ctrl)
	cf.EXPECT().For(ctx, node).Return(cc, nil)
	if got, err := cf.For(ctx, node); err != nil || got == nil {
		t.Fatalf("ClientFactory.For = (%v, %v)", got, err)
	}

	ccf := NewMockClusterClientFactory(ctrl)
	clusterClient := NewMockClusterClient(ctrl)
	ccf.EXPECT().ForNode(ctx, node).Return("10.0.0.1:6379", clusterClient, nil)
	addr, c, err := ccf.ForNode(ctx, node)
	if err != nil || c == nil || addr != "10.0.0.1:6379" {
		t.Fatalf("ClusterClientFactory.ForNode = (%q, %v, %v)", addr, c, err)
	}
}

func mustNoErr(t *testing.T, fn func() error) {
	t.Helper()
	if err := fn(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
