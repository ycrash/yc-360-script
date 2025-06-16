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
