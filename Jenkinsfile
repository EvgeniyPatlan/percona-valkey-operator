// Jenkinsfile — e2e on GKE. PLACEHOLDER (M0).
//
// GitHub Actions runs unit + lint + check-generate + scan + buildx only; it
// NEVER spins up a cluster (docs/architecture/10-distribution-release.md §7,
// 11-testing-qa.md §6). The real GKE e2e pipeline (createCluster /
// shutdownCluster / parallel CSV-driven kuttl runs / S3 artifact upload) is
// wired in M8. The stages below are intentionally commented out so this file
// resolves the layout without provisioning anything.
//
// pipeline {
//   agent { label 'docker' }
//   environment {
//     GCP_PROJECT_ID  = credentials('gcp-project-id')
//     CLUSTER_NAME    = "valkey-${env.BUILD_NUMBER}"
//   }
//   stages {
//     stage('Build & push operator image') { steps { sh 'make build PUSH=true VERSION=...' } }
//     stage('Create GKE cluster')          { steps { sh './e2e-tests/scripts/createCluster.sh' } }
//     stage('Run e2e (CSV matrix)')        { steps { sh 'make e2e-test' } }
//   }
//   post {
//     always { sh './e2e-tests/scripts/shutdownCluster.sh' }
//   }
// }

echo 'Jenkinsfile placeholder — e2e pipeline lands in M8.'
