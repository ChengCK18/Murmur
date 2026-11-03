# Images for Murmur's commands (guide steps 2.9 and 3.5):
#   docker build -f deploy/Dockerfile --build-arg CMD=ingester -t murmur/ingester .
#   docker build -f deploy/Dockerfile --target gateway --build-arg CMD=gateway -t murmur/gateway .
FROM golang:1.25.14-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG CMD
RUN test -n "$CMD" && CGO_ENABLED=0 GOTOOLCHAIN=local \
    go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

# The web app, type-checked and built (web/dist).
FROM node:24.22.2-alpine3.24 AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-fund --no-audit
COPY web/ ./
RUN npm run build

FROM alpine:3.24 AS app
RUN adduser -D -H -u 10001 murmur
USER murmur
COPY --from=build /out/app /usr/local/bin/app
ENTRYPOINT ["/usr/local/bin/app"]

# The gateway also serves the web app (build with CMD=gateway).
FROM app AS gateway
COPY --from=web /web/dist /web
CMD ["-web", "/web"]

# Default: a plain command image.
FROM app
