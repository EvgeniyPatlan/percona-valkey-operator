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

package backup

import (
	"fmt"
	"strings"
)

// Destination prefixes disambiguate the backend in a backup's status.destination
// string, exactly mirroring the PXC BackupDestination StorageTypePrefix
// convention (06 §3.2, §8.1). A destination is "<prefix><bucket-or-container>/<key>".
const (
	// SchemeS3 prefixes S3-compatible destinations: "s3://bucket/prefix/<backup>".
	SchemeS3 = "s3://"
	// SchemeGCS prefixes Google Cloud Storage destinations: "gs://bucket/...".
	SchemeGCS = "gs://"
	// SchemeAzure prefixes Azure Blob destinations: "azure://container/...".
	SchemeAzure = "azure://"
	// SchemePVC prefixes filesystem/PVC (test-only) destinations: "pvc/<path>".
	SchemePVC = "pvc/"
)

// Destination is a parsed backend-prefixed destination root. It is the structured
// form of PerconaValkeyBackup.status.destination; restore re-parses that string to
// pick the backend and locate the set root.
type Destination struct {
	// Scheme is one of the Scheme* constants (e.g. "s3://").
	Scheme string
	// Bucket is the bucket (S3/GCS) or container (Azure) name. Empty for pvc/.
	Bucket string
	// Path is the in-bucket prefix/path of the backup-set root (no leading slash).
	Path string
}

// String formats a Destination back to its prefixed form. For object stores it is
// "<scheme><bucket>/<path>"; for pvc/ it is "pvc/<path>".
func (d Destination) String() string {
	if d.Scheme == SchemePVC {
		return SchemePVC + d.Path
	}
	if d.Path == "" {
		return d.Scheme + d.Bucket
	}
	return d.Scheme + d.Bucket + "/" + d.Path
}

// ParseDestination parses a backend-prefixed destination string into a
// Destination. It recognises the four Scheme* prefixes and splits the remainder
// into bucket/container + path. It returns an error on an unknown/absent scheme so
// a malformed status.destination fails loudly rather than silently mis-routing
// (the S3 endpointUrl region trap, 06 §8.2, motivates explicit parsing).
func ParseDestination(dest string) (Destination, error) {
	switch {
	case strings.HasPrefix(dest, SchemeS3):
		return parseBucketDest(SchemeS3, dest)
	case strings.HasPrefix(dest, SchemeGCS):
		return parseBucketDest(SchemeGCS, dest)
	case strings.HasPrefix(dest, SchemeAzure):
		return parseBucketDest(SchemeAzure, dest)
	case strings.HasPrefix(dest, SchemePVC):
		path := strings.TrimPrefix(dest, SchemePVC)
		return Destination{Scheme: SchemePVC, Path: strings.Trim(path, "/")}, nil
	default:
		return Destination{}, fmt.Errorf("unrecognised backup destination %q (want one of s3:// gs:// azure:// pvc/)", dest)
	}
}

// parseBucketDest splits a bucket-style destination ("<scheme>bucket/path...")
// into its bucket and path components.
func parseBucketDest(scheme, dest string) (Destination, error) {
	rest := strings.TrimPrefix(dest, scheme)
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return Destination{}, fmt.Errorf("backup destination %q has no bucket/container", dest)
	}
	bucket, path, _ := strings.Cut(rest, "/")
	if bucket == "" {
		return Destination{}, fmt.Errorf("backup destination %q has an empty bucket/container", dest)
	}
	return Destination{Scheme: scheme, Bucket: bucket, Path: strings.Trim(path, "/")}, nil
}

// FormatDestination builds the destination root string for a backup-set of a
// cluster on a given backend. bucketOrContainer is the bucket (S3/GCS) /container
// (Azure) /base path (pvc), and prefix is the backend's configured in-bucket
// prefix. The backup-set occupies "<cluster>/<backup>" under that prefix
// (keys.go). This is the inverse of ParseDestination and is what the controller
// records into status.destination (06 §3.2).
func FormatDestination(scheme, bucketOrContainer, prefix, cluster, backup string) string {
	setPath := joinKey(prefix, SetPrefix(cluster, backup))
	d := Destination{Scheme: scheme, Bucket: bucketOrContainer, Path: setPath}
	if scheme == SchemePVC {
		// pvc/ destinations fold the bucket/base-path into Path.
		d.Bucket = ""
		d.Path = joinKey(bucketOrContainer, setPath)
	}
	return d.String()
}
