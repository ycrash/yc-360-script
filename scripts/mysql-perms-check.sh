#!/bin/sh
# Runs the perms profile's test beside its server, where the agent can be refused
# the error log rather than not find it.
#
#   docker compose -f compose.mysql.yaml --profile perms up -d --wait my84-perms
#   scripts/mysql-perms-check.sh
#
# my84-perms writes /var/lib/mysql/error.log 0640, owned by the server's uid. From
# the host that path does not exist. Here this package's tests are built for Linux
# and run in a container that shares my84-perms's network and mounts its data
# directory read-only, as uid 1000, which is not the server's: the open is
# refused, as it is for an agent on a real database host. Twice: sharing the
# server's process table, which is how the same-host verdict can say yes on a
# machine whose servers all run in a VM, and with its own, where it must say the
# container hides the answer.
set -eu

cd "$(dirname "$0")/.."

server=yc-mysql-matrix-my84-perms-1
image=mysql:8.4

case "$(docker info --format '{{.Architecture}}')" in
aarch64 | arm64) goarch=arm64 ;;
x86_64 | amd64) goarch=amd64 ;;
*)
	echo "unknown Docker architecture" >&2
	exit 1
	;;
esac

build=$(mktemp -d)
trap 'rm -rf "$build"' EXIT

CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
	go test -c -tags mysqlintegration -o "$build/mysql.test" ./internal/capture/mysql/

for mode in shared-pid own-pid; do
	pid=""
	if [ "$mode" = shared-pid ]; then
		pid="--pid=container:$server"
	fi

	echo "== $mode"

	# $pid is empty or one word, so it is left unquoted on purpose.
	# shellcheck disable=SC2086
	docker run --rm $pid \
		--network "container:$server" \
		--volumes-from "$server:ro" \
		--user 1000:1000 \
		--env "YC_MY_PERMS_CONTAINER=$mode" \
		--volume "$build/mysql.test:/yc/mysql.test:ro" \
		--entrypoint /yc/mysql.test \
		"$image" -test.run '^TestPermsBesideTheServer$' -test.count=1 -test.v
done
