CWD := $(shell pwd)

_:
	echo "default"

alpine:
	docker build -f Dockerfile.base.alpine -t yc-agent-base:alpine .

base: alpine
	docker run --init -d -ti --rm \
	--name yc-agent-alpine \
	-v $(CWD):/opt/workspace/yc-agent-repo \
	yc-agent-base:alpine

shell:
	docker exec -it yc-agent-alpine /bin/sh

deploy:
	docker exec -it yc-agent-alpine /bin/sh -c "mkdir -p ./bin && go build -gcflags='all=-N -l' -buildvcs=false -o ./bin/yc_deploy ./cmd/yc"

build:
	docker exec -it yc-agent-alpine /bin/sh -c "mkdir -p ./bin && go build -gcflags='all=-N -l' -buildvcs=false -o ./bin/ ./cmd/..."

