pipeline {
  agent any

  options {
    skipDefaultCheckout(true)
  }

  environment {
    NOMAD_ADDR = 'http://127.0.0.1:4646'
    CONSUL_ADDR = 'http://127.0.0.1:8500'
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
          docker version
          docker buildx version
          docker buildx ls
          nomad version
        '''
      }
    }

    stage('Build Image') {
      steps {
        sh '''
          docker buildx build -t go-worker-demo:latest . --load
        '''
      }
    }

    stage('Deploy') {
      steps {
        sh '''
          export NOMAD_ADDR=${NOMAD_ADDR}
          docker rm -f go-worker-demo || true
          nomad job run -detach -var-file=nomad/worker.vars.hcl nomad/worker.nomad.hcl
        '''
      }
    }

    stage('Smoke Test') {
      steps {
        sh '''
          export NOMAD_ADDR=${NOMAD_ADDR}
          sleep 10
          nomad node status
          nomad job status -verbose worker
          curl -fsS ${CONSUL_ADDR}/v1/health/service/worker-http?passing=true | tee /tmp/worker-http.json | jq .
          jq -e 'length > 0' /tmp/worker-http.json >/dev/null
          curl -fsS ${CONSUL_ADDR}/v1/health/service/worker-prom?passing=true | tee /tmp/worker-prom.json | jq .
          jq -e 'length > 0' /tmp/worker-prom.json >/dev/null
        '''
      }
    }
  }
}
