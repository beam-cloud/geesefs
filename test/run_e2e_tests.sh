#!/bin/bash

set -e

echo "Starting LocalStack..."
docker-compose -f test/docker-compose.test.yml up -d

echo "Waiting for LocalStack to be ready..."
for i in {1..30}; do
    if curl -s http://localhost:4566/_localstack/health | grep -q '"s3": "available"'; then
        echo "LocalStack is ready!"
        break
    fi
    echo "Waiting... ($i/30)"
    sleep 2
done

echo "Running E2E tests..."
cd test
LOCALSTACK_ENABLED=true go test -v -timeout 30m ./...

echo "Cleaning up..."
docker-compose -f docker-compose.test.yml down

echo "Tests completed!"
