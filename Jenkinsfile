pipeline {
  agent any

  options {
    skipDefaultCheckout(true)
  }

  environment {
    HOST_APP_PORT = '38081'
    HOST_METRICS_PORT = '32113'
    HOST_LOG_ROOT = '/opt/monitoring/fluent-bit/logs'
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
          docker rm -f go-worker-demo || true
          docker run -d \
            --name go-worker-demo \
            --restart unless-stopped \
            --add-host=host.docker.internal:host-gateway \
            -e SERVICE_NAME=worker \
            -e TARGET_SERVICE_NAME=gateway \
            -e TARGET_DISCOVERY_SERVICE_NAME=gateway-http \
            -e APP_PORT=18081 \
            -e METRICS_PORT=12113 \
            -e CONSUL_HTTP_ADDR=http://host.docker.internal:8500 \
            -e APP_LOG_PATH=/app/logs/worker/go-worker-demo.log \
            -p ${HOST_APP_PORT}:18081 \
            -p ${HOST_METRICS_PORT}:12113 \
            -v ${HOST_LOG_ROOT}:/app/logs \
            go-worker-demo:latest
        '''
      }
    }

    stage('Smoke Test') {
      steps {
        sh '''
          sleep 5
          curl -fsS http://127.0.0.1:${HOST_APP_PORT}/healthz
          curl -fsS http://127.0.0.1:${HOST_APP_PORT}/gateways
          curl -fsS http://127.0.0.1:${HOST_METRICS_PORT}/metrics | grep '^go_worker_process_up'
          curl -fsS http://127.0.0.1:${HOST_METRICS_PORT}/metrics | grep '^go_worker_queue_depth'
        '''
      }
    }
  }
}
