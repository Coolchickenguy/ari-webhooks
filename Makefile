# go runs in docker, so nothing but docker is needed on the host
ariMigrationsDir ?= $(abspath ../ari-next/prisma/migrations)
testDatabaseUrl ?= postgres://ari:ari@host.docker.internal:5433/ari

goRun = docker run --rm -v "$(CURDIR)":/src -v "$(ariMigrationsDir)":/arimigrations:ro \
	-v ariw-gomod:/go/pkg/mod -v ariw-gocache:/root/.cache/go-build -w /src \
	-e TEST_DATABASE_URL=$(testDatabaseUrl) -e ARI_MIGRATIONS_DIR=/arimigrations
# git-daemon serves the test repositories. -count=1 because a cached result once hid a failure.
# the directories are named, never ./...: private/web sits beside private/webhooks, and Go would
# compile any .go file that a node_modules folder in it happens to ship
check = apk add --no-cache git git-daemon >/dev/null 2>&1; test -z "$$(gofmt -l $(2) | tee /dev/stderr)" && go vet $(1) $(patsubst %,./%/...,$(2)) && go test -count=1 $(1) $(patsubst %,./%/...,$(2))

.PHONY: test-public test-private image-public image-private

# an empty tmpfs hides private/, so this is what a checkout without it sees
test-public:
	$(goRun) --mount type=tmpfs,destination=/src/private golang:1.26-alpine sh -c '$(call check,,cmd internal)'

test-private:
	@test -d private/webhooks || { echo "private/webhooks is not checked out"; exit 1; }
	$(goRun) golang:1.26-alpine sh -c '$(call check,-tags private,cmd internal private/webhooks)'

image-public:
	docker build -t ari-webhooks:public .

image-private:
	@test -d private/webhooks || { echo "private/webhooks is not checked out"; exit 1; }
	docker build --build-arg edition=private -t ari-webhooks:private .
