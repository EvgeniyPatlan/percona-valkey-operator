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
	"crypto/tls"
	"fmt"
	"strconv"
	"strings"

	vgo "github.com/valkey-io/valkey-go"
)

// BusPort is the Valkey cluster-bus port (Charter: client port + 10000 = 16379).
const BusPort = 16379

// ClientPort is the Valkey client port (Charter: 6379).
const ClientPort = 6379

// wrongPass is the engine error prefix returned when auth credentials are
// rejected. On WRONGPASS we retry once unauthenticated (05 §10) — a freshly
// bootstrapped node may not yet have the ACL applied.
const wrongPass = "WRONGPASS"

// Auth carries the credentials used to authenticate to a node. An empty
// Username/Password connects unauthenticated.
type Auth struct {
	// Username is the ACL user to AUTH as (typically _operator).
	Username string
	// Password is the user's password.
	Password string
}

// realClient is the real ConfigClient backed by a single valkey-go connection.
type realClient struct {
	c vgo.Client
}

// NewClient dials a single Valkey node at addr (host:port) with
// ForceSingleClient=true (prevents redirect loops when talking to one node),
// authenticating with auth and optional TLS. On a WRONGPASS error it retries
// once unauthenticated so a node whose ACL is not yet applied is still reachable
// (05 §10). The returned client satisfies the full ClusterClient surface.
func NewClient(addr string, auth Auth, tlsConfig *tls.Config) (ClusterClient, error) {
	opt := vgo.ClientOption{
		InitAddress:       []string{addr},
		ForceSingleClient: true,
		TLSConfig:         tlsConfig,
		Username:          auth.Username,
		Password:          auth.Password,
	}
	c, err := vgo.NewClient(opt)
	if err != nil {
		if auth.Username != "" && isWrongPass(err) {
			// WRONGPASS: fall back to an unauthenticated connection (05 §10).
			opt.Username = ""
			opt.Password = ""
			c, err = vgo.NewClient(opt)
			if err != nil {
				return nil, fmt.Errorf("dial %s (unauth fallback): %w", addr, err)
			}
			return &realClient{c: c}, nil
		}
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return &realClient{c: c}, nil
}

// isWrongPass reports whether err is an engine WRONGPASS auth rejection.
func isWrongPass(err error) bool {
	return err != nil && strings.Contains(err.Error(), wrongPass)
}

// InfoReplication issues `INFO replication` and parses the reply.
func (rc *realClient) InfoReplication(ctx context.Context) (map[string]string, error) {
	out, err := rc.c.Do(ctx, rc.c.B().Info().Section("replication").Build()).ToString()
	if err != nil {
		return nil, fmt.Errorf("INFO replication: %w", err)
	}
	return ParseInfoReplication(out), nil
}

// ConfigSet applies a single key=value via CONFIG SET.
func (rc *realClient) ConfigSet(ctx context.Context, key, value string) error {
	if err := rc.c.Do(ctx, rc.c.B().ConfigSet().ParameterValue().ParameterValue(key, value).Build()).Error(); err != nil {
		return fmt.Errorf("CONFIG SET %s: %w", key, err)
	}
	return nil
}

// Ping issues PING.
func (rc *realClient) Ping(ctx context.Context) error {
	if err := rc.c.Do(ctx, rc.c.B().Ping().Build()).Error(); err != nil {
		return fmt.Errorf("PING: %w", err)
	}
	return nil
}

// Close releases the underlying connection.
func (rc *realClient) Close() error {
	rc.c.Close()
	return nil
}

// Address builds a host:port dial address for the client port.
func Address(host string) string {
	return fmt.Sprintf("%s:%d", host, ClientPort)
}

// ClusterMyID issues CLUSTER MYID.
func (rc *realClient) ClusterMyID(ctx context.Context) (string, error) {
	out, err := rc.c.Do(ctx, rc.c.B().ClusterMyid().Build()).ToString()
	if err != nil {
		return "", fmt.Errorf("CLUSTER MYID: %w", err)
	}
	return out, nil
}

// ClusterMyShardID issues CLUSTER MYSHARDID.
func (rc *realClient) ClusterMyShardID(ctx context.Context) (string, error) {
	out, err := rc.c.Do(ctx, rc.c.B().ClusterMyshardid().Build()).ToString()
	if err != nil {
		return "", fmt.Errorf("CLUSTER MYSHARDID: %w", err)
	}
	return out, nil
}

// ClusterInfo issues CLUSTER INFO. The verbatim-string "txt:" prefix is left to
// the parser to strip.
func (rc *realClient) ClusterInfo(ctx context.Context) (string, error) {
	out, err := rc.c.Do(ctx, rc.c.B().ClusterInfo().Build()).ToString()
	if err != nil {
		return "", fmt.Errorf("CLUSTER INFO: %w", err)
	}
	return out, nil
}

// ClusterNodes issues CLUSTER NODES.
func (rc *realClient) ClusterNodes(ctx context.Context) (string, error) {
	out, err := rc.c.Do(ctx, rc.c.B().ClusterNodes().Build()).ToString()
	if err != nil {
		return "", fmt.Errorf("CLUSTER NODES: %w", err)
	}
	return out, nil
}

// Info issues INFO for the given section ("" requests all sections).
func (rc *realClient) Info(ctx context.Context, section string) (string, error) {
	cmd := rc.c.B().Info()
	var out string
	var err error
	if section == "" {
		out, err = rc.c.Do(ctx, cmd.Build()).ToString()
	} else {
		out, err = rc.c.Do(ctx, cmd.Section(section).Build()).ToString()
	}
	if err != nil {
		return "", fmt.Errorf("INFO %s: %w", section, err)
	}
	return out, nil
}

// ClusterSetConfigEpoch issues CLUSTER SET-CONFIG-EPOCH.
func (rc *realClient) ClusterSetConfigEpoch(ctx context.Context, epoch int64) error {
	if err := rc.c.Do(ctx, rc.c.B().ClusterSetConfigEpoch().ConfigEpoch(epoch).Build()).Error(); err != nil {
		return fmt.Errorf("CLUSTER SET-CONFIG-EPOCH %d: %w", epoch, err)
	}
	return nil
}

// ClusterMeet issues CLUSTER MEET ip port busport.
func (rc *realClient) ClusterMeet(ctx context.Context, ip string, port, busPort int) error {
	cmd := rc.c.B().ClusterMeet().Ip(ip).Port(int64(port)).ClusterBusPort(int64(busPort)).Build()
	if err := rc.c.Do(ctx, cmd).Error(); err != nil {
		return fmt.Errorf("CLUSTER MEET %s:%d@%d: %w", ip, port, busPort, err)
	}
	return nil
}

// ClusterAddSlotsRange issues a single CLUSTER ADDSLOTSRANGE covering every
// supplied range.
func (rc *realClient) ClusterAddSlotsRange(ctx context.Context, ranges []SlotRange) error {
	if len(ranges) == 0 {
		return nil
	}
	cmd := rc.c.B().ClusterAddslotsrange().StartSlotEndSlot()
	for _, r := range ranges {
		cmd = cmd.StartSlotEndSlot(int64(r.Start), int64(r.End))
	}
	if err := rc.c.Do(ctx, cmd.Build()).Error(); err != nil {
		return fmt.Errorf("CLUSTER ADDSLOTSRANGE %s: %w", FormatSlotRanges(ranges), err)
	}
	return nil
}

// ClusterReplicate issues CLUSTER REPLICATE primaryID.
func (rc *realClient) ClusterReplicate(ctx context.Context, primaryID string) error {
	if err := rc.c.Do(ctx, rc.c.B().ClusterReplicate().NodeId(primaryID).Build()).Error(); err != nil {
		return fmt.Errorf("CLUSTER REPLICATE %s: %w", primaryID, err)
	}
	return nil
}

// ClusterMigrateSlots issues a single atomic CLUSTER MIGRATESLOTS for every
// range to dstID (Valkey 9.0+). An "unknown subcommand" reply is wrapped with an
// actionable upgrade hint (05 §4).
func (rc *realClient) ClusterMigrateSlots(ctx context.Context, ranges []SlotRange, dstID string) error {
	if len(ranges) == 0 {
		return nil
	}
	cmd := rc.c.B().Arbitrary("CLUSTER", "MIGRATESLOTS")
	for _, r := range ranges {
		cmd = cmd.Args("SLOTSRANGE", strconv.Itoa(r.Start), strconv.Itoa(r.End), "NODE", dstID)
	}
	if err := rc.c.Do(ctx, cmd.Build()).Error(); err != nil {
		return wrapUnsupportedErr(fmt.Errorf("CLUSTER MIGRATESLOTS -> %s: %w", dstID, err))
	}
	return nil
}

// ClusterGetSlotMigrations issues CLUSTER GETSLOTMIGRATIONS and parses the
// reply. An entry that cannot be parsed is reported as a non-terminal "running"
// migration so the caller errs on the side of waiting (05 §4).
func (rc *realClient) ClusterGetSlotMigrations(ctx context.Context) ([]SlotMigration, error) {
	cmd := rc.c.B().Arbitrary("CLUSTER", "GETSLOTMIGRATIONS").Build()
	entries, err := rc.c.Do(ctx, cmd).ToArray()
	if err != nil {
		return nil, wrapUnsupportedErr(fmt.Errorf("CLUSTER GETSLOTMIGRATIONS: %w", err))
	}
	migrations := make([]SlotMigration, 0, len(entries))
	for _, entry := range entries {
		values, parseErr := entry.AsStrMap()
		if parseErr != nil {
			migrations = append(migrations, SlotMigration{State: "running"})
			continue
		}
		migrations = append(migrations, SlotMigration{
			State:  strings.ToLower(values["state"]),
			NodeID: values["node"],
		})
	}
	return migrations, nil
}

// ClusterForget issues CLUSTER FORGET nodeID. An "unknown node" reply means the
// node is already gone and is treated as success (05 §7).
func (rc *realClient) ClusterForget(ctx context.Context, nodeID string) error {
	if err := rc.c.Do(ctx, rc.c.B().ClusterForget().NodeId(nodeID).Build()).Error(); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unknown node") {
			return nil
		}
		return fmt.Errorf("CLUSTER FORGET %s: %w", nodeID, err)
	}
	return nil
}

