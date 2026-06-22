// Jenkinsfile — e2e on GKE (SCAFFOLD; provisions NOTHING by default).
//
// CI/CD SPLIT (arch 10 §7, 11 §6): GitHub Actions runs only the fast checks on
// PRs/main (unit + envtest + lint + check-generate + check-version + scan +
// no-push buildx) and NEVER spins up a Valkey cluster. The REAL e2e runs here,
// on Jenkins, against ephemeral GKE clusters — kuttl suites selected from the
// e2e-tests/run-*.csv matrices, run in parallel across cluster suffixes, with
// artefacts uploaded to S3 (percona-jenkins-artifactory-public).
//
// THIS IS A SCAFFOLD. The active pipeline below is gated behind RUN_E2E (default
// false), so a casual "Build Now" provisions nothing. The kuttl SUITE BODIES are
// M8 (arch 10 §1 non-goal); M7 only wires the harness: GKE create/teardown, the
// CSV-driven parallel runner, `make e2e-test`, and the self-destruct label that
// prevents cluster leaks (arch 10 §7.2, risk R11).
//
// Required Jenkins credentials (configure on the controller, never inline):
//   - gcp-project-id   : GCP_PROJECT_ID
//   - gcloud-key-file  : service-account JSON for `gcloud auth`
//   - docker-credentials, aws-credentials (S3 artefact upload)
//
// Parameters:
//   RUN_E2E        (bool)   gate — leave false for a no-op resolve.
//   CSV            (choice) which e2e-tests/run-*.csv matrix to execute.
//   OPERATOR_IMAGE (string) image under test (perconalab/* for dev runs).
//   CLUSTER_REGION (string) GKE region.

pipeline {
  agent { label 'docker' }

  parameters {
    booleanParam(name: 'RUN_E2E', defaultValue: false,
      description: 'Must be true to provision GKE and run e2e. Default false = no-op scaffold.')
    choice(name: 'CSV', choices: ['run-pr', 'run-minikube', 'run-distro', 'run-release'],
      description: 'Which e2e-tests/run-*.csv matrix to run (rows: test-name,valkey-major-version).')
    string(name: 'OPERATOR_IMAGE', defaultValue: 'perconalab/valkey-operator:main',
      description: 'Operator image under test. Dev runs use perconalab/*; never auto-pushes percona/*.')
    string(name: 'CLUSTER_REGION', defaultValue: 'us-central1',
      description: 'GKE region for the ephemeral cluster.')
  }

  options {
    timeout(time: 3, unit: 'HOURS')
    disableConcurrentBuilds()
  }

  environment {
    GCP_PROJECT_ID = credentials('gcp-project-id')
    // Self-destruct guard (arch 10 §7.2, R11): every cluster carries a label so
    // a sweep deletes leaks even if teardown is skipped.
    CLUSTER_NAME   = "valkey-${env.BUILD_NUMBER}"
    DELETE_AFTER   = 'delete-cluster-after-hours=6'
  }

  stages {
    stage('Guard') {
      steps {
        script {
          if (!params.RUN_E2E) {
            echo 'RUN_E2E=false — SCAFFOLD no-op. Set RUN_E2E=true to provision GKE and run e2e.'
            currentBuild.result = 'SUCCESS'
            // Stop here without failing: the remaining stages are `when`-gated.
            return
          }
        }
      }
    }

    // ---- everything below runs ONLY when RUN_E2E=true (real e2e) ----
    stage('Build & push operator image') {
      when { expression { return params.RUN_E2E } }
      steps {
        // Dev image only (perconalab/*). GA percona/* publishing is the
        // human-gated release.yml, never Jenkins.
        sh 'make build PUSH=true VERSION="${BUILD_NUMBER}" IMAGE_TAG_OWNER=perconalab'
      }
    }

    stage('Create GKE cluster') {
      when { expression { return params.RUN_E2E } }
      steps {
        withCredentials([file(credentialsId: 'gcloud-key-file', variable: 'GCLOUD_KEY')]) {
          sh '''
            gcloud auth activate-service-account --key-file="${GCLOUD_KEY}"
            gcloud container clusters create "${CLUSTER_NAME}" \
              --project "${GCP_PROJECT_ID}" \
              --region "${CLUSTER_REGION}" \
              --labels "${DELETE_AFTER}" \
              --num-nodes 3
            gcloud container clusters get-credentials "${CLUSTER_NAME}" \
              --project "${GCP_PROJECT_ID}" --region "${CLUSTER_REGION}"
          '''
        }
      }
    }

    stage('Run e2e (CSV matrix)') {
      when { expression { return params.RUN_E2E } }
      steps {
        // The parallel CSV runner reads e2e-tests/${CSV}.csv (test-name,version)
        // and fans out across cluster suffixes. `make e2e-test` prepends
        // kuttl-shfmt then runs `kubectl kuttl test --config e2e-tests/kuttl.yaml`.
        sh 'make e2e-test IMAGE="${OPERATOR_IMAGE}" CSV="${CSV}"'
      }
    }
  }

  post {
    always {
      // Teardown is best-effort; the self-destruct label is the real safety net.
      script {
        if (params.RUN_E2E) {
          sh '''
            gcloud container clusters delete "${CLUSTER_NAME}" \
              --project "${GCP_PROJECT_ID}" --region "${CLUSTER_REGION}" --quiet || true
          '''
          // archiveArtifacts / S3 upload of kuttl logs lands with the M8 suites.
        }
      }
    }
  }
}
