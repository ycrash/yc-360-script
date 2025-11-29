CWD := $(shell pwd)

.PHONY: _

_:
	echo "default"

alpine:
	docker build -f Dockerfile.base.alpine -t yc-360-script-base:alpine .

base: alpine
	docker rm -f yc-360-script-alpine || true
	docker run --init -d -ti --rm \
	--name yc-360-script-alpine \
	-v $(CWD):/opt/workspace/yc-360-script \
	yc-360-script-base:alpine

shell:
	docker exec -it yc-360-script-alpine /bin/sh

build:
	docker exec -it yc-360-script-alpine /bin/sh -c "cd cmd/yc && go build -o yc -ldflags='-s -w' -buildvcs=false && mkdir -p ../../bin/ && mv yc ../../bin/"

# Integration tests (requires Docker)
.PHONY: test-integration
test-integration:
	@echo "Setting up test fixtures..."
	./test/scripts/setup-fixtures.sh
	@echo "Starting test environment..."
	docker compose -f docker-compose.test.yml up -d --build
	@echo "Waiting for services to be healthy..."
	sleep 10
	@echo "Running integration tests..."
	docker compose -f docker-compose.test.yml exec -T yc-test-runner \
		go test -v -tags=integration ./test/integration/... -timeout=5m
	@echo "Stopping test environment..."
	docker compose -f docker-compose.test.yml down -v

.PHONY: test-integration-local
test-integration-local:
	@echo "Running integration tests locally (requires BuggyApp running)..."
	go test -v -tags=integration ./test/integration/... -timeout=5m

.PHONY: test
test:
	go test -v -race -coverprofile=coverage.out -covermode=atomic ./...

.PHONY: test-all
test-all: test test-integration
