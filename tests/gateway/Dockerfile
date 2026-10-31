ARG BIFROST_IMAGE=bifrost-web-providers:review
FROM golang:1.27.3-alpine3.24 AS build
WORKDIR /test
COPY tests/gateway/main.go .
RUN go build -o /gateway main.go
FROM ${BIFROST_IMAGE}
COPY --from=build /gateway /gateway
ENTRYPOINT ["/gateway"]