// ClusterFailover issues CLUSTER FAILOVER in the given mode (05 §6-§7).
func (rc *realClient) ClusterFailover(ctx context.Context, mode FailoverMode) error {
	b := rc.c.B().ClusterFailover()
	var cmd vgo.Completed
	switch mode {
	case FailoverForce:
		cmd = b.Force().Build()
	case FailoverTakeover:
		cmd = b.Takeover().Build()
	default:
		cmd = b.Build()
	}
	if err := rc.c.Do(ctx, cmd).Error(); err != nil {
		return fmt.Errorf("CLUSTER FAILOVER %q: %w", string(mode), err)
	}
	return nil
}

// wrapUnsupportedErr appends an upgrade hint when err indicates the engine does
// not recognise an atomic-migration subcommand (Valkey < 9.0, 05 §4).
func wrapUnsupportedErr(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "unknown command") ||
		strings.Contains(msg, "unknown subcommand") ||
		strings.Contains(msg, "wrong number of arguments") {
		return fmt.Errorf("%w; upgrade to Valkey 9.0+ for atomic slot migration", err)
	}
	return err
}

// IsSlotsNotServedByNode reports whether err indicates the source no longer owns
// the requested slots (e.g. a concurrent move completed). Treated as benign — the
// controller requeues and re-plans against fresh state (05 §4).
func IsSlotsNotServedByNode(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "slots are not served by this node")
}
