CWD := $(shell pwd)
# BUILD_DIR := ./bin

.PHONY: _

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

deploy-cross-platform:
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go build -o yc_deploy -ldflags='-s -w' -buildvcs=false && mkdir -p ../../bin/linux/default && mv yc_deploy ../../bin/linux/default"
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go build -o yc_deploy -ldflags='-s -w' -buildvcs=false && mkdir -p ../../bin/linux/amd64 && mv yc_deploy ../../bin/linux/amd64"
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=linux GOARCH=arm64 CGO_ENABLED=1 go build -o yc_deploy -ldflags='-s -w' -buildvcs=false && mkdir -p ../../bin/linux/arm64 && mv yc_deploy ../../bin/linux/arm64"
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 go build -o yc_deploy -ldflags='-s -w' -buildvcs=false && mkdir -p ../../bin/macos && mv yc_deploy ../../bin/macos/default"
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 go build -o yc_deploy -ldflags='-s -w' -buildvcs=false && mkdir -p ../../bin/macos/amd64 && mv yc_deploy ../../bin/macos/amd64"
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 go build -o yc_deploy -ldflags='-s -w' -buildvcs=false && mkdir -p ../../bin/macos/arm64 && mv yc_deploy ../../bin/macos/arm64"
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=windows GOARCH=amd64 CGO_ENABLED=1 go build -o yc_deploy.exe -ldflags='-s -w' -buildvcs=false && mkdir -p ../../bin/windows/default && mv yc_deploy.exe ../../bin/windows/default"
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=windows GOARCH=amd64 CGO_ENABLED=1 go build -o yc_deploy.exe -ldflags='-s -w' -buildvcs=false && mkdir -p ../../bin/windows/amd64 && mv yc_deploy.exe ../../bin/windows/amd64"
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=windows GOARCH=arm64 CGO_ENABLED=1 go build -o yc_deploy.exe -ldflags='-s -w' -buildvcs=false && mkdir -p ../../bin/windows/arm64 && mv yc_deploy.exe ../../bin/windows/arm64"

build-cross-platform:
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go build -o yc -gcflags='all=-N -l' -buildvcs=false && mkdir -p ../../bin/linux && mv yc ../../bin/linux"
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go build -o yc -gcflags='all=-N -l' -buildvcs=false && mkdir -p ../../bin/linux/amd64 && mv yc ../../bin/linux/amd64"
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=linux GOARCH=arm64 CGO_ENABLED=1 go build -o yc -gcflags='all=-N -l' -buildvcs=false && mkdir -p ../../bin/linux/arm64 && mv yc ../../bin/linux/arm64"
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 go build -o yc -gcflags='all=-N -l' -buildvcs=false && mkdir -p ../../bin/macos && mv yc ../../bin/macos"
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 go build -o yc -gcflags='all=-N -l' -buildvcs=false && mkdir -p ../../bin/macos/amd64 && mv yc ../../bin/macos/amd64"
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 go build -o yc -gcflags='all=-N -l' -buildvcs=false && mkdir -p ../../bin/macos/arm64 && mv yc ../../bin/macos/arm64"
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=windows GOARCH=amd64 CGO_ENABLED=1 go build -o yc.exe -gcflags='all=-N -l' -buildvcs=false && mkdir -p ../../bin/windows && mv yc.exe ../../bin/windows"
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=windows GOARCH=amd64 CGO_ENABLED=1 go build -o yc.exe -gcflags='all=-N -l' -buildvcs=false && mkdir -p ../../bin/windows/amd64 && mv yc.exe ../../bin/windows/amd64"
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=windows GOARCH=arm64 CGO_ENABLED=1 go build -o yc.exe -gcflags='all=-N -l' -buildvcs=false && mkdir -p ../../bin/windows/arm64 && mv yc.exe ../../bin/windows/arm64"

build-linux-amd64:
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=linux GOARCH=amd64 go build -o yc -gcflags='all=-N -l' -buildvcs=false && mkdir -p ../../bin/linux && mv yc ../../bin/linux"
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=linux GOARCH=amd64 go build -o yc -gcflags='all=-N -l' -buildvcs=false && mkdir -p ../../bin/linux/amd64 && mv yc ../../bin/linux/amd64"

build-linux-arm64:
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=linux GOARCH=arm64 go build -o yc -gcflags='all=-N -l' -buildvcs=false && mkdir -p ../../bin/linux/arm64 && mv yc ../../bin/linux/arm64"

build-macos-amd64:
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=darwin GOARCH=amd64 go build -o yc -gcflags='all=-N -l' -buildvcs=false && mkdir -p ../../bin/macos && mv yc ../../bin/macos"
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=darwin GOARCH=amd64 go build -o yc -gcflags='all=-N -l' -buildvcs=false && mkdir -p ../../bin/macos/amd64 && mv yc ../../bin/macos/amd64"

build-macos-arm64:
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=darwin GOARCH=arm64 go build -o yc -gcflags='all=-N -l' -buildvcs=false && mkdir -p ../../bin/macos/arm64 && mv yc ../../bin/macos/arm64"

build-windows-amd64:
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=windows GOARCH=amd64 go build -o yc.exe -gcflags='all=-N -l' -buildvcs=false && mkdir -p ../../bin/windows && mv yc.exe ../../bin/windows"
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=windows GOARCH=amd64 go build -o yc.exe -gcflags='all=-N -l' -buildvcs=false && mkdir -p ../../bin/windows/amd64 && mv yc.exe ../../bin/windows/amd64"

build-windows-arm64:
	docker exec -it yc-agent-alpine /bin/sh -c "cd cmd/yc && GOOS=windows GOARCH=arm64 go build -o yc.exe -gcflags='all=-N -l' -buildvcs=false && mkdir -p ../../bin/windows/arm64 && mv yc.exe ../bin/windows/arm64"
