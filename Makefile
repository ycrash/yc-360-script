CWD := $(shell pwd)
GO_VERSION := $(shell grep '^go ' go.mod | awk '{print $$2}')

IMAGE_NAME := yc-360-script-base:alpine
CONTAINER_NAME := yc-360-script-base-alpine

.PHONY: _ alpine base shell build build-all clean

_:
	echo "default"

alpine:
	docker build --build-arg GO_VERSION=$(GO_VERSION) -f Dockerfile.base.alpine --target builder -t $(IMAGE_NAME) .

base: alpine
	docker rm -f $(CONTAINER_NAME) || true
	docker run --init -d -ti --rm \
		--name $(CONTAINER_NAME) \
		-v $(CWD):/opt/workspace/yc-360-script \
		$(IMAGE_NAME)

shell:
	docker exec -it $(CONTAINER_NAME) /bin/sh

build:
	docker exec -it $(CONTAINER_NAME) /bin/sh -c \
		"cd cmd/yc && \
		go build -o yc -ldflags='-s -w' -buildvcs=false && \
		mkdir -p ../../bin/ && \
		mv yc ../../bin/"

# Multi-arch build (exports ONLY yc binary)
build-all:
	rm -rf bin/linux
	mkdir -p bin/linux/amd64 bin/linux/arm64

	# linux/amd64
	docker buildx build \
		--build-arg GO_VERSION=$(GO_VERSION) \
		--platform linux/amd64 \
		-f Dockerfile.base.alpine \
		--target export \
		--output type=local,dest=bin/linux/amd64 \
		.

	# linux/arm64
	docker buildx build \
		--build-arg GO_VERSION=$(GO_VERSION) \
		--platform linux/arm64 \
		-f Dockerfile.base.alpine \
		--target export \
		--output type=local,dest=bin/linux/arm64 \
		.

clean:
	docker rm -f $(CONTAINER_NAME) || true