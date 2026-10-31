# Build host + plugin in one Go workspace so every shared module (including
# future provider dependencies) has the same version and package hash.
FROM alpine:3.24 AS source
RUN apk add --no-cache git
ARG BIFROST_COMMIT=8b4fce4f1709d66f9208d02f50552da522535f9e
RUN git init /upstream && cd /upstream && \
    git remote add origin https://github.com/maximhq/bifrost.git && \
    git fetch --depth 1 origin ${BIFROST_COMMIT} && git checkout --detach FETCH_HEAD

FROM node:25-alpine3.23 AS ui
WORKDIR /ui
COPY --from=source /upstream/ui/package*.json ./
RUN npm ci
COPY --from=source /upstream/ui/ ./
RUN npm run build-enterprise

FROM golang:1.27.0-alpine3.24 AS build
RUN apk add --no-cache gcc musl-dev sqlite-dev binutils binutils-gold
WORKDIR /src
COPY --from=source /upstream/transports /src/transports
COPY --from=ui /ui/out /src/transports/bifrost-http/ui
COPY . /src/web-providers
ENV CGO_ENABLED=1 GOOS=linux
RUN --mount=type=cache,target=/go/pkg/mod \
    go work init ./transports ./web-providers && go work sync
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    cd /src/transports && go build -trimpath -tags sqlite_static \
    -ldflags='-w -s -X main.Version=v2.2.6-web-providers' -o /out/bifrost ./bifrost-http
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    cd /src/web-providers && go build -trimpath -tags sqlite_static \
    -buildmode=plugin -o /out/web-providers.so ./plugin

FROM alpine:3.24
RUN apk add --no-cache ca-certificates libgcc zlib && \
    adduser -D -u 1000 appuser && mkdir -p /app/data /plugins && chown -R 1000:0 /app/data && chmod g+rwX /app/data
WORKDIR /app
COPY --from=build /out/bifrost /app/main
COPY --from=build /out/web-providers.so /plugins/web-providers.so
COPY --from=source /upstream/transports/docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh
ENV APP_DIR=/app/data APP_PORT=8080 APP_HOST=0.0.0.0 LOG_LEVEL=info LOG_STYLE=json
USER 1000:0
VOLUME ["/app/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD wget -q -O /dev/null http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["/app/docker-entrypoint.sh"]
CMD ["/app/main"]
