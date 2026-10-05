FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# "public" builds the open core. "private" needs the private repository checked out at private/
ARG edition=public
RUN if [ "$edition" = private ]; then \
      CGO_ENABLED=0 go build -tags private -o /ariwebhooks ./private/webhooks/cmd/ariwebhooks; \
    else \
      CGO_ENABLED=0 go build -o /ariwebhooks ./cmd/ariwebhooks; \
    fi

FROM alpine:3.21
RUN apk add --no-cache git ca-certificates
COPY --from=build /ariwebhooks /usr/local/bin/ariwebhooks
# the Go runtime never learns the container's memory limit on its own; without
# this it grows the heap until the kernel OOM-kills the box. Keep it well under
# the host limit so git subprocesses (the real memory hogs) have headroom.
ENV GOMEMLIMIT=256MiB
USER nobody
ENTRYPOINT ["ariwebhooks"]
