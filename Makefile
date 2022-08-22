CWD := $(shell pwd)

_:
	echo "default"

alpine:
	docker build -f Dockerfile.base.alpine -t yc-agent-base:alpine .

base:
	docker run --init -d -ti --rm \
	--name yc-agent-alpine \
	-v $(CWD):/opt/workspace/yc-agent-repo \
	yc-agent-base:alpine

shell:
	docker exec -it yc-agent-alpine /bin/sh

deploy:
	docker exec -it yc-agent-alpine /bin/sh -c "cd yc && go build -o yc_deploy -ldflags='-s -w' -buildvcs=false && mkdir -p ../bin && mv yc_deploy ../bin"

build:
	docker exec -it yc-agent-alpine /bin/sh -c "cd yc && go build -o yc -gcflags='all=-N -l' -buildvcs=false && mkdir -p ../bin && mv yc ../bin"

