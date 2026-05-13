pipeline {
  agent any

  options {
    skipDefaultCheckout(true)
  }

  environment {
    NOMAD_ADDR = 'http://127.0.0.1:4646'
    CONSUL_ADDR = 'http://127.0.0.1:8500'
    IMAGE_TAG = 'dev'
  }

  stages {
    stage('Checkout') {
      steps {
        checkout scm
      }
    }

    stage('Check Docker') {
      steps {
        sh '''
          #!/usr/bin/env bash
          set -euo pipefail
          docker version
          docker buildx version
          docker buildx ls
          nomad version
        '''
      }
    }

    stage('Preflight') {
      steps {
        sh '''
          #!/usr/bin/env bash
          set -euo pipefail
          export NOMAD_ADDR="${NOMAD_ADDR}"

          echo '=== active nomad processes ==='
          ps -ef | grep '[n]omad' || true

          echo '=== consul leader ==='
          curl -fsS "${CONSUL_ADDR}/v1/status/leader"
          echo

          echo '=== nomad leader ==='
          curl -fsS "${NOMAD_ADDR}/v1/status/leader"
          echo

          echo '=== nomad node status ==='
          nomad node status

          READY_NODE="$(nomad node status -json | jq -r 'map(select(.Status=="ready"))[0].ID // empty')"
          test -n "${READY_NODE}"

          echo "=== ready node: ${READY_NODE} ==="
          nomad node status -verbose "${READY_NODE}" | tee /tmp/nomad-worker-node.txt

          grep -Eq '^[[:space:]]*logs[[:space:]]' /tmp/nomad-worker-node.txt
        '''
      }
    }

    stage('Build Image') {
      steps {
        sh '''
          #!/usr/bin/env bash
          set -euo pipefail
          docker buildx build -t go-worker-demo:${IMAGE_TAG} . --load
        '''
      }
    }

    stage('Deploy') {
      steps {
        sh '''
          #!/usr/bin/env bash
          set -euo pipefail
          export NOMAD_ADDR="${NOMAD_ADDR}"
          docker rm -f go-worker-demo || true
          nomad job run -detach -var-file=nomad/worker.vars.hcl nomad/worker.nomad.hcl
        '''
      }
    }

    stage('Smoke Test') {
      steps {
        sh '''
          #!/usr/bin/env bash
          set -euo pipefail
          export NOMAD_ADDR="${NOMAD_ADDR}"

          diagnose() {
            echo '=== nomad node status ==='
            nomad node status || true
            echo '=== nomad job status ==='
            nomad job status -verbose worker || true
            echo '=== nomad job allocations ==='
            nomad job allocations worker || true
            echo '=== consul worker-http ==='
            curl -fsS "${CONSUL_ADDR}/v1/health/service/worker-http?passing=true" | jq . || true
            echo '=== consul worker-prom ==='
            curl -fsS "${CONSUL_ADDR}/v1/health/service/worker-prom?passing=true" | jq . || true
          }

          trap diagnose EXIT

          for _ in $(seq 1 30); do
            if curl -fsS "${CONSUL_ADDR}/v1/health/service/worker-http?passing=true" | tee /tmp/worker-http.json | jq -e 'length > 0' >/dev/null; then
              curl -fsS "${CONSUL_ADDR}/v1/health/service/worker-prom?passing=true" | tee /tmp/worker-prom.json | jq -e 'length > 0' >/dev/null
              nomad job status -verbose worker
              jq . /tmp/worker-http.json
              jq . /tmp/worker-prom.json
              trap - EXIT
              exit 0
            fi

            sleep 2
          done

          exit 1
        '''
      }
    }
  }
}
