CWD := $(shell pwd)

.PHONY: _

_:
	echo "default"

alpine:
	docker build -f Dockerfile.base.alpine -t yc-agent-base:alpine .

base: alpine
	docker rm -f yc-agent-alpine || true
	docker run --init -d -ti --rm \
	--name yc-agent-alpine \
	-v $(CWD):/opt/workspace/yc-agent \
	yc-agent-base:alpine

shell:
	docker exec -it yc-agent-alpine /bin/sh

build:
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && go build -o yc -ldflags='-s -w' -buildvcs=false && mkdir -p ../../bin/ && mv yc ../../bin/"
