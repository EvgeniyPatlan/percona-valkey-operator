//go:build m4deps

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

// This file exists ONLY to pin the M4 backup/restore module dependencies in
// go.mod / go.sum so the parallel M4 legs (S3/GCS/Azure backends in GO-4.2/GO-4.3
// and the scheduled-backup cron registry in GO-4.11) can import them WITHOUT
// editing go.mod themselves. The `m4deps` build tag is never set during a normal
// build, so none of these packages are compiled into the operator or any binary;
// they are referenced here purely so `go mod tidy` retains them as direct
// requirements. As each leg adds a real import, that import takes over and this
// file's reference becomes redundant (harmless).
//
// See docs/implementation/05-phase4-backup-restore.md §4 (in-scope deps) and the
// M4 foundation contract.

package backup

import (
	// AWS SDK v2 — s3Store (GO-4.2): config/credentials loading, the S3 client,
	// and the streaming multipart upload/download manager.
	_ "github.com/aws/aws-sdk-go-v2/config"
	_ "github.com/aws/aws-sdk-go-v2/credentials"
	_ "github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	_ "github.com/aws/aws-sdk-go-v2/service/s3"

	// Azure Blob — azureStore (GO-4.3).
	_ "github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	_ "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	// Google Cloud Storage — gcsStore (GO-4.3).
	_ "cloud.google.com/go/storage"

	// robfig/cron — the in-operator scheduled-backup cron registry (GO-4.11).
	_ "github.com/robfig/cron/v3"
)
