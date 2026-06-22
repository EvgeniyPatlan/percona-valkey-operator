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
	"io"
	"net"
	"strings"
	"testing"
)

func TestEncodeCommand(t *testing.T) {
	got := string(encodeCommand("AUTH", "_operator", "pw"))
	want := "*3\r\n$4\r\nAUTH\r\n$9\r\n_operator\r\n$2\r\npw\r\n"
	if got != want {
		t.Fatalf("encodeCommand = %q, want %q", got, want)
	}
	if string(encodeCommand("SYNC")) != "*1\r\n$4\r\nSYNC\r\n" {
		t.Fatalf("encodeCommand(SYNC) = %q", encodeCommand("SYNC"))
	}
}

func TestReadBulkHeader(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"$1024\r\n", 1024, false},
		{"$0\r\n", 0, false},
		{"$EOF:40bytesofdelimiter\r\n", 0, true},
		{"+OK\r\n", 0, true},
		{"$-1\r\n", 0, true},
		{"$notanumber\r\n", 0, true},
	}
	for _, tc := range cases {
		br := bufio.NewReader(strings.NewReader(tc.in))
		got, err := readBulkHeader(br)
		if (err != nil) != tc.wantErr {
			t.Errorf("readBulkHeader(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("readBulkHeader(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestReadLine(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("+OK\r\nnext"))
	line, err := readLine(br)
	if err != nil || line != "+OK" {
		t.Fatalf("readLine = %q,%v want '+OK',nil", line, err)
	}
}

// fakeValkeyServer drives one side of a net.Pipe as a minimal Valkey replica
// endpoint: it (optionally) ACKs AUTH, then on SYNC streams "$<len>\r\n<rdb>".
func fakeValkeyServer(t *testing.T, srv net.Conn, expectAuth bool, rdb []byte) {
	t.Helper()
	go func() {
		defer func() { _ = srv.Close() }()
		br := bufio.NewReader(srv)
		if expectAuth {
			// Drain the AUTH command (one RESP array of 3 bulk strings).
			drainCommand(br, 3)
			if _, err := srv.Write([]byte("+OK\r\n")); err != nil {
				return
			}
		}
		// Drain the SYNC command (array of 1 bulk string).
		drainCommand(br, 1)
		if _, err := srv.Write([]byte("$" + itoa(len(rdb)) + "\r\n")); err != nil {
			return
		}
		_, _ = srv.Write(rdb)
	}()
}

// drainCommand reads and discards a RESP array command of n bulk-string args.
func drainCommand(br *bufio.Reader, n int) {
	_, _ = readLine(br) // *<n>
	for i := 0; i < n; i++ {
		_, _ = readLine(br) // $<len>
		_, _ = readLine(br) // value
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestSyncRDBSourceOpenHandshake(t *testing.T) {
	client, server := net.Pipe()
	rdb := []byte("REDIS0011\xff\x00\x00fake-rdb-bytes")
	fakeValkeyServer(t, server, true, rdb)

	src := newSyncRDBSource("10.0.0.1:6379", authCreds{username: "_operator", password: "pw"}, nil)
	src.dial = func(_ context.Context) (net.Conn, error) { return client, nil }

	stream, length, err := src.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = stream.Close() }()
	if length != int64(len(rdb)) {
		t.Fatalf("length = %d, want %d", length, len(rdb))
	}
	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != string(rdb) {
		t.Fatalf("RDB stream = %q, want %q", got, rdb)
	}
}

func TestSyncRDBSourceOpenNoAuth(t *testing.T) {
	client, server := net.Pipe()
	rdb := []byte("no-auth-rdb")
	fakeValkeyServer(t, server, false, rdb)

	src := newSyncRDBSource("10.0.0.1:6379", authCreds{}, nil)
	src.dial = func(_ context.Context) (net.Conn, error) { return client, nil }

	stream, length, err := src.Open(context.Background())
	if err != nil {
		t.Fatalf("Open(no auth): %v", err)
	}
	defer func() { _ = stream.Close() }()
	if length != int64(len(rdb)) {
		t.Fatalf("length = %d, want %d", length, len(rdb))
	}
	got, _ := io.ReadAll(stream)
	if string(got) != string(rdb) {
		t.Fatalf("RDB = %q, want %q", got, rdb)
	}
}

func TestSyncRDBSourceAuthRejected(t *testing.T) {
	client, server := net.Pipe()
	go func() {
		defer func() { _ = server.Close() }()
		br := bufio.NewReader(server)
		drainCommand(br, 3)
		_, _ = server.Write([]byte("-WRONGPASS invalid\r\n"))
	}()

	src := newSyncRDBSource("10.0.0.1:6379", authCreds{username: "_operator", password: "bad"}, nil)
	src.dial = func(_ context.Context) (net.Conn, error) { return client, nil }

	if _, _, err := src.Open(context.Background()); err == nil {
		t.Fatalf("Open(auth rejected) = nil error, want failure")
	}
}

func TestSyncRDBSourceDialError(t *testing.T) {
	src := newSyncRDBSource("10.0.0.1:6379", authCreds{}, nil)
	src.dial = func(_ context.Context) (net.Conn, error) { return nil, io.ErrUnexpectedEOF }
	if _, _, err := src.Open(context.Background()); err == nil {
		t.Fatalf("Open(dial error) = nil error, want failure")
	}
}
