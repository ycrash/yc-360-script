# Building the yCrash Agent on Linux and macOS

This guide outlines the steps to build the yCrash agent in both Linux and macOS environments. You have two options for building the agent:

1) Build via Dockerized Environment (Recommended)  
2) Build on Bare Metal
---
## 1. Build via Dockerized Environment (Recommended)

This method uses an Alpine-based Docker image to set up a clean Go development environment with all required dependencies.

#### Step 1: Create a Dockerfile

Create a `Dockerfile.base.alpine` and add the following content:

```dockerfile
# Build stage
FROM golang:1.23.6-alpine AS builder

RUN apk add --no-cache \
    clang \
    curl \
    git \
    wget \
    vim \
    gcc \
    make \
    musl musl-dev \
    ncurses ncurses-dev ncurses-libs ncurses-static

ENV PATH=${PATH}:/usr/local/go/bin

WORKDIR /opt/workspace/yc-agent

ENTRYPOINT ["/bin/sh"]
```
#### Step 2: Create a Makefile
To simplify the build process, create a `Makefile` and add the following content:

```makefile
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
```
#### Step 3: Build the Agent
To build the agent using the containerized environment, run the following command:
```
sudo make alpine base build
```
After successful execution, the yc binary will be created in the `../yc-data-script/bin/` directory.


## 2. Build on Bare Metal

If you prefer building the agent directly on your local system, ensure the following packages are installed.

**Required Packages:**
- go
- clang
- git
- gcc
- musl
- musl-dev
- ncurses
- ncurses-dev
- ncurses-libs
- ncurses-static

> 📌 **Note**: Package names may vary slightly depending on your Linux distribution or macOS package manager (e.g., Homebrew).

### Build Steps:
#### Step 1:  Navigate to the cmd/yc directory inside the repository:
```
cd ../yc-data-script/cmd/yc
```
#### Step 2: Then run the following command to build the agent:
```
go build -o yc -ldflags='-s -w' -buildvcs=false && mkdir -p ../../bin/ && mv yc ../../bin/
```
Once the build is completed, the yc binary will be available in the `../yc-data-script/bin/` directory.
