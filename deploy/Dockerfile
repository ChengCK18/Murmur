# One image per Murmur command (guide step 2.9):
#   docker build -f deploy/Dockerfile --build-arg CMD=ingester -t murmur/ingester .
FROM golang:1.25.14-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG CMD
RUN test -n "$CMD" && CGO_ENABLED=0 GOTOOLCHAIN=local \
    go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

FROM alpine:3.24
RUN adduser -D -H -u 10001 murmur
USER murmur
COPY --from=build /out/app /usr/local/bin/app
ENTRYPOINT ["/usr/local/bin/app"]
