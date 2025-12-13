CWD := $(shell pwd)

IMAGE_NAME := yc-360-script-base:alpine
CONTAINER_NAME := yc-360-script-base-alpine

.PHONY: _ alpine base shell build clean

_:
	echo "default"

alpine:
	docker build -f Dockerfile.base.alpine -t $(IMAGE_NAME) .

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

clean:
	docker rm -f $(CONTAINER_NAME) || true