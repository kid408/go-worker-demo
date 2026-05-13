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
          nomad job run -var-file=nomad/worker.vars.hcl nomad/worker.nomad.hcl
        '''
      }
    }

    stage('Smoke Test') {
      steps {
        sh '''
          export NOMAD_ADDR=${NOMAD_ADDR}
          sleep 10
          nomad job status worker
          curl -fsS ${CONSUL_ADDR}/v1/health/service/worker-http?passing=true | jq 'length > 0' | grep true
          curl -fsS ${CONSUL_ADDR}/v1/health/service/worker-prom?passing=true | jq 'length > 0' | grep true
        '''
      }
    }
  }
}
