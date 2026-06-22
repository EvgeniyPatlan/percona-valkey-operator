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

package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// RDBSource abstracts HOW the backup Job obtains a shard primary's RDB bytes,
// resolving open question CR-10 / Q1 (06 §4.2 leaves the wire mechanism open).
//
// DECISION (CR-10): the Job acts as a Valkey REPLICA and receives the RDB bulk
// payload over the replication protocol via the legacy SYNC command. SYNC makes
// the primary fork (the BGSAVE the spec calls for) and stream the resulting RDB
// inline as a single RESP bulk string: "$<length>\r\n<length bytes of RDB>".
// This needs NO pod-filesystem access, NO privileged in-pod helper, and NO second
// copy on the data PVC — the Job reads the dump straight off the wire and pipes
// it to object storage. It is the canonical "attach as replica, pull the RDB"
// mechanism Valkey itself uses for full resync, so it is robust and version-stable.
//
// The interface exists so the upload/manifest logic is unit-testable with a fake
// source (no live Valkey); the live SYNC path is exercised by e2e/integration
// against a real engine + MinIO (06 §4.4 test plan, GO-4.4).
type RDBSource interface {
	// Open issues the snapshot request to the primary and returns a stream of the
	// RDB bytes plus the declared length (-1 if the engine streams an unbounded
	// EOF-delimited payload, the "diskless" SYNC form). The caller reads to EOF (or
	// to length) and MUST Close the stream.
	Open(ctx context.Context) (stream io.ReadCloser, length int64, err error)
}

// syncReadTimeout bounds how long the Job waits for the primary to begin
// streaming the RDB after SYNC (the fork + dump can take a while on large
// datasets, but a wedged connection must not hang the Job forever — the Job's
// activeDeadlineSeconds is the outer bound, 06 §4.8).
const syncReadTimeout = 10 * time.Minute

// syncRDBSource is the production RDBSource: it dials the shard primary directly
// (not via the high-level valkey-go client, which would try to parse the bulk
// RDB as a command reply), AUTHs as _operator, optionally SELECTs no DB, and
// issues SYNC, then exposes the inline RDB bulk payload as an io.ReadCloser.
type syncRDBSource struct {
	addr      string
	auth      authCreds
	tlsConfig *tls.Config
	// dial is overridable in tests; defaults to net/tls dialing.
	dial func(ctx context.Context) (net.Conn, error)
}

// authCreds carries the AUTH username/password the SYNC connection sends. An
// empty username uses single-arg AUTH (password only); both empty skips AUTH.
type authCreds struct {
	username string
	password string
}

// newSyncRDBSource builds a syncRDBSource for a primary at addr.
func newSyncRDBSource(addr string, auth authCreds, tlsConfig *tls.Config) *syncRDBSource {
	return &syncRDBSource{addr: addr, auth: auth, tlsConfig: tlsConfig}
}

// Open performs the replication handshake and returns the RDB stream. It dials,
// AUTHs (if credentials are set), then issues SYNC and reads the "$<len>\r\n"
// bulk header; the returned ReadCloser yields exactly len bytes of RDB and closes
// the connection on Close.
func (s *syncRDBSource) Open(ctx context.Context) (io.ReadCloser, int64, error) {
	conn, err := s.dialConn(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("dial %s: %w", s.addr, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(syncReadTimeout))
	}

	br := bufio.NewReader(conn)
	if err = s.authenticate(conn, br); err != nil {
		_ = conn.Close()
		return nil, 0, err
	}

	if _, err = conn.Write(encodeCommand("SYNC")); err != nil {
		_ = conn.Close()
		return nil, 0, fmt.Errorf("write SYNC: %w", err)
	}
	length, err := readBulkHeader(br)
	if err != nil {
		_ = conn.Close()
		return nil, 0, fmt.Errorf("read RDB bulk header: %w", err)
	}
	return &rdbStream{conn: conn, r: io.LimitReader(br, length), n: length}, length, nil
}

// dialConn establishes the raw (optionally TLS) connection, honouring the
// overridable dial hook used by tests.
func (s *syncRDBSource) dialConn(ctx context.Context) (net.Conn, error) {
	if s.dial != nil {
		return s.dial(ctx)
	}
	d := &net.Dialer{}
	if s.tlsConfig != nil {
		td := &tls.Dialer{NetDialer: d, Config: s.tlsConfig}
		return td.DialContext(ctx, "tcp", s.addr)
	}
	return d.DialContext(ctx, "tcp", s.addr)
}

// authenticate sends AUTH when credentials are present and verifies the +OK
// reply, so a NOAUTH/WRONGPASS surfaces before SYNC rather than as a confusing
// RDB-parse failure.
func (s *syncRDBSource) authenticate(conn net.Conn, br *bufio.Reader) error {
	if s.auth.password == "" {
		return nil
	}
	var cmd []byte
	if s.auth.username != "" {
		cmd = encodeCommand("AUTH", s.auth.username, s.auth.password)
	} else {
		cmd = encodeCommand("AUTH", s.auth.password)
	}
	if _, err := conn.Write(cmd); err != nil {
		return fmt.Errorf("write AUTH: %w", err)
	}
	line, err := readLine(br)
	if err != nil {
		return fmt.Errorf("read AUTH reply: %w", err)
	}
	if len(line) == 0 || line[0] != '+' {
		return fmt.Errorf("AUTH rejected: %s", line)
	}
	return nil
}

// rdbStream is the io.ReadCloser over the inline RDB bulk payload. It enforces
// the declared length via a LimitReader and closes the underlying connection on
// Close so the replica link is torn down after the dump is read.
type rdbStream struct {
	conn net.Conn
	r    io.Reader
	n    int64
}

func (s *rdbStream) Read(p []byte) (int, error) { return s.r.Read(p) }

func (s *rdbStream) Close() error { return s.conn.Close() }

// encodeCommand RESP-encodes a command as an array of bulk strings, the inline
// request form every Valkey server accepts.
func encodeCommand(args ...string) []byte {
	var b []byte
	b = append(b, '*')
	b = append(b, strconv.Itoa(len(args))...)
	b = append(b, '\r', '\n')
	for _, a := range args {
		b = append(b, '$')
		b = append(b, strconv.Itoa(len(a))...)
		b = append(b, '\r', '\n')
		b = append(b, a...)
		b = append(b, '\r', '\n')
	}
	return b
}

// readLine reads a single CRLF-terminated protocol line (without the CRLF).
func readLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	// Trim trailing CRLF / LF.
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line, nil
}

// readBulkHeader reads the "$<length>\r\n" RDB bulk header SYNC emits. Inline
// (non-diskless) SYNC always sends a finite length; the "$EOF:" diskless marker
// is rejected here as unsupported in v1alpha1 (the engine config the operator
// renders uses disk-based replication backlog).
func readBulkHeader(br *bufio.Reader) (int64, error) {
	line, err := readLine(br)
	if err != nil {
		return 0, err
	}
	if len(line) == 0 || line[0] != '$' {
		return 0, fmt.Errorf("unexpected SYNC reply %q (want $<len>)", line)
	}
	lenStr := line[1:]
	if len(lenStr) > 0 && lenStr[0] == 'E' { // "$EOF:..." diskless form
		return 0, fmt.Errorf("diskless SYNC ($EOF) not supported in v1alpha1")
	}
	length, err := strconv.ParseInt(lenStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse RDB length %q: %w", lenStr, err)
	}
	if length < 0 {
		return 0, fmt.Errorf("negative RDB length %d", length)
	}
	return length, nil
}
